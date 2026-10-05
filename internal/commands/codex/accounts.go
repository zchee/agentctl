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
	"io"
	"strings"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

// Accounts manages only the Codex rows in the account registry.
type Accounts struct {
	Paths *config.Paths
	Out   io.Writer
}

// AccountKey returns the canonical, unambiguous spelling of a registry row.
func AccountKey(row *config.CodexAccountRecord) string {
	return row.ChatGPTUserID + "/" + row.ChatGPTAccountID
}

// ResolveAccount resolves a pair, user, account, email or label within Codex rows.
// An unknown or ambiguous selector returns a configuration error.
func ResolveAccount(rows []config.CodexAccountRecord, id string) (*config.CodexAccountRecord, error) {
	for i := range rows {
		if AccountKey(&rows[i]) == id {
			return &rows[i], nil
		}
	}
	matches := make([]*config.CodexAccountRecord, 0, 1)
	for i := range rows {
		row := &rows[i]
		if row.ChatGPTUserID == id || row.ChatGPTAccountID == id || row.Email != nil && *row.Email == id || row.Label != nil && *row.Label == id {
			matches = append(matches, row)
		}
	}
	switch len(matches) {
	case 0:
		return nil, errs.NewConfig(fmt.Sprintf("no account matches `%s`; `agentctl codex accounts list --all` shows every row", id))
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, row := range matches {
			names[i] = AccountKey(row)
		}
		return nil, errs.NewConfig(fmt.Sprintf("`%s` matches %d rows; use one of: %s", id, len(matches), strings.Join(names, ", ")))
	}
}

// List prints the recorded accounts, optionally including forgotten rows.
func (a *Accounts) List(ctx context.Context, opts cli.CodexAccountsListOptions) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	shown := 0
	var text strings.Builder
	for i := range registry.CodexAccounts {
		row := &registry.CodexAccounts[i]
		if row.Forgotten && !opts.All {
			continue
		}
		shown++
		suffix := ""
		if row.Forgotten {
			suffix = "  (forgotten)"
		}
		fmt.Fprintf(&text, "%s  %s  %s%s\n", AccountKey(row), accountKind(row.Kind), valueOrDash(row.Email), suffix)
	}
	if shown == 0 {
		if len(registry.CodexAccounts) == 0 {
			text.WriteString("no Codex accounts; `agentctl codex login` adds one\n")
		} else {
			fmt.Fprintf(&text, "no Codex accounts shown; %d forgotten (`--all` shows them)\n", len(registry.CodexAccounts))
		}
	}
	_, err = io.WriteString(a.Out, text.String())
	return err
}

// Show prints one recorded account without reading its credential.
func (a *Accounts) Show(ctx context.Context, opts cli.CodexAccountsShowOptions) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	row, err := ResolveAccount(registry.CodexAccounts, opts.ID)
	if err != nil {
		return err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "id:       %s\nuser:     %s\naccount:  %s\nemail:    %s\nplan:     %s\nlabel:    %s\nkind:     %s\n", AccountKey(row), row.ChatGPTUserID, row.ChatGPTAccountID, valueOrDash(row.Email), valueOrDash(row.PlanType), valueOrDash(row.Label), accountKind(row.Kind))
	switch {
	case row.Kind.Owned != nil:
		policy := "auto (agentctl may refresh this grant)"
		if row.Kind.Owned.Refresh == config.RefreshNever {
			policy = "never (agentctl never sends this refresh token)"
		}
		fmt.Fprintf(&text, "store:    %s\nrefresh:  %s\n", row.Kind.Owned.ExportSpelling, policy)
	case row.Kind.Live:
		text.WriteString("store:    the Codex home this environment names (read-only)\n")
	case row.Kind.HomeReadOnly != nil:
		fmt.Fprintf(&text, "store:    %s (read-only)\n", row.Kind.HomeReadOnly.Dir)
	}
	fmt.Fprintf(&text, "created:  %s\n", row.CreatedAt)
	if row.Forgotten {
		text.WriteString("hidden:   yes (`agentctl codex accounts unforget` shows it again)\n")
	}
	_, err = io.WriteString(a.Out, text.String())
	return err
}

// Forget changes a row's visibility without touching its namespace.
func (a *Accounts) Forget(ctx context.Context, id string, hide bool) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	row, err := ResolveAccount(registry.CodexAccounts, id)
	if err != nil {
		return err
	}
	shown := AccountKey(row)
	if row.Forgotten == hide {
		state := "shown"
		if hide {
			state = "forgotten"
		}
		_, err = fmt.Fprintf(a.Out, "%s is already %s\n", shown, state)
		return err
	}
	user, account := row.ChatGPTUserID, row.ChatGPTAccountID
	if err := config.UpdateRegistry(ctx, a.Paths, func(registry *config.Registry) {
		if row := findAccount(registry, user, account); row != nil {
			row.Forgotten = hide
		}
	}); err != nil {
		return err
	}
	state := "shown again"
	if hide {
		state = "hidden from reports"
	}
	_, err = fmt.Fprintf(a.Out, "%s is now %s\n", shown, state)
	return err
}

// Set changes refresh policy in the registry, without opening a namespace.
func (a *Accounts) Set(ctx context.Context, opts cli.CodexAccountsSetOptions) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	row, err := ResolveAccount(registry.CodexAccounts, opts.ID)
	if err != nil {
		return err
	}
	shown := AccountKey(row)
	policy := config.RefreshPolicy(opts.Refresh)
	if policy != config.RefreshAuto && policy != config.RefreshNever {
		return errs.NewConfig("refresh policy must be auto or never")
	}
	if row.Kind.Owned == nil {
		return errs.NewRefused(0, fmt.Sprintf("agentctl never refreshes `%s`: its credential belongs to a Codex home agentctl did not create, and only the account's own client refreshes it", shown))
	}
	if row.Kind.Owned.Refresh == policy {
		_, err = fmt.Fprintf(a.Out, "%s is already `%s`\n", shown, policy)
		return err
	}
	user, account := row.ChatGPTUserID, row.ChatGPTAccountID
	if err := config.UpdateRegistry(ctx, a.Paths, func(registry *config.Registry) {
		if row := findAccount(registry, user, account); row != nil && row.Kind.Owned != nil {
			row.Kind.Owned.Refresh = policy
		}
	}); err != nil {
		return err
	}
	message := shown + ": agentctl may refresh this grant again when its access token expires"
	if policy == config.RefreshNever {
		message = shown + ": agentctl will never send this refresh token; the row reads `expired (run agentctl codex login)` once its access token runs out"
	}
	_, err = fmt.Fprintln(a.Out, message)
	return err
}

func findAccount(registry *config.Registry, user, account string) *config.CodexAccountRecord {
	for i := range registry.CodexAccounts {
		row := &registry.CodexAccounts[i]
		if row.ChatGPTUserID == user && row.ChatGPTAccountID == account {
			return row
		}
	}
	return nil
}

func valueOrDash(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}

func accountKind(kind config.CodexKind) string {
	switch {
	case kind.Owned != nil:
		return "owned"
	case kind.Live:
		return "live"
	case kind.HomeReadOnly != nil:
		return "read-only home"
	default:
		return ""
	}
}
