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

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// RunLive swaps an owned account into the store named by one environment view.
func (p SessionProcess) RunLive(ctx context.Context, globals cli.Globals, opts cli.ClaudeUseOptions) error {
	if opts.ID == "" {
		return errs.NewConfig("`--live` needs the account to swap in; give it an id")
	}
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return err
	}
	if err := paths.EnsureDirs(ctx); err != nil {
		return err
	}
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return err
	}
	incoming, err := registry.ResolveID(opts.ID)
	if err != nil {
		return err
	}
	if incoming.Kind.Owned == nil {
		return errs.NewConfig(fmt.Sprintf("only an account agentctl owns can be swapped into a live store; `%s` is `%s`, whose credentials live outside agentctl's own store", incoming.AccountUUID, incoming.Kind.Name()))
	}
	env := claude.EnvFromProcess()
	inherited, namespaced := claude.SecureStorageNamespace(&env)
	var store *config.AccountRecord
	if namespaced {
		for _, record := range registry.Accounts {
			if record.Kind.Owned != nil && record.Kind.Owned.ExportSpelling == inherited {
				store = new(record)
				break
			}
		}
		if store == nil {
			report := useRefused(claude.SwapRefusal{Kind: claude.SwapNotOwned}, "", fmt.Sprintf("`%s` is not a store agentctl owns, so there is no record saying whose credentials are in it or where the displaced one should go", inherited))
			p.followUpRemoteControl(ctx, opts, report)
			if err := p.emitUse(report, opts.JSON); err != nil {
				return err
			}
			return &errs.ChildExit{Code: report.outcome.ExitCode()}
		}
	}
	swap := useLiveSwap{paths: paths, config: registry, env: &env, live: !namespaced, inherited: inherited, process: p}
	report := swap.swapIn(ctx, useIncoming{record: incoming, direction: secret.DirectionForward, source: useSource{kind: useSourceOwn}}, store, opts)
	p.followUpRemoteControl(ctx, opts, report)
	if err := p.emitUse(report, opts.JSON); err != nil {
		return err
	}
	if code := report.outcome.ExitCode(); code != 0 {
		return &errs.ChildExit{Code: code}
	}
	return nil
}
