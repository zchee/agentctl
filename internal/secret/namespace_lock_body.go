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
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

// LockBody is what an acquired lock records about its holder.
//
// Written into the lock file's body so doctor can say who holds a lock,
// and whether that process is alive, stopped or gone.
type LockBody struct {
	// PID is the holder's process id.
	PID uint32 `json:"pid"`
	// PIDStartTime is when that process started, for telling a live
	// holder from a recycled pid. It is written as null when the kernel
	// would not answer: doctor then falls back to the process id alone,
	// which is weaker but still useful, rather than the acquisition
	// failing over a diagnostic field. The string is compared, never
	// parsed, so its spelling is private to the proc package.
	PIDStartTime *string `json:"pid_start_time"`
	// AcquiredAt is when the lock was taken, RFC 3339.
	AcquiredAt string `json:"acquired_at"`
}

// lockBodyMaxLen bounds how large a file ReadBody is willing to parse.
// A holder record is well under 200 bytes; anything kilobytes long is not
// one, and parsing it would spend doctor's time on somebody else's data.
const lockBodyMaxLen = 4096

// ReadBody reads a lock file's holder record, if it has a readable one.
//
// It reports false for every failure — a missing file, a body larger than
// a record can be, or bytes that do not parse into one. An unreadable
// body means the holder is older, or newer, or crashed mid-write, and
// none of those is worth failing a doctor run over.
func ReadBody(path string) (LockBody, bool) {
	bytes, err := os.ReadFile(path)
	if err != nil || len(bytes) > lockBodyMaxLen {
		return LockBody{}, false
	}
	// Presence is checked through pointers because a record without its
	// required members is not a record: a zero-valued pid must not be
	// invented for a body that never named one.
	var probe struct {
		PID          *uint32 `json:"pid"`
		PIDStartTime *string `json:"pid_start_time"`
		AcquiredAt   *string `json:"acquired_at"`
	}
	if err := json.Unmarshal(bytes, &probe); err != nil || probe.PID == nil || probe.AcquiredAt == nil {
		return LockBody{}, false
	}
	return LockBody{PID: *probe.PID, PIDStartTime: probe.PIDStartTime, AcquiredAt: *probe.AcquiredAt}, true
}

// writeBody replaces the lock file's body with a description of this
// holder: truncate, rewind, then one write of the record. No rename and
// no fsync — the flock, not the body, is what excludes other writers, and
// a body lost to a crash reads as absent, which doctor already handles.
func writeBody(ctx context.Context, guard *LockGuard) error {
	body := LockBody{PID: uint32(os.Getpid()), AcquiredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if rec, err := proc.Lookup(ctx, os.Getpid()); err == nil {
		if id := rec.StartIdentity(); id != "" {
			body.PIDStartTime = &id
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return &LockUnavailableError{Reason: fmt.Sprintf("could not serialize the lock body: %v", err), Err: err}
	}

	if err := replaceContents(guard.File(), encoded); err != nil {
		return &LockUnavailableError{Reason: fmt.Sprintf("could not write `%s`: %v", guard.Path(), err), Err: err}
	}
	return nil
}

// replaceContents truncates an open file and writes bytes from its start.
func replaceContents(file *os.File, bytes []byte) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := file.Write(bytes)
	return err
}
