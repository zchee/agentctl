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

package testutil

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// testContext returns the test's context, so every subprocess the harness
// starts dies with the test that started it.
func testContext(tb testing.TB) context.Context {
	switch t := tb.(type) {
	case *testing.T:
		return t.Context()
	case *testing.B:
		return t.Context()
	default:
		// Only tests and benchmarks run subprocesses here; anything else
		// is a harness bug to surface, not to paper over with a context
		// that outlives the caller.
		tb.Fatalf("no test context for %T", tb)
		return nil
	}
}

// WaitUntil polls ready every 20 ms until it returns true or budget
// elapses. It reports whether the condition was ever observed, so the
// caller decides what a timeout means. Polling rather than sleeping a
// fixed interval is what keeps tests fast in the common case and honest in
// the slow one.
func WaitUntil(budget time.Duration, ready func() bool) bool {
	start := time.Now()
	for {
		if ready() {
			return true
		}
		if time.Since(start) >= budget {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// StripANSI returns one block of output with its terminal escape sequences
// removed.
//
// Log formatters style field names and separators even when standard error
// is a pipe, so a test that reads field=value out of a log line has to
// strip the escapes first: the = itself is wrapped in them, and the
// sequences contain digits, so neither a literal "field=" search nor
// "skip to the first digit" survives them.
func StripANSI(text string) string {
	var out bytes.Buffer
	out.Grow(len(text))
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '\x1b' {
			out.WriteRune(runes[i])
			continue
		}
		// A control sequence introduced by [ runs to its final byte, the
		// first character in 0x40..0x7e after the introducer. Anything
		// else after the escape is a two-character sequence.
		i++
		if i < len(runes) && runes[i] == '[' {
			for i++; i < len(runes); i++ {
				if runes[i] >= '\x40' && runes[i] <= '\x7e' {
					break
				}
			}
		}
	}
	return out.String()
}

// SendSIGTERM sends SIGTERM to one process id, asking the operating system
// to do what a user's kill would.
func SendSIGTERM(tb testing.TB, pid int) {
	tb.Helper()
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		tb.Fatalf("kill -TERM %d: %v", pid, err)
	}
}

// Output is everything a finished child said.
type Output struct {
	// Code is the exit status code, or -1 when a signal ended the process.
	Code int
	// Signaled reports whether a signal ended the process instead of an
	// exit.
	Signaled bool
	// Stdout is standard output, as text.
	Stdout string
	// Stderr is standard error, as text.
	Stderr string
}

// ExitCode returns the exit code, insisting there was one. A process ended
// by a signal fails the test, because for the binary under test that means
// signal handling did not run.
func (o Output) ExitCode(tb testing.TB) int {
	tb.Helper()
	if o.Signaled {
		tb.Fatalf("the process was terminated by a signal rather than exiting; stdout:\n%s\nstderr:\n%s", o.Stdout, o.Stderr)
	}
	return o.Code
}

// Capture attaches in-memory buffers to cmd's standard output and standard
// error and returns them. The standard library drains both pipes
// concurrently, so a child that fills one before finishing the other
// cannot deadlock the test - a pipe holds about 64 KiB before it blocks
// the writer.
func Capture(cmd *exec.Cmd) (stdout, stderr *bytes.Buffer) {
	stdout, stderr = new(bytes.Buffer), new(bytes.Buffer)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return stdout, stderr
}

// Finish waits for a started cmd and returns what it said, with both pipes
// fully drained.
func Finish(tb testing.TB, cmd *exec.Cmd, stdout, stderr *bytes.Buffer) Output {
	tb.Helper()
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		tb.Fatalf("wait for the child: %v", err)
	}
	state := cmd.ProcessState
	return Output{
		Code:     state.ExitCode(),
		Signaled: !state.Exited(),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
}

// Run starts cmd with captured pipes, waits for it, and returns what it
// said.
func Run(tb testing.TB, cmd *exec.Cmd) Output {
	tb.Helper()
	stdout, stderr := Capture(cmd)
	if err := cmd.Start(); err != nil {
		tb.Fatalf("start %q: %v", cmd.Path, err)
	}
	return Finish(tb, cmd, stdout, stderr)
}
