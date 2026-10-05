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
	"path/filepath"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

type useReversal struct {
	live      bool
	store     *config.AccountRecord
	inherited string
	owner     config.AccountRecord
	source    useSource
	undone    *useUndoneEntry
}

func namespacedReversal(ctx context.Context, paths *config.Paths, registry *config.Registry, entry secret.Undoable) (useReversal, error) {
	var store *config.AccountRecord
	for i := range registry.Accounts {
		record := &registry.Accounts[i]
		owned := record.Kind.Owned
		if owned == nil || owned.ExportSHA8 != entry.SHA8 {
			continue
		}
		kind, valid := claude.Classify(claude.LiveService + "-" + owned.ExportSHA8)
		if !valid || kind.Live || kind.Suffix != owned.ExportSHA8 {
			continue
		}
		if !paths.IsUnderNamespaceRoot(paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)) {
			continue
		}
		store = record
		break
	}
	if store == nil {
		return useReversal{}, errs.NewConfig(fmt.Sprintf("the swap to undo named the keychain item `%s`, which no account agentctl currently owns still derives; there is nothing to put it back into", entry.SHA8))
	}
	dir := paths.NamespaceDir(store.AccountUUID, store.OrganizationUUID)
	displaced := readUndoCredential(ctx, dir, secret.AdoptedFile)
	if displaced == nil {
		return useReversal{}, errs.NewConfig(fmt.Sprintf("the credential that swap displaced is no longer in `%s`, so there is nothing to put back", filepath.Join(dir, secret.AdoptedFile)))
	}
	digests, err := displaced.Digests()
	if err != nil {
		return useReversal{}, err
	}
	found, _ := secret.Digest8(digests.AccessSHA256)
	if entry.FromDigest8 != nil && found != *entry.FromDigest8 {
		return useReversal{}, errs.NewConfig(fmt.Sprintf("the adopted copy in `%s` holds `%s`, but the swap being undone displaced `%s`; agentctl will not put back a credential it cannot match to that swap", dir, found, *entry.FromDigest8))
	}
	if entry.FromDigest8 == nil && found == entry.ToDigest8 {
		return useReversal{}, errs.NewConfig(fmt.Sprintf("the adopted copy in `%s` holds `%s`, which is the credential that swap wrote rather than the one it displaced; agentctl will not put back a credential it cannot match to that swap", dir, entry.ToDigest8))
	}
	owner := *store
	if identity := displaced.Identity(); identity != nil {
		for _, record := range registry.Accounts {
			if record.AccountUUID == identity.AccountUUID {
				owner = record
				break
			}
		}
	}
	return useReversal{store: store, inherited: store.Kind.Owned.ExportSpelling, owner: owner, source: useSource{kind: useSourceAdopted, dir: dir}}, nil
}

func liveReversal(ctx context.Context, paths *config.Paths, registry *config.Registry, entry secret.Undoable) (useReversal, error) {
	if entry.FromDigest8 == nil {
		return useReversal{}, errs.NewConfig(fmt.Sprintf("the live swap to undo recorded no displaced credential (it wrote `%s`), so agentctl cannot tell which credential to put back", entry.ToDigest8))
	}
	if entry.IncomingIdentity == nil {
		return useReversal{}, errs.NewConfig(fmt.Sprintf("the live swap to undo does not record which account it installed (it wrote `%s`), so agentctl cannot tell whose credential the live item holds and will not guess", entry.ToDigest8))
	}
	var result useReversal
	var foundPath string
	for _, record := range registry.Accounts {
		if record.Kind.Owned == nil {
			continue
		}
		dir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
		var adoptedDigests *claude.Digests
		for _, name := range []string{secret.AdoptedFile, secret.CredentialsFile} {
			credentials := readUndoCredential(ctx, dir, name)
			if credentials == nil {
				continue
			}
			digests, err := credentials.Digests()
			if err != nil {
				return useReversal{}, err
			}
			prefix, _ := secret.Digest8(digests.AccessSHA256)
			if prefix != *entry.FromDigest8 || !claude.SameIdentity(credentials, &record) {
				continue
			}
			if adoptedDigests != nil && *adoptedDigests == digests {
				continue
			}
			path := filepath.Join(dir, name)
			if foundPath != "" {
				return useReversal{}, errs.NewConfig(fmt.Sprintf("the credential that live swap displaced (`%s`) is in both `%s` and `%s`; agentctl will not guess which of them to put back into the live item", *entry.FromDigest8, foundPath, path))
			}
			source := useSource{kind: useSourceOwn}
			if name == secret.AdoptedFile {
				adoptedDigests = &digests
				source = useSource{kind: useSourceAdopted, dir: dir}
			}
			foundPath = path
			result = useReversal{live: true, owner: record, source: source, undone: &useUndoneEntry{installed: claude.Identity{AccountUUID: entry.IncomingIdentity.AccountUUID, OrganizationUUID: entry.IncomingIdentity.OrganizationUUID}}}
		}
	}
	if foundPath == "" {
		return useReversal{}, errs.NewConfig(fmt.Sprintf("the credential that live swap displaced (`%s`) is not in its own account's namespace in any store agentctl owns, so there is nothing to put back; `agentctl claude doctor` reports what each namespace holds", *entry.FromDigest8))
	}
	return result, nil
}

func readUndoCredential(ctx context.Context, dir, name string) *claude.Credentials {
	if ctx.Err() != nil {
		return nil
	}
	read, err := secret.ReadFile(filepath.Join(dir, name), secret.MaxCredentialsBytes)
	if err != nil || !read.Present {
		return nil
	}
	defer memguard.WipeBytes(read.Bytes)
	credentials, _ := claude.ParseBlob(read.Bytes)
	return credentials
}
