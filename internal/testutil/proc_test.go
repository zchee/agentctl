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
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestWaitUntil(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		ready func() func() bool
		want  bool
	}{
		"success: an immediate condition returns at once": {
			ready: func() func() bool { return func() bool { return true } },
			want:  true,
		},
		"success: a condition that turns true is observed": {
			ready: func() func() bool {
				calls := 0
				return func() bool { calls++; return calls >= 3 }
			},
			want: true,
		},
		"error: a condition that never turns true times out": {
			ready: func() func() bool { return func() bool { return false } },
			want:  false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := WaitUntil(200*time.Millisecond, tt.ready()); got != tt.want {
				t.Fatalf("WaitUntil = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		text string
		want string
	}{
		"success: plain text passes through":   {text: "field=value", want: "field=value"},
		"success: a color sequence is removed": {text: "\x1b[2mfield\x1b[0m=\x1b[32mvalue\x1b[0m", want: "field=value"},
		"success: digits inside stay out":      {text: "\x1b[38;5;245mgray\x1b[0m", want: "gray"},
		// A non-CSI escape is a two-character sequence: the escape and its
		// immediate follower drop, and whatever comes after stays.
		"success: a two-character escape drops": {text: "\x1b(Btext", want: "Btext"},
		"success: empty input stays empty":      {text: "", want: ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := StripANSI(tt.text); got != tt.want {
				t.Fatalf("StripANSI(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestRunReportsExitAndBothStreams(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "echo out; echo err >&2; exit 3")
	out := Run(t, cmd)

	if out.Signaled {
		t.Fatalf("the child exited and must not report a signal")
	}
	if out.ExitCode(t) != 3 {
		t.Fatalf("exit code = %d, want 3", out.Code)
	}
	if out.Stdout != "out\n" || out.Stderr != "err\n" {
		t.Fatalf("streams = (%q, %q), want (out\\n, err\\n)", out.Stdout, out.Stderr)
	}
}

func TestFinishDrainsBothPipesWithoutDeadlock(t *testing.T) {
	t.Parallel()

	// 200,000 bytes of standard error before any standard output: a
	// harness that drained the streams one after the other would deadlock
	// against the roughly 64 KiB pipe buffer.
	script := `i=0; while [ $i -lt 2000 ]; do printf '%0100d\n' "$i" >&2; i=$((i+1)); done; echo done`
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	out := Run(t, cmd)

	if out.ExitCode(t) != 0 {
		t.Fatalf("exit code = %d, want 0", out.Code)
	}
	if out.Stdout != "done\n" {
		t.Fatalf("stdout = %q, want done\\n", out.Stdout)
	}
	if got := len(out.Stderr); got < 200_000 {
		t.Fatalf("stderr holds %d bytes, want the whole 200,000+", got)
	}
}

func TestSendSIGTERMEndsAChild(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 30")
	stdout, stderr := Capture(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}

	SendSIGTERM(t, cmd.Process.Pid)
	out := Finish(t, cmd, stdout, stderr)

	if !out.Signaled {
		t.Fatalf("the child was killed and must report a signal, got exit %d", out.Code)
	}
}

func TestOutputExitCodeInsistsOnAnExit(t *testing.T) {
	t.Parallel()

	// ExitCode fails the test on a signal death, so the failing path is
	// exercised against a throwaway recorder rather than this test itself.
	recorder := &recordingTB{TB: t}
	out := Output{Signaled: true, Code: -1}
	_ = out.ExitCode(recorder)
	if !recorder.failed {
		t.Fatalf("ExitCode accepted a signal death")
	}
	if !strings.Contains(recorder.message, "signal") {
		t.Fatalf("ExitCode's failure does not name the signal death: %q", recorder.message)
	}
}

// recordingTB captures a fatal failure instead of ending the test, so a
// helper's failing path can itself be tested.
type recordingTB struct {
	testing.TB
	failed  bool
	message string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.message = strings.TrimSpace(format)
}
