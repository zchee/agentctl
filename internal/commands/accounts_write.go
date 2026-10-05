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
	"slices"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// Remove deletes an owned account's record and, only on explicit consent, its
// filesystem namespace. Read-only and foreign records are never removed.
func (a *Accounts) Remove(ctx context.Context, opts cli.ClaudeAccountsRemoveOptions, prompt Prompter) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	found, err := registry.ResolveID(opts.ID)
	if err != nil {
		return err
	}
	record := *found
	id := record.AccountUUID + "/" + record.OrganizationUUID
	switch {
	case record.Kind.Owned != nil:
	case record.Kind.Foreign != nil:
		return errs.NewConfig(fmt.Sprintf("`%s` belongs to %s; agentctl never reads or writes it, so there is nothing to remove", id, record.Kind.Foreign.Source))
	default:
		named := "`" + id + "`"
		if record.Kind.ConfigDirReadOnly != nil {
			named = fmt.Sprintf("keychain service `%s`", record.Kind.ConfigDirReadOnly.Service)
		}
		return errs.NewConfig(fmt.Sprintf("%s is a read-only row (kind `%s`): its credentials live in the login keychain, which agentctl never writes or deletes. Use `agentctl claude accounts forget` to stop reporting it, or remove the item with Keychain Access.", named, record.Kind.Name()))
	}
	if opts.DeleteSecret {
		nsDir := a.Paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
		if !opts.Yes {
			if err := prompt.Tell(fmt.Sprintf("This deletes `%s`, including any pending write and any leftover temporary file.\nThe refresh token in it is the only copy agentctl holds: logging in again is the only way back. Nothing in the login keychain is touched.", nsDir)); err != nil {
				return err
			}
			yes, err := prompt.Confirm(ctx, fmt.Sprintf("Delete the stored credentials for `%s`?", id))
			if err != nil {
				return err
			}
			if !yes {
				return errs.NewRefused(0, "cancelled; nothing was removed")
			}
		}
		guard, err := a.lockNamespace(ctx, record.AccountUUID, record.OrganizationUUID)
		if err != nil {
			return err
		}
		err = secret.RemoveNamespace(a.Paths, nsDir)
		_ = guard.Release()
		if err != nil {
			return errs.NewIO(fmt.Sprintf("could not remove `%s`: %v", nsDir, err), err)
		}
	}
	if err := config.UpdateRegistry(ctx, a.Paths, func(registry *config.Registry) {
		registry.Accounts = slices.DeleteFunc(registry.Accounts, func(r config.AccountRecord) bool {
			return r.AccountUUID == record.AccountUUID && r.OrganizationUUID == record.OrganizationUUID
		})
	}); err != nil {
		return err
	}
	if opts.DeleteSecret {
		return prompt.Tell(fmt.Sprintf("Removed `%s` and deleted its stored credentials.", id))
	}
	return prompt.Tell(fmt.Sprintf("Removed the record for `%s`. Its stored credentials are still on disk; re-run with `--delete-secret` to delete them too.", id))
}

func (a *Accounts) lockNamespace(ctx context.Context, account, org string) (*secret.LockGuard, error) {
	if err := config.ValidateSegment(account); err != nil {
		return nil, err
	}
	if err := config.ValidateSegment(org); err != nil {
		return nil, err
	}
	guard, err := secret.Acquire(ctx, a.Paths.LocksDir(), account+"."+org+".lock", time.Now().Add(secret.NamespaceLockWait))
	if err != nil {
		return nil, errs.NewRefused(0, fmt.Sprintf("could not take the namespace lock for `%s/%s`: %v", account, org, err))
	}
	return guard, nil
}

// Forget changes only registry visibility. hide=false restores reporting.
// The live service cannot be hidden and foreign items are always refused.
func (a *Accounts) Forget(ctx context.Context, service string, hide bool) error {
	if hide && service == claude.ServiceName(a.Env) {
		return errs.NewConfig(fmt.Sprintf("`%s` is the credential Claude Code is using right now; hiding it would hide the live row", service))
	}
	if _, ok := claude.Classify(service); !ok {
		whose := fmt.Sprintf("`%s` is not an item agentctl reports — only `%s` and `%s-<8 hex>` are", service, claude.LiveService, claude.LiveService)
		if strings.HasPrefix(service, "claude-switcher:") {
			whose = fmt.Sprintf("`%s` belongs to claude-switcher", service)
		}
		return errs.NewConfig(whose + "; agentctl never reads or writes it, so there is nothing to hide or report")
	}
	changed := false
	if err := config.UpdateRegistry(ctx, a.Paths, func(registry *config.Registry) {
		for i := range registry.Accounts {
			record := &registry.Accounts[i]
			if record.Kind.ConfigDirReadOnly != nil && record.Kind.ConfigDirReadOnly.Service == service {
				changed = record.Forgotten != hide
				record.Forgotten = hide
				return
			}
		}
		known := slices.Contains(registry.ForgottenServices, service)
		switch {
		case hide && !known:
			registry.ForgottenServices = append(registry.ForgottenServices, service)
			changed = true
		case !hide && known:
			registry.ForgottenServices = slices.DeleteFunc(registry.ForgottenServices, func(s string) bool { return s == service })
			changed = true
		}
	}); err != nil {
		return err
	}
	var line string
	switch {
	case hide && changed:
		line = fmt.Sprintf("`%s` is hidden from `status` and `doctor`; `accounts list --all` still shows it. The keychain was not touched.", service)
	case hide:
		line = fmt.Sprintf("`%s` was already hidden.", service)
	case changed:
		line = fmt.Sprintf("`%s` is reported again.", service)
	default:
		line = fmt.Sprintf("`%s` was not hidden.", service)
	}
	return tell(a.Out, line)
}
