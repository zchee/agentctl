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

package tty

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// openPTY opens a real pseudo-terminal pair and closes it with the test.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("opening a pty: %v", err)
	}
	t.Cleanup(func() {
		_ = master.Close()
		_ = slave.Close()
	})
	return master, slave
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		file func(t *testing.T) *os.File
		want bool
	}{
		"success: a pty slave is a terminal": {
			file: func(t *testing.T) *os.File {
				_, slave := openPTY(t)
				return slave
			},
			want: true,
		},
		"success: a regular file is not a terminal": {
			file: func(t *testing.T) *os.File {
				f, err := os.CreateTemp(t.TempDir(), "plain")
				if err != nil {
					t.Fatalf("creating a plain file: %v", err)
				}
				t.Cleanup(func() { _ = f.Close() })
				return f
			},
			want: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := IsTerminal(tt.file(t)); got != tt.want {
				t.Errorf("IsTerminal = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestSize(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rows, cols uint16
	}{
		"success: the reported size is the one the device was given": {
			rows: 20,
			cols: 84,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			master, slave := openPTY(t)
			winsize := &pty.Winsize{Rows: tt.rows, Cols: tt.cols}
			if err := pty.Setsize(master, winsize); err != nil {
				t.Fatalf("sizing the pty: %v", err)
			}

			width, height, err := Size(slave)
			if err != nil {
				t.Fatalf("Size: %v", err)
			}
			if width != int(tt.cols) || height != int(tt.rows) {
				t.Errorf("Size = %dx%d, want %dx%d", width, height, tt.cols, tt.rows)
			}
		})
	}
}

func TestWaitReadableProbesPoll(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{}{
		"success: readiness reflects the real device's poll result": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			master, slave := openPTY(t)
			if _, err := master.Write([]byte("y\n")); err != nil {
				t.Fatalf("writing to the master side: %v", err)
			}

			// Probe the device directly, so the assertion below matches
			// whatever this kernel's pty driver answers poll with.
			probe := []unix.PollFd{{Fd: int32(slave.Fd()), Events: unix.POLLIN}}
			if _, err := unix.Poll(probe, 100); err != nil {
				t.Fatalf("probing the pty with poll: %v", err)
			}
			needsSelect := probe[0].Revents&unix.POLLNVAL != 0

			var waiter Readiness
			ready, err := waiter.WaitReadable(slave, 100*time.Millisecond)
			if err != nil {
				t.Fatalf("WaitReadable: %v", err)
			}
			if !ready {
				t.Fatal("WaitReadable = false with input pending")
			}
			if waiter.useSelect != needsSelect {
				t.Errorf("useSelect = %t, want the device's own poll result %t", waiter.useSelect, needsSelect)
			}

			answer := make([]byte, 2)
			if _, err := io.ReadFull(slave, answer); err != nil {
				t.Fatalf("reading the pending input: %v", err)
			}
			if string(answer) != "y\n" {
				t.Errorf("read %q, want %q", answer, "y\n")
			}
		})
	}
}

func TestWaitReadableSelectFallback(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{}{
		"success: the select fallback reads a real pty and keeps its bound": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			master, slave := openPTY(t)
			// This is the state retained after an invalid-descriptor poll
			// answer, exercised even on systems whose ptys support poll so
			// the fallback cannot silently rot.
			waiter := Readiness{useSelect: true}

			started := time.Now()
			ready, err := waiter.WaitReadable(slave, 30*time.Millisecond)
			if err != nil {
				t.Fatalf("WaitReadable: %v", err)
			}
			if ready {
				t.Fatal("WaitReadable = true with nothing to read")
			}
			if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
				t.Errorf("an empty 30 ms wait took %v", elapsed)
			}

			if _, err := master.Write([]byte("y\n")); err != nil {
				t.Fatalf("writing to the master side: %v", err)
			}
			ready, err = waiter.WaitReadable(slave, 100*time.Millisecond)
			if err != nil {
				t.Fatalf("WaitReadable: %v", err)
			}
			if !ready {
				t.Fatal("WaitReadable = false with input pending")
			}

			answer := make([]byte, 2)
			if _, err := io.ReadFull(slave, answer); err != nil {
				t.Fatalf("reading the pending input: %v", err)
			}
			if string(answer) != "y\n" {
				t.Errorf("read %q, want %q", answer, "y\n")
			}

			ready, err = waiter.WaitReadable(slave, 0)
			if err != nil {
				t.Fatalf("WaitReadable: %v", err)
			}
			if ready {
				t.Error("WaitReadable = true on a drained pty with a zero bound")
			}
		})
	}
}
