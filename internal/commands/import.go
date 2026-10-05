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
	"io"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// ImportDeadline bounds all keychain reads in one import.
const ImportDeadline = 60 * time.Second

// Import records existing keychain accounts without modifying their credentials.
type Import struct {
	Paths  *config.Paths
	Reader secret.Reader
	Env    *claude.EnvView
	Out    io.Writer
}

// Run discovers accounts and writes only new registry records unless DryRun is set.
// Source failures and registry failures are fatal configuration or I/O errors.
func (im *Import) Run(ctx context.Context, opts cli.ClaudeImportOptions) error {
	ctx, cancel := context.WithTimeout(ctx, ImportDeadline)
	defer cancel()
	existing, err := config.LoadRegistry(ctx, im.Paths)
	if err != nil {
		return err
	}
	if opts.From != cli.ImportSourceKeychain {
		return errs.NewConfig("unsupported import source")
	}
	status := im.Reader.Preflight(ctx)
	switch status.State {
	case secret.KeychainStateUnlocked:
	case secret.KeychainStateLocked:
		return errs.NewConfig("the keychain is locked, so there is nothing to import from; unlock it and run this again")
	case secret.KeychainStateTimeout:
		return errs.NewConfig("the keychain did not answer in time; nothing was imported")
	case secret.KeychainStateUnavailable:
		return errs.NewConfig(fmt.Sprintf("the keychain is not available (%s); nothing was imported", status.Reason))
	default:
		return errs.NewConfig("unsupported on this platform")
	}
	listing, err := im.Reader.ListServices(ctx, claude.LiveService)
	if err != nil {
		return errs.NewConfig(fmt.Sprintf("could not list keychain services: %v", err))
	}
	plan := config.PlanKeychainImport(ctx, importItems(opts.ClaudeConfigDirs, listing, im.Env), claude.ServiceName(im.Env), existing, im.identity)
	lines := plan.Lines()
	if opts.DryRun {
		lines = append(lines, "--dry-run: nothing was written")
	} else if records := plan.Records(); len(records) > 0 {
		if err := config.UpdateRegistry(ctx, im.Paths, func(registry *config.Registry) {
			for _, record := range records {
				registry.Upsert(record)
			}
		}); err != nil {
			return err
		}
	}
	return tell(im.Out, strings.Join(lines, "\n"))
}

func (im *Import) identity(ctx context.Context, service string) *config.ImportIdentity {
	blob, err := im.Reader.Read(ctx, service)
	if err != nil {
		return nil
	}
	var identity *config.ImportIdentity
	if err := blob.WithPlaintext(func(data []byte) error {
		credentials, err := claude.ParseBlob(data)
		if err != nil {
			return err
		}
		if found := credentials.Identity(); found != nil {
			identity = &config.ImportIdentity{AccountUUID: found.AccountUUID, OrganizationUUID: found.OrganizationUUID, Email: found.Email, OrgName: found.OrgName}
		}
		return nil
	}); err != nil {
		return nil
	}
	return identity
}

func importItems(dirs []string, listing []secret.ServiceEntry, env *claude.EnvView) []config.ImportItem {
	liveDir := claude.LiveStoreDir(env)
	canonical, canonicalErr := claude.Canonical(liveDir)
	var items []config.ImportItem
	if len(dirs) == 0 {
		hashes := map[string]bool{claude.SHA8(claude.ExportSpelling(liveDir)): true}
		if canonicalErr == nil {
			hashes[claude.SHA8(claude.ExportSpelling(canonical))] = true
		}
		for _, entry := range listing {
			kind, ok := claude.Classify(entry.Service)
			if !ok || kind.Live {
				continue
			}
			items = append(items, config.ImportItem{Service: entry.Service, Listed: true, SharesLiveDir: hashes[kind.Suffix]})
		}
		return items
	}
	listed := make(map[string]bool, len(listing))
	for _, entry := range listing {
		listed[entry.Service] = true
	}
	for _, dir := range dirs {
		view := claude.EnvView{ConfigDir: &dir, Home: env.Home}
		service := claude.ServiceName(&view)
		resolved, err := claude.Canonical(dir)
		items = append(items, config.ImportItem{Service: service, Dir: &dir, Listed: listed[service], Unsuffixed: service == claude.LiveService, SharesLiveDir: canonicalErr == nil && err == nil && resolved == canonical})
	}
	return items
}
