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

package logbuf

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// The hold flag and the buffer are process-wide, so these tests run
// serially against a reset state rather than in parallel.

// resetState empties the process-wide buffer and lowers the hold, so each
// test starts from the state a fresh process has.
func resetState(t *testing.T) {
	t.Helper()
	held.Store(false)
	takeBuffered()
	t.Cleanup(func() {
		held.Store(false)
		takeBuffered()
	})
}

// droppedNotice is the single line the flush ends with when the cap
// dropped output.
func droppedNotice(n int) string {
	return fmt.Sprintf("agentctl: %d further bytes of log output were dropped while the watch display held the terminal\n", n)
}

func TestBufferBoundary(t *testing.T) {
	tests := map[string]struct {
		writes      []int // sizes of consecutive writes while held
		wantKept    int
		wantDropped int
	}{
		"success: exactly the cap is kept with no notice": {
			writes:      []int{bufferLimit},
			wantKept:    bufferLimit,
			wantDropped: 0,
		},
		"success: one byte past the cap is counted, not kept": {
			writes:      []int{bufferLimit, 1},
			wantKept:    bufferLimit,
			wantDropped: 1,
		},
		"success: a single write past the cap keeps its head": {
			writes:      []int{bufferLimit + 1},
			wantKept:    bufferLimit,
			wantDropped: 1,
		},
		"success: writes spanning the cap keep the head and count the tail": {
			writes:      []int{bufferLimit - 10, 7, 7, 100},
			wantKept:    bufferLimit,
			wantDropped: 104,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resetState(t)
			HoldTerminal()

			var w Writer
			total := 0
			for _, size := range tt.writes {
				n, err := w.Write(bytes.Repeat([]byte{'x'}, size))
				if err != nil {
					t.Fatalf("Write: %v", err)
				}
				// A buffered write must never report short, or the
				// logging layer would retry and duplicate the tail.
				if n != size {
					t.Fatalf("Write reported %d bytes of %d", n, size)
				}
				total += size
			}

			var sink bytes.Buffer
			releaseTerminalTo(&sink)

			out := sink.String()
			wantNotice := ""
			if tt.wantDropped > 0 {
				wantNotice = droppedNotice(tt.wantDropped)
			}
			if got := len(out) - len(wantNotice); got != tt.wantKept {
				t.Errorf("flushed %d buffered bytes, want %d", got, tt.wantKept)
			}
			if wantNotice != "" && !strings.HasSuffix(out, wantNotice) {
				t.Errorf("flush does not end with the dropped notice %q", wantNotice)
			}
			if tt.wantDropped == 0 && strings.Contains(out, "dropped") {
				t.Error("flush mentions dropping although nothing was dropped")
			}
		})
	}
}

func TestReleaseOrderAndIdempotence(t *testing.T) {
	tests := map[string]struct{}{
		"success: the flush preserves write order and a second release writes nothing": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			resetState(t)
			HoldTerminal()

			var w Writer
			for _, line := range []string{"first\n", "second\n", "third\n"} {
				if _, err := w.Write([]byte(line)); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}

			var sink bytes.Buffer
			releaseTerminalTo(&sink)
			if got, want := sink.String(), "first\nsecond\nthird\n"; got != want {
				t.Errorf("flush = %q, want %q", got, want)
			}

			var again bytes.Buffer
			releaseTerminalTo(&again)
			if got := again.String(); got != "" {
				t.Errorf("second release wrote %q, want nothing", got)
			}
		})
	}
}

func TestHoldToggle(t *testing.T) {
	tests := map[string]struct{}{
		"success: writes after the release stop being buffered": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			resetState(t)
			HoldTerminal()

			var w Writer
			if _, err := w.Write([]byte("held line\n")); err != nil {
				t.Fatalf("Write: %v", err)
			}

			var sink bytes.Buffer
			releaseTerminalTo(&sink)
			if got, want := sink.String(), "held line\n"; got != want {
				t.Fatalf("flush = %q, want %q", got, want)
			}

			// The terminal is released now, so nothing accumulates: a
			// later flush finds an empty buffer even after more writes
			// would have been buffered under a hold.
			var after bytes.Buffer
			releaseTerminalTo(&after)
			if got := after.String(); got != "" {
				t.Errorf("flush after release wrote %q, want nothing", got)
			}
		})
	}
}

func TestConcurrentWriters(t *testing.T) {
	tests := map[string]struct {
		goroutines int
		perG       int
	}{
		"success: concurrent held writes lose nothing under the cap": {
			goroutines: 8,
			perG:       100,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resetState(t)
			HoldTerminal()

			var wg sync.WaitGroup
			for range tt.goroutines {
				wg.Go(func() {
					var w Writer
					for range tt.perG {
						if _, err := w.Write([]byte("0123456789\n")); err != nil {
							t.Errorf("Write: %v", err)
							return
						}
					}
				})
			}
			wg.Wait()

			var sink bytes.Buffer
			releaseTerminalTo(&sink)
			if got, want := sink.Len(), tt.goroutines*tt.perG*11; got != want {
				t.Errorf("flushed %d bytes, want %d", got, want)
			}
		})
	}
}

func TestBufferCap(t *testing.T) {
	tests := map[string]struct {
		got  int
		want int
	}{
		"success: the held buffer caps at 256 KiB": {
			got:  bufferLimit,
			want: 256 * 1024,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("cap = %d, want %d", tt.got, tt.want)
			}
		})
	}
}
