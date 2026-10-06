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
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	provider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/runtime/tty"
)

const codexRefreshCommandLockWait = 5 * time.Second

// AccountsRefresh takes interactive consent and delegates refresh policy to the locked driver.
type AccountsRefresh struct {
	Paths  *config.Paths
	Stdin  *os.File
	Prompt commands.Prompter
}

// Run performs one requested resend or floor reset.
// Piped stdin refuses both actions even with --yes, before opening the namespace.
func (c *AccountsRefresh) Run(ctx context.Context, opts cli.CodexAccountsRefreshOptions) error {
	stdinIsTTY := tty.IsTerminal(c.Stdin)
	if !opts.Resend && !opts.ResetFloor {
		return errs.NewConfig("`agentctl codex accounts refresh` needs `--resend` (send the stored refresh token once more) or `--reset-floor` (lift a terminal 401 state)")
	}
	registry, err := config.LoadRegistry(ctx, c.Paths)
	if err != nil {
		return err
	}
	record, err := ResolveAccount(registry.CodexAccounts, opts.ID)
	if err != nil {
		return err
	}
	shown := AccountKey(record)
	owned := provider.Owned(record)
	if owned == nil {
		return errs.NewRefused(0, fmt.Sprintf("agentctl stores no credential for `%s`, so it has no refresh state to act on; `agentctl codex login` adds one", shown))
	}
	if c.Prompt == nil {
		return errs.NewConfig("Codex refresh requires a confirmation prompt")
	}
	if opts.Resend {
		question := fmt.Sprintf("send `%s`'s stored refresh token once more? agentctl does not know whether the token host consumed the first send, and a reuse can be answered by revoking the grant; this is the only re-send agentctl will make for it", shown)
		consent, err := takeRefreshConsent(ctx, c.Prompt, stdinIsTTY, opts.Yes, question, provider.NewResendConsent)
		if err != nil {
			return err
		}
		if consent == nil {
			return c.Prompt.Tell("nothing was sent")
		}
		permit := provider.NewPostPermit()
		refresh := provider.RefreshContext{Paths: c.Paths, Deadline: time.Now().Add(codexRefreshCommandLockWait + provider.RefreshPostBudget + provider.RefreshWriteAllowance + time.Second), LockBudget: codexRefreshCommandLockWait}
		report := provider.RunRefresh(ctx, permit, owned, provider.SendMode{Kind: provider.SendResend, Consent: consent}, refresh)
		return reportResend(c.Prompt, shown, report)
	}
	question := fmt.Sprintf("lift the refresh floor for `%s`, so agentctl may send its refresh token again after three that did not help?", shown)
	consent, err := takeRefreshConsent(ctx, c.Prompt, stdinIsTTY, opts.Yes, question, provider.NewResetConsent)
	if err != nil {
		return err
	}
	if consent == nil {
		return c.Prompt.Tell("nothing was changed")
	}
	guard, err := provider.AcquireCodex(ctx, c.Paths, owned, codexRefreshCommandLockWait)
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	defer func() { _ = guard.Release() }()
	namespace, err := provider.OpenOwnedNamespace(c.Paths, owned, guard)
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	defer func() { _ = namespace.Close() }()
	if err := provider.ResetRefreshFloor(ctx, c.Paths, namespace, consent); err != nil {
		return err
	}
	return c.Prompt.Tell(fmt.Sprintf("%s: the 401 floor and the `refresh did not help` count are back to their defaults; no token was sent", shown))
}

func takeRefreshConsent[T any](ctx context.Context, prompt commands.Prompter, stdinIsTTY, yes bool, question string, build func(string, bool, bool) (*T, error)) (*T, error) {
	answer := ""
	if stdinIsTTY && !yes {
		confirmed, err := prompt.Confirm(ctx, question)
		if err != nil {
			return nil, err
		}
		answer = "no"
		if confirmed {
			answer = "yes"
		}
	}
	consent, err := build(answer, stdinIsTTY, yes)
	if errors.Is(err, provider.ErrRefreshNotConfirmed) {
		return nil, nil
	}
	if errors.Is(err, provider.ErrRefreshNotATerminal) {
		return nil, errs.NewRefused(0, question+" — "+err.Error())
	}
	return consent, err
}
