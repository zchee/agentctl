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

package desktop

import (
	"bufio"
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

// Record types in the journal.
const (
	// TypeIntent is written, and flushed, before the first rename of an
	// operation.
	TypeIntent = "desktop_intent"
	// TypeResult is written after the launch step; every field in it is an
	// observation, never a prediction.
	TypeResult = "desktop_result"
)

// Outcome is the direction a result row records.
type Outcome string

const (
	// OutcomeApplied is an operation that installed its target.
	OutcomeApplied Outcome = "applied"
	// OutcomeReverted is an operation whose exchange was put back.
	OutcomeReverted Outcome = "reverted"
	// OutcomeAborted is an operation that never moved anything, closed by
	// repair.
	OutcomeAborted Outcome = "aborted"
	// OutcomeNoop is a request for the account already in place.
	OutcomeNoop Outcome = "noop"
	// OutcomeUnknown is an operation whose end state could not be decided.
	OutcomeUnknown Outcome = "unknown"
)

// FileID is a directory's (device, inode) pair: the provenance of a
// profile, independent of its name and of what it claims to hold.
type FileID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// String renders the pair for reports.
func (f FileID) String() string { return fmt.Sprintf("%d:%d", f.Dev, f.Ino) }

// IdentityRecord is a reader state as the journal stores it.
type IdentityRecord struct {
	State ReaderState `json:"state"`
	UUID  string      `json:"uuid,omitzero"`
}

// RecordOf converts a reader answer for the journal.
func RecordOf(id Identity) IdentityRecord { return IdentityRecord{State: id.State, UUID: id.UUID} }

// Outgoing describes the profile leaving the data path.
type Outgoing struct {
	// Name is the stored name it is moved to.
	Name string `json:"name"`
	// Identity is what it read as before the move.
	Identity IdentityRecord `json:"identity"`
	// Destination is the absolute path it is moved to.
	Destination string `json:"destination"`
	// File is its (device, inode) pair before the move.
	File FileID `json:"file"`
}

// Target describes the profile entering the data path.
type Target struct {
	// Name is its stored name, or empty when no stored profile exists for
	// the account (the app creates a fresh one).
	Name string `json:"name,omitzero"`
	// Identity is what it read as, or StateFresh for a missing target.
	Identity IdentityRecord `json:"identity"`
	// Source is the absolute path it is moved from, or empty.
	Source string `json:"source,omitzero"`
	// Existed reports whether a stored profile was moved in.
	Existed bool `json:"target_existed"`
	// File is its (device, inode) pair when Existed.
	File *FileID `json:"file,omitzero"`
	// Park is where a rollback or a revert parks a directory the app
	// created at the data path during this operation, when Existed is
	// false.
	Park string `json:"park,omitzero"`
}

// Main is one main process of the app, identified by pid and start.
type Main struct {
	PID   int32  `json:"pid"`
	Start string `json:"start"`
}

// Intent is the record written before the first rename.
type Intent struct {
	Type             string    `json:"type"`
	OpID             string    `json:"op_id"`
	Time             string    `json:"time"`
	Command          string    `json:"command"`
	RequestedAccount string    `json:"requested_account"`
	Store            string    `json:"store,omitzero"`
	SwapAuditID      string    `json:"swap_audit_id,omitzero"`
	Outgoing         *Outgoing `json:"outgoing"`
	Target           Target    `json:"target"`
}

// Result is the record written after the launch step.
type Result struct {
	Type             string          `json:"type"`
	OpID             string          `json:"op_id"`
	Time             string          `json:"time"`
	Command          string          `json:"command"`
	RequestedAccount string          `json:"requested_account,omitzero"`
	SwapAuditID      string          `json:"swap_audit_id,omitzero"`
	Outcome          Outcome         `json:"outcome"`
	Word             string          `json:"word"`
	Verified         bool            `json:"verified"`
	Observed         *IdentityRecord `json:"observed"`
	Installed        *FileID         `json:"installed"`
	Main             *Main           `json:"main"`
	// AppRunning is whether a main of the app was seen when the row was
	// written; null when that observation failed, which proves nothing
	// either way.
	AppRunning *bool `json:"app_running"`
}

// envelope reads only what tells one record from another.
type envelope struct {
	Type string `json:"type"`
	OpID string `json:"op_id"`
}

// Entry is one journal record as read back.
type Entry struct {
	// Line is the 1-based line number.
	Line int
	// Intent is set for an intent record.
	Intent *Intent
	// Result is set for a result record.
	Result *Result
}

// Now formats the current instant for a record.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// appendHook lets a test make an append fail exactly as a full disk
// would, without filling one.
var appendHook func(path string) error

// Append writes one record as a single line to the journal and flushes it
// to stable storage before returning; a journal created by this call also
// has its directory entry flushed. The file is opened append-only and
// never through a symbolic link.
func Append(path string, record any) error {
	if appendHook != nil {
		if err := appendHook(path); err != nil {
			return err
		}
	}
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding a journal record: %w", err)
	}
	line = append(line, '\n')
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, fs.ErrNotExist)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("opening the journal: %w", err)
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing the journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("flushing the journal: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing the journal: %w", err)
	}
	if created {
		if err := syncDir(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("flushing %s: %w", dir, err)
	}
	return nil
}

// maxJournalLine bounds one record. A record is a few hundred bytes; a
// longer line is not one this build wrote.
const maxJournalLine = 64 << 10

// ReadJournal reads every record. An absent journal is empty. Members this
// build does not know are ignored, so a newer build's records still read;
// a line that is not a record of a known type is an error, because a
// journal that cannot be read in full cannot say which operation is open.
func ReadJournal(path string) ([]Entry, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening the journal: %w", err)
	}
	defer func() { _ = file.Close() }()
	return parseJournal(file)
}

func parseJournal(r io.Reader) ([]Entry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), maxJournalLine)
	var entries []Entry
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("journal line %d: %w", line, err)
		}
		if !ValidOpID(env.OpID) {
			return nil, fmt.Errorf("journal line %d: %q is not an operation id", line, env.OpID)
		}
		switch env.Type {
		case TypeIntent:
			var in Intent
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("journal line %d: %w", line, err)
			}
			entries = append(entries, Entry{Line: line, Intent: &in})
		case TypeResult:
			var res Result
			if err := json.Unmarshal(raw, &res); err != nil {
				return nil, fmt.Errorf("journal line %d: %w", line, err)
			}
			entries = append(entries, Entry{Line: line, Result: &res})
		default:
			return nil, fmt.Errorf("journal line %d: unknown record type %q", line, env.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the journal: %w", err)
	}
	return entries, nil
}

// OpenIntents returns every intent that no later result closes, oldest
// first.
func OpenIntents(entries []Entry) []*Intent {
	closed := make(map[string]bool)
	for _, e := range entries {
		if e.Result != nil {
			closed[e.Result.OpID] = true
		}
	}
	var open []*Intent
	for _, e := range entries {
		if e.Intent != nil && !closed[e.Intent.OpID] {
			open = append(open, e.Intent)
		}
	}
	return open
}

// LatestApplied returns the newest result with outcome applied, or nil.
func LatestApplied(entries []Entry) *Result {
	for _, entrie := range slices.Backward(entries) {
		if r := entrie.Result; r != nil && r.Outcome == OutcomeApplied {
			return r
		}
	}
	return nil
}

// IntentFor returns the intent with the given operation id, or nil.
func IntentFor(entries []Entry, opID string) *Intent {
	for _, e := range entries {
		if e.Intent != nil && e.Intent.OpID == opID {
			return e.Intent
		}
	}
	return nil
}
