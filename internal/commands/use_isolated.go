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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/secret"
)

// RunUse dispatches isolated sessions, live swaps, and removal requests.
func (p SessionProcess) RunUse(ctx context.Context, globals cli.Globals, opts cli.ClaudeUseOptions) error {
	if opts.Forget != "" {
		return p.RunForget(ctx, globals, opts)
	}
	if opts.RestartRemoteControl && !useRemoteControlSupported {
		report := useRemoteControlPlatformRefusal()
		if err := p.emitUse(report, opts.JSON); err != nil {
			return err
		}
		return &errs.ChildExit{Code: report.outcome.ExitCode()}
	}
	if opts.Undo {
		return p.RunUndo(ctx, globals, opts)
	}
	if opts.Live {
		return p.RunLive(ctx, globals, opts)
	}
	if opts.ID == "" {
		return errs.NewConfig("an account id is required unless one of --undo or --forget is given")
	}
	session, spec, paths, record, err := prepareSession(ctx, globals.ConfigDir, opts.ID, SessionOptions{ConfigDir: opts.ClaudeConfigDir, FreshContext: opts.FreshContext, NoMCP: opts.NoMCP})
	if err != nil {
		return err
	}
	// Seeding is a convenience: whatever it reports, the session launches,
	// and only a signal, which also refuses the launch, ends the run early.
	var seed *useSeedReport
	if !opts.NoMCP && !opts.NoMCPSeed {
		reader := secret.Reader(secret.DisabledReader{})
		if p.Keychain != nil {
			reader = p.Keychain()
		}
		env := claude.EnvFromProcess()
		seed = p.seedNamespaceSiblings(ctx, seedInput{paths: paths, record: record, env: &env, reader: reader, id: opts.ID})
		if line := seed.prose(); line != "" && p.Err != nil {
			_, _ = fmt.Fprintln(p.Err, line)
		}
	}
	if opts.JSON {
		var mcp *string
		if session.MCPConfig != "" {
			mcp = &session.MCPConfig
		}
		doc := struct {
			SecureStorageDir string         `json:"securestorage_dir"`
			ConfigDir        string         `json:"config_dir"`
			SessionPath      string         `json:"session_path"`
			MCPConfig        *string        `json:"mcp_config"`
			MCPSeed          *useSeedReport `json:"mcp_seed"`
		}{spec.SecureStorageDir, spec.ConfigDir, session.Path, mcp, seed}
		data, err := json.Marshal(doc, jsontext.WithIndent("  "), jsontext.EscapeForHTML(false))
		if err != nil {
			return errs.NewConfig("could not render the session as JSON")
		}
		if err := render.Print(p.Out, string(data)); err != nil {
			return err
		}
	}
	code, err := p.Exec(ctx, spec, []string{"claude"})
	if err != nil {
		return err
	}
	if code != 0 {
		return &errs.ChildExit{Code: code}
	}
	return nil
}
