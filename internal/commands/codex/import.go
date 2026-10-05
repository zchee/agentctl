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
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	provider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/secret"
)

// Import records metadata from another home without changing its credentials.
type Import struct {
	Paths  *config.Paths
	Reader secret.Reader
	Env    provider.Env
	Out    io.Writer
}

// Run reads the configured credential source and records a new read-only account.
// Dry runs and already-recorded identities do not write any files.
func (im *Import) Run(ctx context.Context, opts cli.CodexImportOptions) error {
	if opts.From != cli.CodexImportSourceCodexHome {
		return errs.NewConfig("unsupported import source")
	}
	home, err := importHome(opts, im.Env)
	if err != nil {
		return err
	}
	cfg := provider.LoadConfig(ctx, home.Dir)
	lines := []string{}
	if cfg.Note != nil {
		switch {
		case cfg.Note.Kind == "unreadable":
			lines = append(lines, "note: `config.toml` could not be read; assuming the default store")
		case cfg.Note.Line != nil:
			lines = append(lines, fmt.Sprintf("note: `config.toml` is not TOML (line %d); assuming the default store", *cfg.Note.Line))
		default:
			lines = append(lines, "note: `config.toml` is not TOML; assuming the default store")
		}
	}
	read, note := provider.FileInEffect(cfg.Store, func() provider.KeyringProbe {
		listCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if im.Reader == nil {
			return provider.KeyringUnknown
		}
		entries, err := im.Reader.ListServices(listCtx, "Codex Auth")
		if err != nil {
			return provider.KeyringUnknown
		}
		account := provider.KeyringAccount(home.Dir)
		for _, entry := range entries {
			if entry.Service == "Codex Auth" && (entry.Account == "" || entry.Account == account) {
				return provider.KeyringPresent
			}
		}
		return provider.KeyringAbsent
	})
	if !read {
		return errs.NewRefused(0, fmt.Sprintf("`%s` keeps its credentials in `%s`, which agentctl does not read; there is nothing to import from it", home.Dir, cfg.Store.Label()))
	}
	if note != "" {
		lines = append(lines, "note: "+note)
	}
	resolved := provider.ReadAuth(ctx, home.Dir)
	switch resolved.Kind {
	case provider.ResolvedAbsent:
		return errs.NewRefused(0, fmt.Sprintf("`%s` holds no `%s`; log in with `codex` there first", home.Dir, provider.ShownName()))
	case provider.ResolvedTorn:
		return errs.NewRefused(0, fmt.Sprintf("`%s`'s `%s` was being rewritten; run this again", home.Dir, provider.ShownName()))
	case provider.ResolvedTransient:
		return errs.NewRefused(0, resolved.Reason)
	case provider.ResolvedCredentials:
	default:
		return errs.NewRefused(0, "the Codex credential could not be read")
	}
	identity := resolved.Credentials.Identity()
	if identity == nil {
		return errs.NewRefused(0, fmt.Sprintf("`%s`'s credential names no ChatGPT user or account, so there is no account to record", home.Dir))
	}
	existing, err := config.LoadRegistry(ctx, im.Paths)
	if err != nil {
		return err
	}
	who := identity.UserID
	if identity.Email != nil {
		who = *identity.Email
	}
	if importRecorded(existing, identity) {
		lines = append(lines, who+" is already recorded; nothing was changed")
	} else {
		lines = append(lines, fmt.Sprintf("%s: read-only, from `%s`", who, home.Dir))
		if opts.DryRun {
			lines = append(lines, "--dry-run: nothing was written")
		} else if err := config.UpdateRegistry(ctx, im.Paths, func(registry *config.Registry) { recordImportOnce(registry, identity, home.Dir) }); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(im.Out, strings.Join(lines, "\n"))
	return err
}

func importHome(opts cli.CodexImportOptions, env provider.Env) (provider.Home, error) {
	if opts.CodexHome != "" {
		env = provider.Env{CodexHome: opts.CodexHome}
	}
	home, err := provider.ResolveHome(env)
	if err == nil {
		return home, nil
	}
	if opts.CodexHome == "" {
		return provider.Home{}, errs.NewRefused(0, err.Error())
	}
	reason := "`--codex-home` names nothing"
	if problem, ok := errors.AsType[*provider.HomeError](err); ok {
		switch problem.Kind {
		case "missing":
			reason = fmt.Sprintf("`--codex-home` names `%s`, but that path does not exist", problem.Path)
		case "not_directory":
			reason = fmt.Sprintf("`--codex-home` names `%s`, but that path is not a directory", problem.Path)
		case "unreadable":
			reason = fmt.Sprintf("could not read `--codex-home` `%s`: %s", problem.Path, problem.Reason)
		}
	}
	return provider.Home{}, errs.NewRefused(0, "codex home unreadable: "+reason)
}

func importRecorded(registry *config.Registry, identity *provider.Identity) bool {
	for _, record := range registry.CodexAccounts {
		if record.ChatGPTUserID == identity.UserID && record.ChatGPTAccountID == identity.AccountID {
			return true
		}
	}
	return false
}

func recordImportOnce(registry *config.Registry, identity *provider.Identity, dir string) {
	if importRecorded(registry, identity) {
		return
	}
	registry.CodexAccounts = append(registry.CodexAccounts, config.CodexAccountRecord{ChatGPTUserID: identity.UserID, ChatGPTAccountID: identity.AccountID, Email: identity.Email, PlanType: identity.Plan, Kind: config.CodexKindHomeReadOnly(dir), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}
