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

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/render"
)

// RunUse starts an isolated Claude session, optionally printing its paths first.
// Live swaps and deletion are separate operations and are not implemented here.
func (p SessionProcess) RunUse(ctx context.Context, globals cli.Globals, opts cli.ClaudeUseOptions) error {
	if opts.Live || opts.Undo || opts.Forget != "" || opts.RestartRemoteControl {
		return errs.NewNotImplemented("claude use live or session management")
	}
	if opts.ID == "" {
		return errs.NewConfig("an account id is required unless one of --undo or --forget is given")
	}
	session, spec, err := prepareSession(ctx, globals.ConfigDir, opts.ID, SessionOptions{ConfigDir: opts.ClaudeConfigDir, FreshContext: opts.FreshContext, NoMCP: opts.NoMCP})
	if err != nil {
		return err
	}
	if opts.JSON {
		var mcp *string
		if session.MCPConfig != "" {
			mcp = &session.MCPConfig
		}
		doc := struct {
			SecureStorageDir string  `json:"securestorage_dir"`
			ConfigDir        string  `json:"config_dir"`
			SessionPath      string  `json:"session_path"`
			MCPConfig        *string `json:"mcp_config"`
		}{spec.SecureStorageDir, spec.ConfigDir, session.Path, mcp}
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
