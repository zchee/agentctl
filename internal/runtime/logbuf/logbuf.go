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

// Package logbuf decides where log output goes while the watch display
// owns the terminal.
//
// The watch command puts the terminal into raw mode on the alternate
// screen and draws a full frame several times a second. A log line written
// to standard error in the middle of that lands inside the frame: it is
// painted over by the next draw, so the user sees a flicker of text they
// cannot read and the warning is lost. Worse, a multi-line message with
// the line discipline off leaves the cursor somewhere the renderer does
// not expect and corrupts the frame until the next full redraw.
//
// So while the terminal is held, the Writer this package hands the log
// handler does not touch the terminal at all. It appends to an in-memory
// buffer, and the buffer is flushed to standard error by ReleaseTerminal —
// which the terminal restore calls after the terminal has been put back,
// so the lines land on the user's shell where they can be read and
// scrolled. Everything else — every other command, and watch before it
// enters and after it leaves — writes straight through to standard error.
//
// # The buffer is capped
//
// A watch at the most verbose log level can run for hours, and a buffer
// that grew for all of it would be a leak. Past the cap the first bytes
// are kept and later ones counted but dropped: the beginning of a failure
// is what explains it, and the flush ends with a single line saying how
// much it left out.
//
// # Pass goroutines log through this too, and the flush never waits on one
//
// The writer is a zero-size value over process-wide state, so every
// goroutine writes through the same one — which matters because most of
// the warnings during a watch come from the worker side. The buffer mutex
// is therefore held across a copy and nothing else: never across a write
// to standard error, never across a system call. That is what lets
// ReleaseTerminal run on the way out without ever blocking on a pass
// goroutine — which may still be running and may still be logging. A line
// such a goroutine writes after the release goes straight to standard
// error, which by then is where it belongs.
package logbuf

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// bufferLimit is how much output is kept while the terminal is held.
const bufferLimit = 256 * 1024

// held reports whether the terminal currently belongs to something other
// than the log.
var held atomic.Bool

// buffer holds what was written while the terminal was held, and how much
// was dropped. The mutex guards both fields and is never held across any
// write to standard error.
var buffer struct {
	mu      sync.Mutex
	bytes   []byte
	dropped int
}

// appendBuffered keeps what fits under the cap and counts what does not.
func appendBuffered(p []byte) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	room := bufferLimit - len(buffer.bytes)
	taken := min(room, len(p))
	buffer.bytes = append(buffer.bytes, p[:taken]...)
	buffer.dropped += len(p) - taken
}

// takeBuffered empties the buffer, returning what to write and how much
// was lost.
func takeBuffered() ([]byte, int) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	bytes, dropped := buffer.bytes, buffer.dropped
	buffer.bytes, buffer.dropped = nil, 0
	return bytes, dropped
}

// HoldTerminal starts holding log output back, because the terminal is
// about to be taken over by the watch display. Idempotent, and safe to
// call from any goroutine.
func HoldTerminal() {
	held.Store(true)
}

// ReleaseTerminal stops holding log output back and writes what was held
// to standard error.
//
// Call it after the terminal has been restored: the whole point is that
// these lines land on the shell rather than on the alternate screen.
//
// Idempotent. A second call finds an empty buffer and writes nothing,
// which is what makes it safe on the several restore routes — the normal
// return, the panic path and the cleanup registry can all reach it, in
// any order.
func ReleaseTerminal() {
	releaseTerminalTo(os.Stderr)
}

// releaseTerminalTo is ReleaseTerminal writing into sink instead of
// standard error: the seam the tests use, because the process cannot
// capture its own standard error. Output errors are swallowed — this runs
// on the way out, and there is nowhere left to report a failing stderr.
func releaseTerminalTo(sink io.Writer) {
	held.Store(false)
	bytes, dropped := takeBuffered()

	if len(bytes) != 0 {
		_, _ = sink.Write(bytes)
	}
	if dropped > 0 {
		_, _ = fmt.Fprintf(sink, "agentctl: %d further bytes of log output were dropped while the watch display held the terminal\n", dropped)
	}
}

// Writer is what the log handler is given: standard error, unless the
// terminal is held.
//
// A zero-size value rather than a handle, because what it writes to is a
// process-wide fact — which terminal is in use — and not something a
// caller chooses per handler.
type Writer struct{}

// Write implements io.Writer.
func (Writer) Write(p []byte) (int, error) {
	if held.Load() {
		appendBuffered(p)
		// Buffering is not a short write: reporting fewer bytes would
		// make the logging layer retry the tail and duplicate it.
		return len(p), nil
	}
	return os.Stderr.Write(p)
}
