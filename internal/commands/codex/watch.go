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

package codex

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/tui"
)

// Watch displays read-only Codex usage passes without a refresh capability.
type Watch struct {
	NewStatus     func() *Status
	Input, Output *os.File
}

// Run enforces the polling floor and owns the terminal until cancellation or quit.
func (w *Watch) Run(ctx context.Context, globals cli.Globals, opts cli.CodexWatchOptions) error {
	if opts.Interval < cli.WatchFloor {
		return errs.NewConfig(fmt.Sprintf("`--interval %ds` is below the %ds floor; agentctl will not poll the usage API more often than once every %d seconds", int64(opts.Interval/time.Second), int64(cli.WatchFloor/time.Second), int64(cli.WatchFloor/time.Second)))
	}
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return err
	}
	if err := paths.EnsureDirs(ctx); err != nil {
		return err
	}
	if err := paths.EnsureCodexDirs(ctx); err != nil {
		return err
	}
	if w.NewStatus == nil {
		return errs.NewConfig("the watch status factory is not configured")
	}
	input, output := w.Input, w.Output
	if input == nil {
		input = os.Stdin
	}
	if output == nil {
		output = os.Stdout
	}
	model := tui.New[render.CodexWatchRow](time.Now(), "agentctl codex watch", "agentctl codex status --all")
	loop := tui.NewLoop(ctx, model, opts.Interval, func(ctx context.Context, forced bool) ([]render.CodexWatchRow, error) {
		return w.collect(ctx, paths, forced)
	})
	defer loop.Close()
	return tui.Run(ctx, loop, input, output)
}

func (w *Watch) collect(ctx context.Context, paths *config.Paths, forced bool) ([]render.CodexWatchRow, error) {
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return nil, err
	}
	status := w.NewStatus()
	env := EnvFromProcess()
	if status.Env != nil {
		env = *status.Env
	}
	plans := planRows(ctx, paths, registry.CodexAccounts, env)
	reader := status.Reader
	if reader == nil {
		reader = secret.NewReader()
	}
	client := status.Client
	if client == nil {
		client = codexprovider.NewUsageClientFromEnv(cli.HTTPTimeoutDefault)
	}
	deadline := time.Now().Add(UsageRequestBudget(cli.HTTPTimeoutDefault))
	shared := &passShared{paths: paths, client: client, listing: listKeyring(ctx, plans, reader), options: passOptions{noCache: forced}, signals: status.Signals}
	accounts := finishRows(collectRows(ctx, plans, shared, deadline))
	rows := make([]render.CodexWatchRow, len(accounts))
	for i, account := range accounts {
		rows[i] = render.CodexWatchRow{Account: account}
	}
	return rows, nil
}
