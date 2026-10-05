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
	"fmt"
	"os"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/tui"
)

// Watch runs status passes in a terminal display. NewStatus must create a fresh
// keychain reader per call, so a later pass observes a newly locked keychain.
type Watch struct {
	NewStatus     func() *Status
	Input, Output *os.File
}

// Run resolves the store, enforces the polling floor and owns the terminal until
// the user quits or ctx is canceled. Terminal and store failures are returned.
func (w *Watch) Run(ctx context.Context, globals cli.Globals, opts cli.ClaudeWatchOptions) error {
	if opts.Interval < cli.WatchFloor {
		return errs.NewConfig(fmt.Sprintf("`--interval %ds` is below the %ds floor; agentctl will not poll the usage API more often than once every %d seconds", int64(opts.Interval/time.Second), int64(cli.WatchFloor/time.Second), int64(cli.WatchFloor/time.Second)))
	}
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return err
	}
	if err = paths.EnsureDirs(ctx); err != nil {
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
	model := tui.New[rowOutcome](time.Now(), watchTitle, watchHiddenHint)
	loop := tui.NewLoop(ctx, model, opts.Interval, func(ctx context.Context, forced bool) ([]rowOutcome, error) { return w.collect(ctx, paths, forced) })
	defer loop.Close()
	return tui.Run(ctx, loop, input, output)
}

func (w *Watch) collect(ctx context.Context, paths *config.Paths, forced bool) ([]rowOutcome, error) {
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return nil, err
	}
	status := w.NewStatus()
	found := claude.Discover(ctx, registry, paths, status.Reader, status.Env)
	return status.collect(ctx, paths, found.Rows, statusOptions{noCache: forced, listing: found.Listing}), nil
}

const (
	watchTitle      = "agentctl claude watch"
	watchHiddenHint = "agentctl claude status --all"
)

func (o rowOutcome) WatchTitle() string     { return watchTitle }
func (o rowOutcome) HiddenHint() string     { return watchHiddenHint }
func (o rowOutcome) Gauges() []render.Gauge { return render.UsageGauges(o.usage) }
func (o rowOutcome) AccountTitle(selected bool) string {
	return render.AccountTitle(selected, o.account, o.org, o.plan)
}
func (o rowOutcome) BlockHeight() int       { return render.BlockHeight(len(o.Gauges())) }
func (o rowOutcome) VisibleByDefault() bool { return o.visibleByDefault }
func (o rowOutcome) StateToken() string     { return o.state.Name() }
func (o rowOutcome) DetailLine(now time.Time) string {
	var badge string
	switch o.state.Kind {
	case claude.StateStale:
		badge = "stale"
	case claude.StateRateLimited:
		badge = "rate-limited"
	case claude.StateClaudeSessionDetected:
		badge = "claude-detected"
	case claude.StateKeychainLocked, claude.StateKeychainTimeout:
		badge = "keychain-locked"
	case claude.StateBusy:
		badge = "busy"
	case claude.StateNeedsLogin:
		badge = "needs login"
	}
	var reset time.Time
	if o.usage != nil {
		reset, _ = o.usage.NextReset()
	}
	return render.DetailLine(now, badge, o.state.Label(), o.note, reset)
}
