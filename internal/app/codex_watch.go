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

	"github.com/zchee/agentctl/internal/cli"
	codexcommands "github.com/zchee/agentctl/internal/commands/codex"
)

func init() { register(codexWatchHandlers) }

func codexWatchHandlers(deps Dependencies, handlers *cli.Handlers) {
	handlers.CodexWatch = func(ctx context.Context, globals cli.Globals, opts cli.CodexWatchOptions) error {
		watch := codexcommands.Watch{NewStatus: func() *codexcommands.Status { return &codexcommands.Status{Out: deps.Stdout, Signals: deps.Signals} }}
		return watch.Run(ctx, globals, opts)
	}
}
