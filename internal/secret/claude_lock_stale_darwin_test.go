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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

func TestProcHoldersStoppedPIDs(t *testing.T) {
	tests := map[string]struct{ cancelled bool }{
		"success: only the stopped same-user process is reported": {},
		"error: a cancelled sweep claims no stopped processes":    {cancelled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), ClaudeProcessName)
			sleep, err := os.ReadFile("/bin/sleep")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binary, sleep, 0o700); err != nil {
				t.Fatal(err)
			}
			var running, stopped int32
			for _, stop := range []bool{false, true} {
				child := exec.Command(binary, "60")
				if err := child.Start(); err != nil {
					t.Fatalf("starting the named process: %v", err)
				}
				t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
				wantHolder := proc.HolderAlive
				if stop {
					stopped = int32(child.Process.Pid)
					wantHolder = proc.HolderStopped
					if err := child.Process.Signal(unix.SIGSTOP); err != nil {
						t.Fatal(err)
					}
				} else {
					running = int32(child.Process.Pid)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					observed, err := proc.Lookup(t.Context(), child.Process.Pid)
					if err == nil && observed.Name == ClaudeProcessName && observed.Holder == wantHolder {
						break
					}
					if !time.Now().Before(deadline) {
						t.Fatalf("process %d did not reach %v with name %q: observed=%+v error=%v", child.Process.Pid, wantHolder, ClaudeProcessName, observed, err)
					}
					time.Sleep(time.Millisecond)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelled {
				cancel()
			}
			pids := (ProcHolders{}).StoppedPIDs(ctx)
			if tt.cancelled {
				if diff := gocmp.Diff([]int32(nil), pids); diff != "" {
					t.Fatalf("cancelled sweep (-want +got): %s", diff)
				}
				return
			}
			if !slices.Contains(pids, stopped) || slices.Contains(pids, running) {
				t.Fatalf("StoppedPIDs = %v, want stopped pid %d included and running pid %d absent", pids, stopped, running)
			}
		})
	}
}
