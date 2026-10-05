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
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func TestStatusAndAccountsRejectIdentityFIFO(t *testing.T) {
	if command := os.Getenv("AGENTCTL_TEST_FIFO_COMMAND"); command != "" {
		home := os.Getenv("AGENTCTL_TEST_FIFO_HOME")
		env := claude.EnvWithHome(home)
		paths := config.NewPaths(filepath.Join(home, "store"))
		if command == "status" {
			status := &Status{Reader: secret.DisabledReader{}, Env: &env, Stdout: io.Discard}
			err := status.Run(t.Context(), cli.Globals{ConfigDir: paths.ConfigDir()}, cli.ClaudeStatusOptions{Timeout: time.Second})
			if errs.ExitCode(err) != errs.ExitPartial {
				t.Fatalf("status exit = %d, want partial: %v", errs.ExitCode(err), err)
			}
		} else {
			accounts := &Accounts{Paths: paths, Env: &env, Reader: secret.DisabledReader{}, Out: io.Discard}
			if err := accounts.List(t.Context(), false); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	tests := map[string]struct{ command string }{
		"success: status reports the unreadable live account": {command: "status"},
		"success: accounts lists the unreadable live account": {command: "accounts"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := unix.Mkfifo(filepath.Join(home, ".claude.json"), 0o600); err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestStatusAndAccountsRejectIdentityFIFO$")
			cmd.Env = append(os.Environ(), "AGENTCTL_TEST_FIFO_COMMAND="+tt.command, "AGENTCTL_TEST_FIFO_HOME="+home)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("FIFO discovery did not terminate successfully: %v (context: %v)\n%s", err, ctx.Err(), output)
			}
		})
	}
}
