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

// Package proc asks the operating system about processes other than this one.
//
// Lock diagnostics need three facts about a recorded holder: whether it still
// exists, whether it is stopped (a stopped holder never releases anything, so
// waiting on it is waiting forever), and when it started (a recycled process
// id with a different start time is a different process, so a stale record
// must never be mistaken for a live claim).
//
// Everything is read in-process from the kernel's process table. Running a
// process lister as a child is not acceptable: on Darwin that binary is
// setuid, a sandboxed caller may not execute it at all, and the failure shape
// would be process ids with no states, which reads as "nothing is stopped"
// and licenses breaking a lock a live session still holds. No call in this
// package can observe another process's arguments or environment; only the
// kernel's per-process accounting record is read.
package proc

import (
	"errors"
	"time"
)

// Holder is what a recorded process is doing now.
type Holder int

const (
	// HolderAlive is a process that exists and can make progress.
	HolderAlive Holder = iota
	// HolderStopped is a process that exists but is suspended, so it will
	// not release anything it holds until something resumes it.
	HolderStopped
	// HolderDead is a process that is gone, or an exit nobody has collected.
	HolderDead
)

// Label returns the word diagnostics print for this state.
func (h Holder) Label() string {
	switch h {
	case HolderStopped:
		return "stopped"
	case HolderDead:
		return "dead"
	default:
		return "alive"
	}
}

// Process is one kernel snapshot of another process's accounting record.
type Process struct {
	// PID is the process id the record describes.
	PID int32
	// PPID is the parent process id.
	PPID int32
	// PGID is the process group id.
	PGID int32
	// Holder is the run state at the time of the snapshot.
	Holder Holder
	// Start is when the process started, to microsecond resolution. It is
	// the zero value when the kernel reported a value that cannot be a real
	// timestamp; a corrupt value is refused rather than rendered, so it can
	// never masquerade as a plausible identity.
	Start time.Time
	// RealUID is the user the process really belongs to. Ownership
	// comparisons use this field, so a setuid process is attributed to the
	// user who started it rather than to the user it runs as.
	RealUID uint32
	// EffectiveUID is the user the process currently acts as.
	EffectiveUID uint32
	// Name is the command name as the kernel accounts for it. The kernel
	// stores at most 16 bytes, so a longer executable name arrives
	// truncated; see Lookup for what that means for exact-name matching.
	Name string
}

// StartIdentity renders the start time as one comparable string.
//
// The string is compared, never parsed: its only job is to differ when a
// process id has been recycled, and the microseconds make a recycling inside
// the same second detectable. An unrenderable start returns the empty string,
// which no caller may treat as a match.
func (p Process) StartIdentity() string {
	if p.Start.IsZero() {
		return ""
	}
	return p.Start.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
}

// ErrProcessGone reports that no process with the requested id exists.
var ErrProcessGone = errors.New("no such process")

// UnsupportedPlatformError reports that this platform offers no process
// observation, so callers can refuse loudly instead of acting on absent
// evidence.
type UnsupportedPlatformError struct {
	// GOOS is the platform that lacks the observation.
	GOOS string
}

// Error implements the error interface.
func (e *UnsupportedPlatformError) Error() string {
	return "process observation is not supported on " + e.GOOS
}
