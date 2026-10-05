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

package app

import (
	"context"
	"os"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
)

func init() { register(loginHandlers) }

func loginHandlers(deps Dependencies, handlers *cli.Handlers) {
	handlers.ClaudeLogin = func(ctx context.Context, globals cli.Globals, opts cli.ClaudeLoginOptions) error {
		paths, err := config.Resolve(globals.ConfigDir)
		if err != nil {
			return err
		}
		client, err := claude.NewLoginClientFromEnv()
		if err != nil {
			return err
		}
		env := claude.EnvFromProcess()
		login := &commands.Login{
			Paths: paths, Client: client,
			IO:           commands.LoginTerminal{TerminalPrompt: commands.TerminalPrompt{In: os.Stdin, Out: deps.Stdout}, Err: os.Stderr},
			LiveIdentity: claude.LiveIdentity(&env), LiveIdentitySource: claude.ClaudeJSONPath(&env),
		}
		return login.Run(ctx, opts)
	}
}
