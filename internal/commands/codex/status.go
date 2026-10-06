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
	"encoding/json/jsontext"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
)

// Status gathers and renders the subscription usage of Codex accounts.
type Status struct {
	Env     *codexprovider.Env
	Client  *codexprovider.UsageClient
	Reader  secret.Reader
	Signals *signals.Controller
	Out     io.Writer
}

// Run renders all selected accounts and counts only shown failures.
func (s *Status) Run(ctx context.Context, globals cli.Globals, opts cli.CodexStatusOptions) error {
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
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return err
	}
	env := EnvFromProcess()
	if s.Env != nil {
		env = *s.Env
	}
	plans, err := selectRows(planRows(ctx, paths, registry.CodexAccounts, env), opts.Accounts)
	if err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = cli.HTTPTimeoutDefault
	}
	mayRefresh := false
	for _, plan := range plans {
		if plan.mayRefresh() {
			mayRefresh = true
			break
		}
	}
	deadline := time.Now().Add(PassBudget(timeout, mayRefresh))
	client := s.Client
	if client == nil {
		client = codexprovider.NewUsageClientFromEnv(timeout)
	}
	reader := s.Reader
	if reader == nil {
		reader = secret.NewReader()
	}
	permit := codexprovider.NewPostPermit()
	plans = refreshPrePass(ctx, permit, plans, paths, deadline)
	shared := &passShared{paths: paths, client: client, listing: listKeyring(ctx, plans, reader), options: passOptions{refresh: opts.Refresh, noCache: opts.NoCache, allowPost: true}, signals: s.Signals}
	passes := afterUnauthorized(ctx, permit, collectRows(ctx, plans, shared, deadline), shared, deadline)
	accounts := finishRows(passes)
	shown := make([]codexprovider.Account, 0, len(accounts))
	failed := 0
	for _, account := range accounts {
		if !opts.All && !account.VisibleByDefault() {
			continue
		}
		shown = append(shown, account)
		if !account.State.IsExitNeutral() {
			failed++
		}
	}
	out := s.Out
	if out == nil {
		out = io.Discard
	}
	now := time.Now()
	if opts.JSON {
		report := render.CodexStatusReportV2(shown, now, len(accounts)-len(shown), opts.Raw)
		body, err := render.MarshalStatusReportV2(&report)
		if err != nil {
			return errs.NewConfig("the JSON report could not be serialized: " + err.Error())
		}
		if _, err := out.Write(append(body, '\n')); err != nil {
			return err
		}
	} else {
		report := render.CodexTableReport(accounts, now, time.Local, opts.All)
		if _, err := fmt.Fprintln(out, render.RenderCodex(&report)); err != nil {
			return err
		}
		if opts.Raw {
			for _, account := range shown {
				if account.Usage == nil || account.Usage.Raw == nil {
					continue
				}
				body := append(jsontext.Value(nil), account.Usage.Raw...)
				if err := body.Indent(jsontext.WithIndent("  ")); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(out, "\n--- raw: %s ---\n%s\n", account.ID, body); err != nil {
					return err
				}
			}
		}
	}
	if failed > 0 {
		return errs.NewPartial(failed)
	}
	return nil
}

// UsageRequestBudget bounds four short and two long request phases.
func UsageRequestBudget(timeout time.Duration) time.Duration {
	short := 4 * min(timeout, 5*time.Second)
	if timeout > (time.Duration(math.MaxInt64)-short)/2 {
		return time.Duration(math.MaxInt64)
	}
	return short + 2*timeout
}

// PassBudget includes one refresh and two usage requests when renewal is possible.
func PassBudget(timeout time.Duration, refreshPossible bool) time.Duration {
	request := UsageRequestBudget(timeout)
	if !refreshPossible {
		return request
	}
	const renewal = 21 * time.Second
	if request > (time.Duration(math.MaxInt64)-renewal)/2 {
		return time.Duration(math.MaxInt64)
	}
	return renewal + 2*request
}
