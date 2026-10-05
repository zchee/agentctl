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

package signals_test

import (
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/proc"
	"github.com/zchee/agentctl/internal/runtime/signals"
)

// The tests in this file install real signal handlers and send real
// signals to the test process, so none of them run in parallel: two
// controllers installed at once would both observe one kill, and the
// process-wide cleanup registry is shared state.

// startChild starts command with args as a real child process and returns
// it; the caller owns the wait.
func startChild(t *testing.T, command string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(command, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", command, err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

// childGone reports whether the child's process id no longer names a live
// process: gone from the table, or an exit nobody has collected yet.
func childGone(t *testing.T, pid int) bool {
	t.Helper()
	current, err := proc.Lookup(t.Context(), pid)
	return err != nil || current.Holder == proc.HolderDead
}

func TestControllerSignalTeardown(t *testing.T) {
	tests := map[string]struct {
		signal   syscall.Signal
		wantCode int
	}{
		"success: TERM cancels the run and takes a cooperative child down with it": {
			signal:   syscall.SIGTERM,
			wantCode: 143,
		},
		"success: INT is recorded as the signal that ended the run": {
			signal:   syscall.SIGINT,
			wantCode: 143,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, c := signals.Install(t.Context())
			defer c.Stop()

			child := startChild(t, "sleep", "30")
			pid := child.Process.Pid
			if _, ok := c.RegisterChild(pid); !ok {
				t.Fatalf("RegisterChild refused a live child (pid %d)", pid)
			}

			// The cleanup registry must run only after the children are
			// gone: it releases lock directories a child could still be
			// writing under. The entry observes the child at the moment
			// the registry runs.
			var childDeadAtCleanup atomic.Bool
			cleanup.Register(func() { childDeadAtCleanup.Store(childGone(t, pid)) })

			if err := syscall.Kill(os.Getpid(), tt.signal); err != nil {
				t.Fatalf("sending %v to the test process: %v", tt.signal, err)
			}

			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the command context was not cancelled by the signal")
			}
			c.Wait()

			fired, ok := c.Fired()
			if !ok {
				t.Fatal("Fired reported no signal after the teardown finished")
			}
			if fired != tt.signal {
				t.Errorf("Fired = %v, want %v", fired, tt.signal)
			}

			if err := child.Wait(); err == nil {
				t.Error("the child exited cleanly, want a signal death")
			}
			if got := signals.ChildExitCode(child.ProcessState); got != tt.wantCode {
				t.Errorf("ChildExitCode = %d, want %d", got, tt.wantCode)
			}
			if !childDeadAtCleanup.Load() {
				t.Error("the cleanup registry ran while the registered child was still alive")
			}
		})
	}
}

func TestControllerKillEscalation(t *testing.T) {
	tests := map[string]struct{}{
		"success: a child ignoring TERM is killed once the budget lapses": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, c := signals.Install(t.Context())
			defer c.Stop()

			// The child shields itself from TERM, so only the KILL
			// escalation can end it. The readiness line keeps the signal
			// from landing before the trap is installed.
			child := exec.Command("sh", "-c", `trap "" TERM; echo ready; while :; do sleep 1; done`)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatalf("piping the child's stdout: %v", err)
			}
			if err := child.Start(); err != nil {
				t.Fatalf("starting the child: %v", err)
			}
			t.Cleanup(func() {
				if child.ProcessState == nil {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			})
			ready := make([]byte, 6)
			if _, err := io.ReadFull(stdout, ready); err != nil {
				t.Fatalf("waiting for the child's trap: %v", err)
			}
			if _, ok := c.RegisterChild(child.Process.Pid); !ok {
				t.Fatalf("RegisterChild refused a live child (pid %d)", child.Process.Pid)
			}

			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatalf("sending TERM to the test process: %v", err)
			}
			<-ctx.Done()
			c.Wait()

			if err := child.Wait(); err == nil {
				t.Error("the child exited cleanly, want a KILL death")
			}
			if got := signals.ChildExitCode(child.ProcessState); got != 137 {
				t.Errorf("ChildExitCode = %d, want 137 (killed)", got)
			}
		})
	}
}

func TestSpawnWindowCoversRegistration(t *testing.T) {
	tests := map[string]struct{}{
		"success: a child registered inside an open spawn window is still taken down": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, c := signals.Install(t.Context())
			defer c.Stop()

			release, ok := c.BeginSpawn()
			if !ok {
				t.Fatal("BeginSpawn refused before any cancellation")
			}

			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatalf("sending TERM to the test process: %v", err)
			}
			<-ctx.Done()

			// The teardown is sweeping now; the open window is what keeps
			// it looking while this registration is still on its way.
			child := startChild(t, "sleep", "30")
			if _, ok := c.RegisterChild(child.Process.Pid); !ok {
				t.Fatalf("RegisterChild refused a live child (pid %d)", child.Process.Pid)
			}
			release()
			c.Wait()

			if err := child.Wait(); err == nil {
				t.Error("the child exited cleanly, want a signal death")
			}
			if got := signals.ChildExitCode(child.ProcessState); got != 143 {
				t.Errorf("ChildExitCode = %d, want 143 (terminated)", got)
			}
		})
	}
}

func TestBeginSpawnAfterCancellation(t *testing.T) {
	tests := map[string]struct{}{
		"error: a cancelled run refuses to open a spawn window": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, c := signals.Install(t.Context())
			defer c.Stop()

			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatalf("sending TERM to the test process: %v", err)
			}
			<-ctx.Done()
			c.Wait()

			if release, ok := c.BeginSpawn(); ok {
				release()
				t.Error("BeginSpawn opened a window after the run was cancelled")
			}
		})
	}
}

func TestRegisterChild(t *testing.T) {
	tests := map[string]struct {
		pid  func(t *testing.T) int
		want bool
	}{
		"success: a live child registers": {
			pid: func(t *testing.T) int {
				return startChild(t, "sleep", "30").Process.Pid
			},
			want: true,
		},
		"error: a process id that names nothing is refused": {
			pid: func(t *testing.T) int {
				// Darwin's process ids stay far below this bound, so the
				// lookup proves absence rather than racing a real process.
				return 1<<31 - 1
			},
			want: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, c := signals.Install(t.Context())
			defer c.Stop()

			token, ok := c.RegisterChild(tt.pid(t))
			if ok != tt.want {
				t.Fatalf("RegisterChild ok = %t, want %t", ok, tt.want)
			}
			if ok && !c.UnregisterChild(token) {
				t.Error("UnregisterChild reported no entry removed for a live token")
			}
		})
	}
}

func TestChildExitCode(t *testing.T) {
	tests := map[string]struct {
		state func(t *testing.T) *os.ProcessState
		want  int
	}{
		"success: a clean exit forwards zero": {
			state: func(t *testing.T) *os.ProcessState {
				cmd := startChild(t, "true")
				_ = cmd.Wait()
				return cmd.ProcessState
			},
			want: 0,
		},
		"success: a nonzero exit forwards the child's own code": {
			state: func(t *testing.T) *os.ProcessState {
				cmd := startChild(t, "sh", "-c", "exit 7")
				_ = cmd.Wait()
				return cmd.ProcessState
			},
			want: 7,
		},
		"success: a signal death maps to 128 plus the signal number": {
			state: func(t *testing.T) *os.ProcessState {
				cmd := startChild(t, "sleep", "30")
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatalf("signalling the child: %v", err)
				}
				_ = cmd.Wait()
				return cmd.ProcessState
			},
			want: 143,
		},
		"error: a missing state is the fatal status": {
			state: func(t *testing.T) *os.ProcessState { return nil },
			want:  1,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := signals.ChildExitCode(tt.state(t)); got != tt.want {
				t.Errorf("ChildExitCode = %d, want %d", got, tt.want)
			}
		})
	}
}
