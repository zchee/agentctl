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
	"io"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	provider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
)

// Login runs the vendor CLI in an owned home and installs only its verified grant.
type Login struct {
	Paths    *config.Paths
	Env      provider.Env
	Reader   secret.Reader
	In       io.Reader
	Out, Err io.Writer
	Prompt   commands.Prompter
	Terminal bool
	Signals  *signals.Controller
}

// Run serializes login, verifies all child evidence, and records a sealed namespace.
// Refusals discard the scratch credential without changing an existing grant.
func (l *Login) Run(ctx context.Context, opts cli.CodexLoginOptions) error {
	if err := l.Paths.EnsureDirs(ctx); err != nil {
		return err
	}
	if err := l.Paths.EnsureCodexDirs(ctx); err != nil {
		return err
	}
	lifecycle, err := provider.AcquireScratchLock(ctx, l.Paths)
	if err != nil {
		return err
	}
	defer func() { _ = lifecycle.Release() }()
	root, err := provider.OpenScratchRoot(ctx, l.Paths.CodexScratchRoot())
	if err != nil {
		return err
	}
	rootOwned := true
	defer func() {
		if rootOwned {
			_ = root.Close()
		}
	}()
	provider.SweepLoginScratch(ctx, root, 15*time.Minute, l.Err)
	bin, err := provider.ResolveLoginBinary()
	if err != nil {
		return err
	}
	before, err := l.listing(ctx)
	if err != nil {
		return errs.NewRefused(0, "could not read the keychain before the login: "+err.Error())
	}
	scratch, err := provider.NewLoginScratch(ctx, root)
	rootOwned = false
	if err != nil {
		return err
	}
	defer scratch.Discard(l.Err)
	child := provider.LoginChild{In: l.In, Out: l.Out, Err: l.Err, Signals: l.Signals}
	report, err := child.RunReport(ctx, scratch, bin, before, func() ([]string, error) { return l.listing(ctx) })
	if err != nil {
		return err
	}
	return l.install(ctx, scratch, report, opts)
}

func (l *Login) listing(ctx context.Context) ([]string, error) {
	if l.Reader == nil {
		return []string{}, nil
	}
	var entries []secret.ServiceEntry
	var err error
	if fresh, ok := l.Reader.(interface {
		ListServicesUncached(context.Context, string) ([]secret.ServiceEntry, error)
	}); ok {
		entries, err = fresh.ListServicesUncached(ctx, provider.KeyringService)
	} else {
		entries, err = l.Reader.ListServices(ctx, provider.KeyringService)
	}
	if errors.Is(err, secret.ErrKeychainUnsupported) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	accounts := make([]string, 0, len(entries))
	for _, entry := range entries {
		account := entry.Account
		if account == "" {
			account = entry.Service + " (no account)"
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func (l *Login) install(ctx context.Context, scratch *provider.LoginScratch, report *provider.PostExitReport, opts cli.CodexLoginOptions) error {
	verified, err := provider.VerifyLogin(ctx, scratch.Path(), report)
	if err != nil {
		for _, account := range report.GainedCodexAuth() {
			if !provider.IsHomeAccount(account) {
				continue
			}
			if auditErr := provider.AppendCodexEvent(ctx, l.Paths, "", "", provider.CodexEvent{Outcome: provider.AuditLoginKeychainGained, KeychainAccount: &account}); auditErr != nil {
				_, _ = fmt.Fprintln(l.Err, "agentctl: a Codex Auth item this login left behind could not be recorded in the write log: "+auditErr.Error())
			}
		}
		return errs.NewRefused(0, err.Error())
	}
	identity := verified.Identity()
	if home, err := provider.ResolveHome(l.Env); err == nil {
		live := provider.ReadAuth(ctx, home.Dir)
		if live.Kind == provider.ResolvedCredentials {
			if found := live.Credentials.Identity(); found != nil && found.UserID == identity.UserID && found.AccountID == identity.AccountID {
				_, _ = fmt.Fprintf(l.Out, "note: `%s` already holds this same account; agentctl now has its own copy, and the two grants refresh independently\n", home.Dir)
			}
		}
	}
	shown, err := loginAlreadyOwned(ctx, l.Paths, identity.UserID, identity.AccountID)
	if err != nil {
		return err
	}
	if shown != "" {
		if err := loginConfirmOverwrite(ctx, shown, l.Terminal, l.Prompt); err != nil {
			return err
		}
	}
	guard, err := provider.AcquireCodexForInstall(ctx, l.Paths, verified, 5*time.Second)
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	defer func() { _ = guard.Release() }()
	active := fault.Active()
	active.PausePoint(fault.CodexLoginBeforeInstall)
	if err := ctx.Err(); err != nil {
		return err
	}
	receipt, _, err := provider.WriteAuth(guard.Context(), l.Paths, verified, guard)
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	if err := provider.ResetRefreshStateForLogin(guard.Context(), l.Paths, guard); err != nil {
		_, _ = fmt.Fprintln(l.Err, "agentctl: the refresh marker could not be reset after a login install: "+err.Error())
	}
	scratch.UnlinkCredential()
	overwrote := receipt.Overwrote()
	if err := provider.AppendCodexReceipt(guard.Context(), l.Paths, receipt); err != nil {
		return err
	}
	if err := guard.Release(); err != nil {
		return err
	}
	if active.Is(fault.CodexLoginAfterWrite) {
		return errs.NewRefused(0, "stopped between the install and the registry update (injected by `"+fault.CodexLoginAfterWrite+"`)")
	}
	if err := l.record(ctx, identity, opts); err != nil {
		return err
	}
	who := identity.UserID
	if identity.Email != nil {
		who = *identity.Email
	}
	description := "a new grant"
	if overwrote {
		description = "replacing the grant that was there"
	}
	_, _ = fmt.Fprintf(l.Out, "logged in as %s (%s)\n", who, description)
	return nil
}

func (l *Login) record(ctx context.Context, identity provider.Identity, opts cli.CodexLoginOptions) error {
	export, err := l.Paths.CodexNamespaceDir(identity.UserID, identity.AccountID)
	if err != nil {
		return err
	}
	refresh := config.RefreshAuto
	if opts.NoRefresh {
		refresh = config.RefreshNever
	}
	kind := config.CodexKindOwned(export, refresh)
	return config.UpdateRegistry(ctx, l.Paths, func(registry *config.Registry) {
		for i := range registry.CodexAccounts {
			row := &registry.CodexAccounts[i]
			if row.ChatGPTUserID == identity.UserID && row.ChatGPTAccountID == identity.AccountID {
				row.Email, row.PlanType, row.Kind, row.Forgotten = identity.Email, identity.Plan, kind, false
				if opts.Label != "" {
					row.Label = &opts.Label
				}
				return
			}
		}
		row := config.CodexAccountRecord{ChatGPTUserID: identity.UserID, ChatGPTAccountID: identity.AccountID, Email: identity.Email, PlanType: identity.Plan, Kind: kind, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if opts.Label != "" {
			row.Label = &opts.Label
		}
		registry.CodexAccounts = append(registry.CodexAccounts, row)
	})
}

// LoginTerminal asks the login's confirmation without letting a broken output pipe
// decide whether a verified scratch credential is cleaned up.
type LoginTerminal struct{ commands.TerminalPrompt }

// Tell drops an undeliverable message; it never authorizes an action.
func (p LoginTerminal) Tell(message string) error { _, _ = fmt.Fprintln(p.Out, message); return nil }

// Confirm accepts input only through the terminal-aware shared prompt.
func (p LoginTerminal) Confirm(ctx context.Context, question string) (bool, error) {
	_, _ = fmt.Fprintf(p.Out, "%s [y/N] ", question)
	if flusher, ok := p.Out.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
	return (commands.TerminalPrompt{In: p.In, Out: io.Discard}).Confirm(ctx, question)
}

var _ commands.Prompter = LoginTerminal{}
