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
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/testutil"
)

func TestExecBinaryExitAndSignalTeardown(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agentctl")
	build := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".")
	build.Dir = testutil.RepoRoot(t)
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.27.1")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	tests := map[string]struct {
		signal syscall.Signal
		code   int
	}{
		"success: silent status forwarding": {code: 37},
		"success: term tears down child":    {signal: syscall.SIGTERM, code: 143},
		"success: hup tears down child":     {signal: syscall.SIGHUP, code: 129},
		"success: int tears down child":     {signal: syscall.SIGINT, code: 130},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := testutil.New(t)
			f.WriteRegistry([]any{f.OwnedRecord("account", "org")})
			script := "printf 'child output\\n'; printf 'child error\\n' >&2; exit 37"
			pidFile := f.Scratch("child.pid")
			if tt.signal != 0 {
				script = "printf '%s' $$ > \"$1\"; exec /bin/sleep 30"
			}
			cmd := exec.CommandContext(t.Context(), binary, "--config-dir", f.ConfigDir(), "claude", "exec", "account", "--no-mcp", "--", "/bin/sh", "-c", script, "sh", pidFile)
			f.Apply(cmd)
			cmd.Env = append(cmd.Env, "GORACE=atexit_sleep_ms=0")
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			pid := 0
			if tt.signal != 0 {
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
			waitReady:
				for {
					data, err := os.ReadFile(pidFile)
					if err == nil && len(data) > 0 {
						pid, err = strconv.Atoi(string(data))
						if err != nil {
							t.Fatal(err)
						}
						break waitReady
					}
					select {
					case <-ticker.C:
					case <-deadline.C:
						t.Fatal("child did not announce its pid")
					case <-t.Context().Done():
						t.Fatal(t.Context().Err())
					}
				}
				if err := cmd.Process.Signal(tt.signal); err != nil {
					t.Fatal(err)
				}
			}
			err := cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tt.code {
				t.Fatalf("exit=%v, want %d; stderr=%s", err, tt.code, stderr.String())
			}
			if tt.signal == 0 {
				if diff := gocmp.Diff("child output\n", out.String()); diff != "" {
					t.Fatal(diff)
				}
				if diff := gocmp.Diff("child error\n", stderr.String()); diff != "" {
					t.Fatal(diff)
				}
			} else {
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("child %d still exists after parent exit: %v", pid, err)
				}
				if strings.Contains(stderr.String(), "agentctl:") {
					t.Fatalf("extra signal diagnostic: %s", stderr.String())
				}
			}
		})
	}
}
