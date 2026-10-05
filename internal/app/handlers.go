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

// Package app connects parsed commands to their runtime dependencies.
package app

import (
	"context"
	"io"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// Dependencies contains the process resources shared by command handlers.
type Dependencies struct {
	// Stdout receives the reports printed by commands.
	Stdout io.Writer
}

// composer fills the handler fields of one command family. Each family
// lives in its own file and registers itself, so adding a command never
// edits a line another family also edits.
type composer func(deps Dependencies, handlers *cli.Handlers)

var composers []composer

// register adds a family composer. It runs from package initialisers, so
// the order of registration follows the file order of the package and no
// two families fill the same field.
func register(c composer) {
	composers = append(composers, c)
}

// Handlers connects the available commands to their application services.
// Each invocation resolves its own store after the global flags are parsed.
func Handlers(deps Dependencies) cli.Handlers {
	handlers := readHandlers(deps)
	for _, compose := range composers {
		compose(deps, &handlers)
	}
	return handlers
}

// readHandlers composes the read-only commands that landed first.
func readHandlers(deps Dependencies) cli.Handlers {
	return cli.Handlers{
		ClaudeStatus: func(ctx context.Context, globals cli.Globals, opts cli.ClaudeStatusOptions) error {
			timeout := opts.Timeout
			if timeout <= 0 {
				timeout = cli.HTTPTimeoutDefault
			}
			env := claude.EnvFromProcess()
			status := &commands.Status{
				Reader: secret.NewReader(),
				Client: claude.NewUsageClientFromEnv(timeout),
				Env:    &env,
				Stdout: deps.Stdout,
			}
			return status.Run(ctx, globals, opts)
		},
		ClaudeAccountsList: func(ctx context.Context, globals cli.Globals, opts cli.ClaudeAccountsListOptions) error {
			accounts, err := accountsFor(ctx, globals, deps.Stdout)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctx, secret.NamespaceLockWait)
			defer cancel()
			return accounts.List(ctx, opts.All)
		},
		ClaudeAccountsShow: func(ctx context.Context, globals cli.Globals, opts cli.ClaudeAccountsShowOptions) error {
			accounts, err := accountsFor(ctx, globals, deps.Stdout)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctx, secret.NamespaceLockWait)
			defer cancel()
			return accounts.Show(ctx, opts.ID)
		},
	}
}

func accountsFor(ctx context.Context, globals cli.Globals, out io.Writer) (*commands.Accounts, error) {
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return nil, err
	}
	if err := paths.EnsureDirs(ctx); err != nil {
		return nil, err
	}
	env := claude.EnvFromProcess()
	return &commands.Accounts{Paths: paths, Env: &env, Reader: secret.NewReader(), Out: out}, nil
}
