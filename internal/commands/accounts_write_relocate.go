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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"slices"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// OrganizationProfileSource fetches a profile document without requiring an
// account or email: relocation only needs the organization the server names.
type OrganizationProfileSource interface {
	ProfileDocument(ctx context.Context, credentials *claude.Credentials) (jsontext.Value, error)
}

// Relocate moves an owned unknown-organization namespace after rereading it
// under both namespace locks. A changed source or occupied target is refused.
func (a *Accounts) Relocate(ctx context.Context, opts cli.ClaudeAccountsRelocateOptions, prompt Prompter, profile OrganizationProfileSource) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	found, err := registry.ResolveID(opts.ID)
	if err != nil {
		return err
	}
	record := *found
	spelling := record.AccountUUID + "/" + record.OrganizationUUID
	if record.Kind.Owned == nil {
		return errs.NewConfig(fmt.Sprintf("`%s` is not a namespace agentctl created, so there is nothing to move", spelling))
	}
	if record.OrganizationUUID != config.UnknownOrg {
		return errs.NewConfig(fmt.Sprintf("`%s` already names an organization; `relocate` only moves a namespace created as `%s`", spelling, config.UnknownOrg))
	}
	source := a.Paths.NamespaceDir(record.AccountUUID, config.UnknownOrg)
	probe, err := readRelocationNamespace(source)
	if err != nil {
		return err
	}
	if probe == nil {
		return errs.NewConfig(fmt.Sprintf("`%s` holds no credentials; run `agentctl claude login` instead", source))
	}
	org, name, err := relocationOrganization(ctx, probe, profile)
	if err != nil {
		return err
	}
	if err := config.ValidateSegment(org); err != nil {
		return err
	}
	target := a.Paths.NamespaceDir(record.AccountUUID, org)
	if !opts.Yes {
		if err := prompt.Tell(fmt.Sprintf("This moves `%s` to `%s` and updates the record to name organization %s.", source, target, org)); err != nil {
			return err
		}
		yes, err := prompt.Confirm(ctx, fmt.Sprintf("Relocate `%s`?", spelling))
		if err != nil {
			return err
		}
		if !yes {
			return errs.NewRefused(0, "cancelled; nothing was moved")
		}
	}
	// Network calls and confirmation happen before either lock. Always acquire
	// source then target, and never persist the potentially superseded probe.
	sourceGuard, err := a.lockNamespace(ctx, record.AccountUUID, config.UnknownOrg)
	if err != nil {
		return err
	}
	defer func() { _ = sourceGuard.Release() }()
	targetGuard, err := a.lockNamespace(ctx, record.AccountUUID, org)
	if err != nil {
		return err
	}
	defer func() { _ = targetGuard.Release() }()
	current, err := readRelocationNamespace(source)
	if err != nil {
		return err
	}
	changed := func() error {
		return errs.NewConfig(fmt.Sprintf("`%s` changed during relocate; nothing was moved. Something refreshed or removed the credential while this command was deciding — re-run `agentctl claude accounts relocate` and it will work from what is there now.", source))
	}
	if current == nil {
		return changed()
	}
	before, err := probe.Digests()
	if err != nil {
		return err
	}
	after, err := current.Digests()
	if err != nil {
		return err
	}
	if before != after {
		return changed()
	}
	existing, err := readRelocationNamespace(target)
	if err != nil {
		return err
	}
	resumed := false
	occupied := func() error {
		return errs.NewConfig(fmt.Sprintf("`%s` already exists and holds a different credential; move or remove it before relocating `%s` into it", target, spelling))
	}
	if existing != nil {
		digests, err := existing.Digests()
		if err != nil {
			return err
		}
		if digests != after {
			return occupied()
		}
		resumed = true
	} else if _, err := os.Lstat(target); err == nil {
		return occupied()
	}
	if err := ClearStaleFiles(ctx, a.Paths, source); err != nil {
		return err
	}
	if !resumed {
		blob, err := current.BlobJSON()
		if err != nil {
			return err
		}
		defer memguard.WipeBytes(blob)
		writeCtx, cancel := context.WithTimeout(ctx, secret.NamespaceLockWait)
		_, err = secret.WriteCredentials(writeCtx, &secret.WriteRequest{Paths: a.Paths, NSDir: target, BlobJSON: blob, NewExpiresAtMS: current.ExpiresAtMillis})
		cancel()
		if err != nil {
			return errs.NewConfig(fmt.Sprintf("could not write `%s`: %v", target, err))
		}
	}
	// Save the registry before removing the source. A crash may leave a stray
	// source directory, but never a registry entry pointing at a deleted one.
	export := claude.ExportSpelling(target)
	moved := record
	moved.OrganizationUUID = org
	if name != nil {
		moved.OrgName = name
	}
	moved.Kind = config.AccountKindOwned(export, claude.SHA8(export))
	if err := config.UpdateRegistry(ctx, a.Paths, func(registry *config.Registry) {
		registry.Accounts = slices.DeleteFunc(registry.Accounts, func(r config.AccountRecord) bool {
			return r.AccountUUID == record.AccountUUID && r.OrganizationUUID == config.UnknownOrg
		})
		registry.Upsert(moved)
	}); err != nil {
		return err
	}
	if err := secret.RemoveNamespace(a.Paths, source); err != nil {
		return errs.NewIO(fmt.Sprintf("could not remove `%s`", source), err)
	}
	if err := targetGuard.Release(); err != nil {
		return err
	}
	if err := sourceGuard.Release(); err != nil {
		return err
	}
	line := fmt.Sprintf("Relocated `%s` to `%s/%s`.", spelling, record.AccountUUID, org)
	if resumed {
		line += " The credential was already in place from an earlier run; this finished the move."
	}
	return prompt.Tell(line)
}

func readRelocationNamespace(dir string) (*claude.Credentials, error) {
	read, err := secret.ReadCredentials(dir)
	if err != nil {
		return nil, errs.NewConfig(fmt.Sprintf("`%s` could not be read: %v", dir, err))
	}
	if !read.Present {
		return nil, nil
	}
	defer memguard.WipeBytes(read.Bytes)
	credentials, err := claude.ParseBlob(read.Bytes)
	if err != nil {
		return nil, errs.NewConfig(fmt.Sprintf("`%s` could not be read: %v", dir, err))
	}
	return credentials, nil
}

func relocationOrganization(ctx context.Context, credentials *claude.Credentials, profile OrganizationProfileSource) (string, *string, error) {
	if identity := credentials.Identity(); identity != nil && identity.OrganizationUUID != nil {
		return *identity.OrganizationUUID, identity.OrgName, nil
	}
	if profile == nil {
		return "", nil, errs.NewConfig("the stored credential names no organization and no profile lookup is available")
	}
	document, err := profile.ProfileDocument(ctx, credentials)
	if err != nil {
		return "", nil, errs.NewConfig(fmt.Sprintf("the account profile could not be read: %v", err))
	}
	defer memguard.WipeBytes(document)
	var response map[string]jsontext.Value
	if document.Kind() == '{' {
		if err := json.Unmarshal(document, &response, jsontext.AllowDuplicateNames(true)); err != nil {
			return "", nil, errs.NewConfig("the account profile could not be read: invalid JSON")
		}
	}
	var organization map[string]jsontext.Value
	if raw := response["organization"]; raw.Kind() == '{' {
		if err := json.Unmarshal(raw, &organization, jsontext.AllowDuplicateNames(true)); err != nil {
			return "", nil, errs.NewConfig("the account profile could not be read: invalid organization")
		}
	}
	var uuid string
	if raw := organization["uuid"]; raw.Kind() == '"' {
		if err := json.Unmarshal(raw, &uuid); err != nil {
			return "", nil, err
		}
	} else {
		return "", nil, errs.NewConfig("neither the stored credential nor the account profile names an organization, so there is nowhere to relocate this namespace to")
	}
	var name *string
	if raw := organization["name"]; raw.Kind() == '"' {
		if err := json.Unmarshal(raw, &name); err != nil {
			return "", nil, err
		}
	}
	return uuid, name, nil
}
