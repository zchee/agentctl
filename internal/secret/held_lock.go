// Copyright 2026 The agentctl Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package secret

import (
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/runtime/proc"
)

// Claude Code's lock artefacts are directories made by mkdir, and the
// kernel releases nothing when the process holding one dies. So the only
// evidence a crash leaves is a record written before the first mkdir:
// which store was being locked, which directories were made, and the
// process that made them. A record whose directories are gone is a stale
// record and nothing more; a record whose directories are still there
// and whose process is dead is a leak, and it is the only thing that
// lets stale-lock removal act on a path outside the namespace root.

// HeldLocksDirName is where the records live, under the namespace root.
const HeldLocksDirName = "held-locks"

// heldLockRecordExtension is the extension every record file carries.
const heldLockRecordExtension = ".json"

// MaxHeldLockRecordBytes is the largest record that will be parsed.
//
// A record names one store and at most three directories, so anything
// larger is not one — and a report must not be turned into an unbounded
// read by a file somebody dropped in this directory.
const MaxHeldLockRecordBytes = 4096

// recordNameAttempts is how many names the writer tries before giving up
// on a unique record file.
const recordNameAttempts = 8

// HeldLocksDir returns the directory holding one store's held-lock
// records.
func HeldLocksDir(paths *config.Paths) string {
	return filepath.Join(paths.NamespaceRoot(), HeldLocksDirName)
}

// HeldLockRecord is one held-lock record, as it is written before the
// first mkdir and as the diagnostics read it back.
//
// One type serves both sides so the reader and the writer cannot
// disagree about a field name or a tree spelling. The JSON keys are the
// store format's fixed spelling: records written by either manager
// binary sharing the store must keep reading each other.
type HeldLockRecord struct {
	// WriterPID is the process that took the locks.
	WriterPID uint32 `json:"agctl_pid"`
	// WriterStartTime is when that process started, so a recycled
	// process id cannot pass for the one that wrote the record. Nil when
	// it could not be read, and nil in a record written by a build that
	// predates the field.
	WriterStartTime *string `json:"agctl_start_time"`
	// Tree is which tree the locks are in.
	Tree Tree `json:"tree"`
	// StoreDir is the credential store directory being locked.
	StoreDir string `json:"store_dir"`
	// Paths is every directory that was created, in the order they were
	// taken.
	Paths []string `json:"paths"`
	// TakenAt is when they were taken, RFC 3339.
	TakenAt string `json:"taken_at"`
}

// WriterIsGone reports whether the process that wrote this record is
// gone, so the directories it names are a leak rather than a live hold.
//
// There are two ways to be gone, and the second is why the start time is
// recorded at all: the process id no longer names a live process (a
// collected exit included), or it does and the process's start identity
// differs from the recorded one, which means the kernel handed the id to
// somebody else. An unknown identity on either side is not evidence of
// anything: reading it as one would turn every record written by an
// older build into a permitted removal.
func (r *HeldLockRecord) WriterIsGone(ctx context.Context) bool {
	recorded := ""
	if r.WriterStartTime != nil {
		recorded = *r.WriterStartTime
	}
	return proc.WriterGone(ctx, int(r.WriterPID), recorded)
}

// Attests reports whether this record vouches for path.
//
// Exact paths, compared whole: this is the check that decides whether a
// stale-lock removal may leave the namespace root, so a prefix or a
// parent is not enough.
func (r *HeldLockRecord) Attests(path string) bool {
	cleaned := filepath.Clean(path)
	for _, held := range r.Paths {
		if filepath.Clean(held) == cleaned {
			return true
		}
	}
	return false
}

// Anchor returns where a no-follow walk to one of these paths may start,
// or false when the store directory has no parent.
//
// The store directory's parent, not the store directory: the legacy lock
// sits beside the store rather than inside it, so an anchor at the store
// itself could not reach it. Everything below the anchor — the store
// directory included — is still walked one no-follow component at a
// time.
func (r *HeldLockRecord) Anchor() (string, bool) {
	cleaned := filepath.Clean(r.StoreDir)
	parent := filepath.Dir(cleaned)
	if parent == cleaned {
		return "", false
	}
	return parent, true
}

// HeldLockFile is one record and the file it was read from.
type HeldLockFile struct {
	// File is the record file itself.
	File string
	// Record is what it said.
	Record HeldLockRecord
}

// ReadAllHeldLockRecords returns every readable record in this store,
// ordered by file name.
//
// Anything that is not a readable, parseable record is skipped rather
// than raised: a report that refuses to render because one file in a
// directory is malformed cannot diagnose the machine it was run on. The
// read itself refuses symbolic links and stops at
// [MaxHeldLockRecordBytes], and the directory is reached by a no-follow
// walk from the namespace root, because these records are the only thing
// that lets stale-lock removal act outside that root: a listing somebody
// else could redirect would be a permit somebody else could redirect. A
// directory that is simply not there reads as no records — that is every
// machine that has never held one of these locks.
func ReadAllHeldLockRecords(paths *config.Paths) []HeldLockFile {
	dir := HeldLocksDir(paths)
	fd, err := lockOpenDirUnder(paths.NamespaceRoot(), dir)
	if err != nil {
		return nil
	}
	listing := os.NewFile(uintptr(fd), dir)
	if listing == nil {
		closeLockFD(fd)
		return nil
	}
	defer func() { _ = listing.Close() }()

	names, err := listing.Readdirnames(-1)
	if err != nil {
		return nil
	}
	sort.Strings(names)

	var records []HeldLockFile
	for _, name := range names {
		if !strings.HasSuffix(name, heldLockRecordExtension) {
			continue
		}
		body, err := lockReadFileAt(int(listing.Fd()), name, MaxHeldLockRecordBytes)
		if err != nil {
			continue
		}
		var record HeldLockRecord
		if err := jsonv2.Unmarshal(body, &record); err != nil {
			continue
		}
		records = append(records, HeldLockFile{File: filepath.Join(dir, name), Record: record})
	}
	return records
}

// writeHeldLockRecord writes the record for one hold, before anything is
// taken, and returns the record file's path.
//
// The directory is reached — and created when missing — by a no-follow
// component walk from the namespace root, and the file is created
// exclusively, without following a link at its name, at 0600. A link
// planted at the directory's name is refused before anything is written;
// one left at the record's own name is refused by the open itself. The
// name is `<pid>-<monotonic ms>.json`; a taken name moves to the next
// stamp rather than disturbing what is there.
func writeHeldLockRecord(ctx context.Context, paths *config.Paths, record *HeldLockRecord, clock Clock) (string, error) {
	dir := HeldLocksDir(paths)
	dirFD, err := lockCreateDirUnder(paths.NamespaceRoot(), dir)
	if err != nil {
		return "", &UnreachableError{Path: dir, Message: err.Error()}
	}
	defer closeLockFD(dirFD)

	body, err := jsonv2.Marshal(record)
	if err != nil {
		return "", &LockIOError{Context: "could not serialize the held-lock record", Message: err.Error()}
	}

	base := uint64(max(clock.Monotonic().Milliseconds(), 0))
	for attempt := range uint64(recordNameAttempts) {
		name := fmt.Sprintf("%d-%d%s", record.WriterPID, base+attempt, heldLockRecordExtension)
		switch err := lockCreateNewFileAt(dirFD, name, body); err {
		case nil:
			return filepath.Join(dir, name), ctx.Err()
		case ErrLockExists:
			// That name is taken — by an earlier record, or by whatever
			// else is sitting there. Either way this hold needs a
			// different one, and the exclusive create disturbed nothing.
			continue
		default:
			return "", &LockIOError{Context: fmt.Sprintf("could not create `%s`", filepath.Join(dir, name)), Message: err.Error()}
		}
	}
	return "", &LockIOError{Context: fmt.Sprintf("could not name a held-lock record under `%s`", dir), Message: "every candidate name was taken"}
}

// rfc3339UTC renders a wall-clock reading for the record, in UTC with
// the sub-second digits the reading actually carries.
func rfc3339UTC(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
}
