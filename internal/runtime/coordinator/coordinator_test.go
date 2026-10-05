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

package coordinator_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/coordinator"
	"github.com/zchee/agentctl/internal/runtime/proc"
	"github.com/zchee/agentctl/internal/runtime/signals"
)

// Several tests below drive the process-wide cleanup registry or install a
// real signal handler, so this package's tests do not run in parallel.

// farDeadline is a pass deadline no test reaches.
func farDeadline() time.Time {
	return time.Now().Add(time.Minute)
}

// start starts command with args and fails the test if it cannot.
func start(t *testing.T, command string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(command, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", command, err)
	}
	return cmd
}

// waitGone polls until the process id no longer names a live process; a
// collected exit counts as gone, an uncollected one as still accounted.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := proc.Lookup(t.Context(), pid)
		if err != nil || current.Holder == proc.HolderDead {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}

func TestRunPassBoundedWorkers(t *testing.T) {
	tests := map[string]struct {
		jobs       int
		maxWorkers int
	}{
		"success: eight jobs never run more than four at once": {
			jobs:       8,
			maxWorkers: 4,
		},
		"success: a worker bound below one is clamped to one": {
			jobs:       3,
			maxWorkers: 0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bound := max(tt.maxWorkers, 1)
			var running, peak atomic.Int64
			jobs := make([]coordinator.Job[int], tt.jobs)
			for i := range jobs {
				jobs[i] = func(pass *coordinator.PassCtx) int {
					now := running.Add(1)
					defer running.Add(-1)
					// Track the high-water mark of concurrent jobs.
					for {
						seen := peak.Load()
						if now <= seen || peak.CompareAndSwap(seen, now) {
							break
						}
					}
					time.Sleep(20 * time.Millisecond)
					return 1
				}
			}

			results := coordinator.RunPass(t.Context(), nil, jobs, farDeadline(), tt.maxWorkers)
			total := 0
			for r := range results {
				total += r
			}

			if total != tt.jobs {
				t.Errorf("received %d results, want %d", total, tt.jobs)
			}
			if got := peak.Load(); got > int64(bound) {
				t.Errorf("observed %d concurrent jobs, want at most %d", got, bound)
			}
		})
	}
}

func TestRunPassCancellation(t *testing.T) {
	tests := map[string]struct{}{
		"success: cancelling the pass closes the channel without running the queue dry": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var started atomic.Int64
			jobs := make([]coordinator.Job[int], 8)
			for i := range jobs {
				jobs[i] = func(pass *coordinator.PassCtx) int {
					started.Add(1)
					cancel()
					<-pass.Context().Done()
					return 1
				}
			}

			results := coordinator.RunPass(ctx, nil, jobs, farDeadline(), 2)
			received := 0
			for range results {
				received++
			}

			// The two in-flight jobs saw the cancellation; the rest of the
			// queue was refused before running.
			if got := started.Load(); got > 2 {
				t.Errorf("%d jobs started, want at most the two in flight", got)
			}
			if received > int(started.Load()) {
				t.Errorf("received %d results from %d started jobs", received, started.Load())
			}
		})
	}
}

func TestRunPassDeadline(t *testing.T) {
	tests := map[string]struct{}{
		"success: a lapsed deadline refuses the jobs and runs the cleanup registry": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			var cleaned atomic.Bool
			token := cleanup.Register(func() { cleaned.Store(true) })
			defer cleanup.Unregister(token)

			var ran atomic.Int64
			jobs := []coordinator.Job[int]{
				func(pass *coordinator.PassCtx) int { ran.Add(1); return 1 },
			}

			results := coordinator.RunPass(t.Context(), nil, jobs, time.Now().Add(-time.Second), 4)
			for range results {
			}

			if got := ran.Load(); got != 0 {
				t.Errorf("%d jobs ran past the deadline, want none", got)
			}
			if !cleaned.Load() {
				t.Error("the cleanup registry did not run on the abnormal pass end")
			}
		})
	}
}

func TestWaitChild(t *testing.T) {
	tests := map[string]struct {
		command  []string
		wantCode int
	}{
		"success: a clean exit comes back with its own code": {
			command:  []string{"sh", "-c", "exit 0"},
			wantCode: 0,
		},
		"success: a nonzero exit is an answer, not a wait failure": {
			command:  []string{"sh", "-c", "exit 7"},
			wantCode: 7,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pass := coordinator.Standalone(t.Context(), nil, farDeadline())
			cmd := start(t, tt.command[0], tt.command[1:]...)
			token := pass.RegisterChild(cmd)

			state, err := pass.WaitChild(token)
			if err != nil {
				t.Fatalf("WaitChild: %v", err)
			}
			if got := state.ExitCode(); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
		})
	}
}

func TestWaitChildCancellation(t *testing.T) {
	tests := map[string]struct{}{
		"success: cancellation kills and reaps the child the job was waiting on": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			pass := coordinator.Standalone(ctx, nil, farDeadline())
			cmd := start(t, "sleep", "30")
			token := pass.RegisterChild(cmd)

			timer := time.AfterFunc(30*time.Millisecond, cancel)
			defer timer.Stop()

			state, err := pass.WaitChild(token)
			if !errors.Is(err, coordinator.ErrPassCancelled) {
				t.Fatalf("WaitChild error = %v, want ErrPassCancelled", err)
			}
			if state != nil {
				t.Errorf("WaitChild state = %v, want nil", state)
			}
			waitGone(t, cmd.Process.Pid)
		})
	}
}

func TestWaitChildTimeout(t *testing.T) {
	tests := map[string]struct {
		command    []string
		timeout    time.Duration
		wantAnswer bool
		wantCode   int
		wantKilled bool
	}{
		"success: a child that answers inside the budget is reported": {
			command:    []string{"sh", "-c", "exit 3"},
			timeout:    5 * time.Second,
			wantAnswer: true,
			wantCode:   3,
		},
		"success: a child that outlives the budget is killed and reaped": {
			command:    []string{"sleep", "30"},
			timeout:    50 * time.Millisecond,
			wantKilled: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pass := coordinator.Standalone(t.Context(), nil, farDeadline())
			cmd := start(t, tt.command[0], tt.command[1:]...)
			token := pass.RegisterChild(cmd)

			state, err := pass.WaitChildTimeout(token, tt.timeout)
			if err != nil {
				t.Fatalf("WaitChildTimeout: %v", err)
			}
			if tt.wantAnswer {
				if state == nil {
					t.Fatal("WaitChildTimeout gave no answer inside the budget")
				}
				if got := state.ExitCode(); got != tt.wantCode {
					t.Errorf("exit code = %d, want %d", got, tt.wantCode)
				}
			}
			if tt.wantKilled {
				if state != nil {
					t.Fatalf("WaitChildTimeout state = %v, want nil after the budget", state)
				}
				waitGone(t, cmd.Process.Pid)
			}
		})
	}
}

func TestRunPassReapsLeakedChild(t *testing.T) {
	tests := map[string]struct{}{
		"success: a child a job leaked is killed when the pass ends": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			jobs := []coordinator.Job[int]{
				func(pass *coordinator.PassCtx) int {
					cmd := start(t, "sleep", "30")
					pass.RegisterChild(cmd)
					return cmd.Process.Pid
				},
			}

			results := coordinator.RunPass(t.Context(), nil, jobs, farDeadline(), 1)
			pid := 0
			for r := range results {
				pid = r
			}
			if pid == 0 {
				t.Fatal("the job's result never arrived")
			}
			waitGone(t, pid)
		})
	}
}

func TestRegisterChildAfterStop(t *testing.T) {
	tests := map[string]struct{}{
		"success: a child registered after cancellation is killed on arrival": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			pass := coordinator.Standalone(ctx, nil, farDeadline())

			cmd := start(t, "sleep", "30")
			token := pass.RegisterChild(cmd)

			if _, err := pass.WaitChild(token); !errors.Is(err, coordinator.ErrPassCancelled) {
				t.Fatalf("WaitChild error = %v, want ErrPassCancelled", err)
			}
			waitGone(t, cmd.Process.Pid)
		})
	}
}

func TestPassChildrenReachableFromSignalExit(t *testing.T) {
	tests := map[string]struct{}{
		"success: a registered pass child dies with the process on a signal": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, controller := signals.Install(t.Context())
			defer controller.Stop()

			pass := coordinator.Standalone(ctx, controller, farDeadline())
			cmd := start(t, "sleep", "30")
			token := pass.RegisterChild(cmd)

			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatalf("sending TERM to the test process: %v", err)
			}
			<-ctx.Done()
			controller.Wait()

			// The signal teardown killed the child; the pass's own wait
			// then observes the reaped signal death as an answer.
			state, err := pass.WaitChild(token)
			if errors.Is(err, coordinator.ErrPassCancelled) {
				// The wait itself may win the race and do the killing;
				// either way the child is gone.
			} else if err != nil {
				t.Fatalf("WaitChild: %v", err)
			} else if got := signals.ChildExitCode(state); got != 143 && got != 137 {
				t.Errorf("ChildExitCode = %d, want a signal death (143 or 137)", got)
			}
			waitGone(t, cmd.Process.Pid)
		})
	}
}
