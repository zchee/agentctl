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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/coordinator"
	"github.com/zchee/agentctl/internal/runtime/signals"
)

// SessionProcess supplies inherited streams and the process signal controller.
// A nil controller is useful for embedding; context cancellation still reaps children.
type SessionProcess struct {
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Signals *signals.Controller
}

// Exec starts argv directly with the session environment and returns its status.
// It strips inherited OAuth overrides and adds MCP arguments only to claude.
// Start and wait failures return errors; a child's unsuccessful exit is a status.
func (p SessionProcess) Exec(ctx context.Context, spec ExportSpec, argv []string) (int, error) {
	if len(argv) == 0 {
		return 0, errs.NewConfig("no command was given to run")
	}
	pass := coordinator.Standalone(ctx, p.Signals, time.Now().Add(365*24*time.Hour))
	release, ok := pass.BeginSpawn()
	if !ok {
		return 0, errs.NewRefused(0, "cancelled before the command could start")
	}
	args := slices.Clone(argv[1:])
	if filepath.Base(argv[0]) == "claude" && spec.MCPConfig != "" {
		args = append(args, "--mcp-config", spec.MCPConfig)
	}
	cmd := exec.Command(argv[0], args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.In, p.Out, p.Err
	for _, env := range os.Environ() {
		name, _, _ := strings.Cut(env, "=")
		if name == claude.OAuthTokenEnv || name == claude.ConfigDirEnv || name == claude.SecureStorageEnv {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env, claude.ConfigDirEnv+"="+spec.ConfigDir, claude.SecureStorageEnv+"="+spec.SecureStorageDir)
	if err := cmd.Start(); err != nil {
		release()
		return 0, errs.NewIO(fmt.Sprintf("could not start `%s`", argv[0]), err)
	}
	token := pass.RegisterChild(cmd)
	release()
	state, err := pass.WaitChild(token)
	if err != nil {
		if errors.Is(err, coordinator.ErrPassCancelled) || errors.Is(err, coordinator.ErrChildKilled) {
			return 0, errs.NewRefused(0, "cancelled while the command was running")
		}
		return 0, errs.NewIO("could not wait for the command to exit", err)
	}
	return signals.ChildExitCode(state), nil
}

// RunExec prepares the account's session and forwards the invoked child's status.
func (p SessionProcess) RunExec(ctx context.Context, globals cli.Globals, opts cli.ClaudeExecOptions) error {
	_, spec, err := prepareSession(ctx, globals.ConfigDir, opts.ID, SessionOptions{ConfigDir: opts.ClaudeConfigDir, FreshContext: opts.FreshContext, NoMCP: opts.NoMCP})
	if err != nil {
		return err
	}
	code, err := p.Exec(ctx, spec, opts.Command)
	if err != nil {
		return err
	}
	if code != 0 {
		return &errs.ChildExit{Code: code}
	}
	return nil
}
