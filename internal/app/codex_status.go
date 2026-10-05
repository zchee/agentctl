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
	"io"

	"github.com/zchee/agentctl/internal/cli"
	codexcommands "github.com/zchee/agentctl/internal/commands/codex"
	"github.com/zchee/agentctl/internal/config"
)

func init() { register(codexStatusHandlers) }

func codexStatusHandlers(deps Dependencies, handlers *cli.Handlers) {
	handlers.CodexAccountsList = func(ctx context.Context, globals cli.Globals, opts cli.CodexAccountsListOptions) error {
		accounts, err := codexAccountsFor(globals, deps.Stdout)
		if err != nil {
			return err
		}
		return accounts.List(ctx, opts)
	}
	handlers.CodexAccountsShow = func(ctx context.Context, globals cli.Globals, opts cli.CodexAccountsShowOptions) error {
		accounts, err := codexAccountsFor(globals, deps.Stdout)
		if err != nil {
			return err
		}
		return accounts.Show(ctx, opts)
	}
	handlers.CodexAccountsForget = func(ctx context.Context, globals cli.Globals, opts cli.CodexAccountsForgetOptions) error {
		accounts, err := codexAccountsFor(globals, deps.Stdout)
		if err != nil {
			return err
		}
		return accounts.Forget(ctx, opts.ID, true)
	}
	handlers.CodexAccountsUnforget = func(ctx context.Context, globals cli.Globals, opts cli.CodexAccountsUnforgetOptions) error {
		accounts, err := codexAccountsFor(globals, deps.Stdout)
		if err != nil {
			return err
		}
		return accounts.Forget(ctx, opts.ID, false)
	}
	handlers.CodexAccountsSet = func(ctx context.Context, globals cli.Globals, opts cli.CodexAccountsSetOptions) error {
		accounts, err := codexAccountsFor(globals, deps.Stdout)
		if err != nil {
			return err
		}
		return accounts.Set(ctx, opts)
	}
}

func codexAccountsFor(globals cli.Globals, out io.Writer) (*codexcommands.Accounts, error) {
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return nil, err
	}
	return &codexcommands.Accounts{Paths: paths, Out: out}, nil
}
