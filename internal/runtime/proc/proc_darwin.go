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

//go:build darwin

package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// The kernel's run-state values for a process accounting record. The sys
// package does not export them, so they are spelled here with the values the
// kernel has used since the interface existed.
const (
	// statusIdle is a process still being created.
	statusIdle = 1
	// statusRunnable is a process that is running or ready to run.
	statusRunnable = 2
	// statusSleeping is a process waiting on an event.
	statusSleeping = 3
	// statusStopped is a suspended process: a debugger stop, a terminal
	// stop from a backgrounded read or write, or an explicit suspension
	// all land here alike.
	statusStopped = 4
	// statusZombie is an exited process whose status nobody has collected.
	statusZombie = 5
)

// maxStartSecond bounds a believable start time at the last second of the
// year 9999. A kernel value past it (or negative) cannot be a real process
// start, so it is refused rather than rendered into a plausible identity.
const maxStartSecond = 253402300799

// List returns a snapshot of every process the kernel accounts for,
// including other users' processes; entries that carry no process id are
// padding, never processes, and are dropped.
func List(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	processes := make([]Process, 0, len(records))
	for i := range records {
		if records[i].Proc.P_pid <= 0 {
			continue
		}
		processes = append(processes, fromRecord(&records[i]))
	}
	return processes, nil
}

// Lookup returns the accounting record of one process.
//
// The kernel stores at most 16 bytes of command name, so an executable with
// a longer file name arrives truncated and can never equal the full name; an
// exact-name caller observes a documented non-match, not an error.
//
// # Errors
//
// Returns an error wrapping [ErrProcessGone] when no such process exists,
// and a plain error when the kernel's answer has an unexpected shape, which
// is a reason to know nothing rather than to trust part of a record.
func Lookup(ctx context.Context, pid int) (Process, error) {
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	// Zero addresses the caller's own record in some interfaces and a
	// negative value a process group; neither names one other process, and
	// the kernel's ids fit int32.
	if pid <= 0 || pid > math.MaxInt32 {
		return Process{}, fmt.Errorf("process %d: %w", pid, ErrProcessGone)
	}
	record, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err == nil {
		return fromRecord(record), nil
	}
	// The kernel answers a missing process with an empty record rather
	// than a distinct error, and the typed read reports that as a size
	// mismatch. Reading the raw bytes tells the two apart: no bytes means
	// no such process, while a short or long record means the running
	// kernel's layout is not the one this build expects — and that answer
	// would come back for every process alike, so trusting it would turn
	// the whole process table invisible.
	raw, rawErr := unix.SysctlRaw("kern.proc.pid", pid)
	if rawErr == nil && len(raw) == 0 {
		return Process{}, fmt.Errorf("process %d: %w", pid, ErrProcessGone)
	}
	if rawErr == nil {
		return Process{}, fmt.Errorf("process %d: record has %d bytes, want %d", pid, len(raw), unix.SizeofKinfoProc)
	}
	return Process{}, fmt.Errorf("reading process %d: %w", pid, err)
}

// SameUserNamed returns every process of the calling user whose kernel
// command name is exactly name, with its run state.
//
// Ownership is the real user id compared against this process's real user
// id, so a setuid peer is attributed to whoever started it. The match is
// exact and case sensitive: a name that merely contains the wanted name is
// somebody else, and matching command lines would sweep in arguments.
func SameUserNamed(ctx context.Context, name string) ([]Process, error) {
	processes, err := List(ctx)
	if err != nil {
		return nil, err
	}
	// Real user ids fit uint32 on this platform.
	uid := uint32(os.Getuid())
	var matched []Process
	for _, p := range processes {
		if p.RealUID == uid && p.Name == name {
			matched = append(matched, p)
		}
	}
	return matched, nil
}

// WriterGone reports whether the process that recorded an identity is
// provably gone, which is the only answer that permits acting as if it were.
//
// Gone is proved two ways: the process id no longer names a live process (a
// collected exit included), or it does but the process's start identity
// differs from the recorded one, so the id has been recycled by a different
// process. Everything else — an unreadable record, an empty recorded
// identity, an unrenderable current start — fails closed to false, because
// not knowing is not evidence of absence.
func WriterGone(ctx context.Context, pid int, recorded string) bool {
	current, err := Lookup(ctx, pid)
	if err != nil {
		return errors.Is(err, ErrProcessGone)
	}
	if current.Holder == HolderDead {
		return true
	}
	if recorded == "" {
		return false
	}
	identity := current.StartIdentity()
	return identity != "" && identity != recorded
}

// fromRecord copies the fields callers may see out of one kernel record.
func fromRecord(record *unix.KinfoProc) Process {
	return Process{
		PID:          record.Proc.P_pid,
		PPID:         record.Eproc.Ppid,
		PGID:         record.Eproc.Pgid,
		Holder:       holderFromStatus(record.Proc.P_stat),
		Start:        startFrom(record.Proc.P_starttime),
		RealUID:      record.Eproc.Pcred.P_ruid,
		EffectiveUID: record.Eproc.Ucred.Uid,
		Name:         commandName(record.Proc.P_comm),
	}
}

// holderFromStatus maps the kernel's run state onto the three states
// callers care about. Stopped covers every suspension alike, and a zombie is
// dead for a holder's purposes: neither will release anything. Idle,
// runnable and sleeping are all alive, and so is any state this build does
// not know, because the record's existence already proved the process is
// there and refusing to answer would report a live holder as dead.
func holderFromStatus(status int8) Holder {
	switch status {
	case statusStopped:
		return HolderStopped
	case statusZombie:
		return HolderDead
	case statusIdle, statusRunnable, statusSleeping:
		return HolderAlive
	default:
		return HolderAlive
	}
}

// startFrom turns the kernel's start time into a typed instant, refusing a
// value that cannot be a real timestamp: the zero time is the one value
// [Process.StartIdentity] will not render.
func startFrom(tv unix.Timeval) time.Time {
	if tv.Sec < 0 || tv.Sec > maxStartSecond || tv.Usec < 0 || tv.Usec > 999999 {
		return time.Time{}
	}
	return time.Unix(tv.Sec, int64(tv.Usec)*1000).UTC()
}

// commandName reads the kernel's fixed-width command-name field up to its
// terminator.
func commandName(comm [17]byte) string {
	if i := bytes.IndexByte(comm[:], 0); i >= 0 {
		return string(comm[:i])
	}
	return string(comm[:])
}
