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
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
)

// Remove deletes a registry row, optionally deleting only owned credential files.
// Namespace refusals leave the row and its files unchanged.
func (a *Accounts) Remove(ctx context.Context, opts cli.CodexAccountsRemoveOptions, prompt commands.Prompter) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	record, err := ResolveAccount(registry.CodexAccounts, opts.ID)
	if err != nil {
		return err
	}
	shown := AccountKey(record)
	if opts.DeleteSecret {
		switch {
		case record.Kind.Live:
			return errs.NewRefused(0, fmt.Sprintf("`%s` is the credential the Codex home this environment names holds; agentctl never writes or deletes a file under a home it did not create. `agentctl codex accounts forget %s` stops reporting it.", shown, shown))
		case record.Kind.HomeReadOnly != nil:
			return errs.NewRefused(0, fmt.Sprintf("`%s` was imported read-only from `%s`; agentctl never writes or deletes a file under a home it did not create. `agentctl codex accounts forget %s` stops reporting it.", shown, record.Kind.HomeReadOnly.Dir, shown))
		}
		if !opts.Yes {
			yes, err := prompt.Confirm(ctx, fmt.Sprintf("delete the credential agentctl stored for `%s` and forget the account?", shown))
			if err != nil {
				return err
			}
			if !yes {
				_, err = fmt.Fprintln(a.Out, "nothing was removed")
				return err
			}
		}
		if err := a.deleteNamespace(ctx, record, shown); err != nil {
			return err
		}
	}
	user, account := record.ChatGPTUserID, record.ChatGPTAccountID
	if err := config.UpdateRegistry(ctx, a.Paths, func(registry *config.Registry) {
		registry.CodexAccounts = slices.DeleteFunc(registry.CodexAccounts, func(row config.CodexAccountRecord) bool {
			return row.ChatGPTUserID == user && row.ChatGPTAccountID == account
		})
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Out, "%s is no longer recorded\n", shown)
	return err
}

func (a *Accounts) deleteNamespace(ctx context.Context, record *config.CodexAccountRecord, shown string) error {
	owned := codexprovider.Owned(record)
	if owned == nil {
		return errs.NewRefused(0, fmt.Sprintf("`%s` is not an owned account, so agentctl stores no credential for it", shown))
	}
	directory, err := a.Paths.CodexNamespaceDir(record.ChatGPTUserID, record.ChatGPTAccountID)
	if err != nil {
		return err
	}
	guard, err := codexprovider.AcquireCodex(ctx, a.Paths, owned, 5*time.Second)
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	defer func() {
		if err := guard.Release(); err != nil {
			slog.WarnContext(ctx, "the Codex removal lock could not be released", "error", err)
		}
	}()
	if _, err := os.Lstat(directory); errors.Is(err, fs.ErrNotExist) {
		removed, err := codexprovider.RemoveRefreshState(ctx, a.Paths, guard)
		if err != nil {
			return errs.NewRefused(0, err.Error())
		}
		if removed {
			if err := codexprovider.AppendCodexEvent(ctx, a.Paths, owned.User(), owned.Account(), codexprovider.CodexEvent{Outcome: codexprovider.AuditDelete}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.Out, "%s has no stored credential; its leftover refresh marker is gone\n", shown)
		} else {
			_, err = fmt.Fprintf(a.Out, "%s has no stored credential; nothing to delete\n", shown)
		}
		return err
	}
	namespace, err := codexprovider.OpenOwnedNamespace(a.Paths, owned, guard)
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	defer func() {
		if err := namespace.Close(); err != nil {
			slog.WarnContext(ctx, "the removed Codex namespace could not be closed", "error", err)
		}
	}()
	receipt, err := namespace.RemoveNamedFiles()
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	if receipt != nil {
		if err := codexprovider.AppendCodexReceipt(ctx, a.Paths, receipt); err != nil {
			return err
		}
	}
	removed, err := codexprovider.RemoveRefreshState(ctx, a.Paths, guard)
	if err != nil {
		return errs.NewRefused(0, err.Error())
	}
	if receipt == nil && removed {
		if err := codexprovider.AppendCodexEvent(ctx, a.Paths, owned.User(), owned.Account(), codexprovider.CodexEvent{Outcome: codexprovider.AuditDelete}); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(a.Out, "%s: the stored credential and its refresh marker are gone\n", shown)
	return err
}
