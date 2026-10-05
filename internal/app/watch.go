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
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func init() { register(watchHandlers) }

func watchHandlers(deps Dependencies, handlers *cli.Handlers) {
	handlers.ClaudeWatch = func(ctx context.Context, globals cli.Globals, opts cli.ClaudeWatchOptions) error {
		env := claude.EnvFromProcess()
		oauth, err := claude.NewOAuthClientFromEnv()
		if err != nil {
			return err
		}
		output, _ := deps.Stdout.(*os.File)
		watch := commands.Watch{Input: os.Stdin, Output: output, NewStatus: func() *commands.Status {
			return &commands.Status{Reader: secret.NewReader(), Client: claude.NewUsageClientFromEnv(cli.HTTPTimeoutDefault), Refresher: oauth, Profiles: oauth, Writer: secret.NewKeychainWriter(), Env: &env}
		}}
		return watch.Run(ctx, globals, opts)
	}
}
