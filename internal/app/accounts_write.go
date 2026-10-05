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

func init() { register(accountsWriteHandlers) }

func accountsWriteHandlers(deps Dependencies, handlers *cli.Handlers) {
	makeAccounts := func(ctx context.Context, globals cli.Globals) (*commands.Accounts, error) {
		paths, err := config.Resolve(globals.ConfigDir)
		if err != nil {
			return nil, err
		}
		if err := paths.EnsureDirs(ctx); err != nil {
			return nil, err
		}
		env := claude.EnvFromProcess()
		return &commands.Accounts{Paths: paths, Env: &env, Out: deps.Stdout}, nil
	}
	handlers.ClaudeAccountsRemove = func(ctx context.Context, globals cli.Globals, opts cli.ClaudeAccountsRemoveOptions) error {
		command, err := makeAccounts(ctx, globals)
		if err != nil {
			return err
		}
		return command.Remove(ctx, opts, commands.TerminalPrompt{In: os.Stdin, Out: deps.Stdout})
	}
	handlers.ClaudeAccountsRelocate = func(ctx context.Context, globals cli.Globals, opts cli.ClaudeAccountsRelocateOptions) error {
		command, err := makeAccounts(ctx, globals)
		if err != nil {
			return err
		}
		client, err := claude.NewOAuthClientFromEnv()
		if err != nil {
			return err
		}
		return command.Relocate(ctx, opts, commands.TerminalPrompt{In: os.Stdin, Out: deps.Stdout}, client)
	}
	handlers.ClaudeAccountsForget = func(ctx context.Context, globals cli.Globals, opts cli.ClaudeAccountsForgetOptions) error {
		command, err := makeAccounts(ctx, globals)
		if err != nil {
			return err
		}
		return command.Forget(ctx, opts.Service, true)
	}
	handlers.ClaudeAccountsUnforget = func(ctx context.Context, globals cli.Globals, opts cli.ClaudeAccountsUnforgetOptions) error {
		command, err := makeAccounts(ctx, globals)
		if err != nil {
			return err
		}
		return command.Forget(ctx, opts.Service, false)
	}
}
