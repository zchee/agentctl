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

package commands

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestDoctorHeartbeatSampling(t *testing.T) {
	tests := map[string]struct{ create, heartbeat, cancel, held bool }{
		"success: unchanged directory is not beating":           {create: true},
		"success: real process heartbeat is observed":           {create: true, heartbeat: true, held: true},
		"error: vanished lock remains conservatively held":      {held: true},
		"error: cancelled interval remains conservatively held": {create: true, cancel: true, held: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".oauth_refresh.lock")
			if tt.create {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.heartbeat {
				doctorHeartbeat(t, path)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			got := doctorHolderAlive(ctx, path, 250*time.Millisecond)
			if diff := gocmp.Diff(tt.held, got); diff != "" {
				t.Errorf("holder observation (-want +got):\n%s", diff)
			}
		})
	}
}

func doctorHeartbeat(t *testing.T, path string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", `touch "$1"; printf 'ready\n'; while :; do touch "$1"; sleep 0.01; done`, "heartbeat", path)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("heartbeat process not ready: %v", scanner.Err())
	}
}
