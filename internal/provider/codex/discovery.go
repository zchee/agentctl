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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/config"
)

// ScratchPrefix names isolated login homes in the owned scratch tree.
const ScratchPrefix = "agctl-codex-login-"

// ScratchStaleAfter includes the login deadline and a cleanup allowance.
const ScratchStaleAfter = 15 * time.Minute

// SourceKind distinguishes read-only homes from owned namespaces.
type SourceKind string

const (
	// SourceLive is the caller's live Codex home.
	SourceLive SourceKind = "live"
	// SourceHomeReadOnly is an imported home that must never be written.
	SourceHomeReadOnly SourceKind = "home_readonly"
	// SourceOwned is a validated registry-owned namespace.
	SourceOwned SourceKind = "owned"
	// SourceInvalidOwned is an editable registry row with unsafe ids.
	SourceInvalidOwned SourceKind = "invalid_owned"
)

// Source tells a pass where to read, without reading any credential itself.
type Source struct {
	Kind     SourceKind
	Home     string
	Record   *config.CodexAccountRecord
	Owned    *OwnedRecord
	Evidence Daemon
}

// Discover returns registry sources even when live-home resolution fails.
// The returned error applies only to the live row.
func Discover(ctx context.Context, paths *config.Paths, accounts []config.CodexAccountRecord, env Env) ([]Source, error) {
	var sources []Source
	home, liveError := ResolveHome(env)
	if liveError == nil {
		sources = append(sources, Source{Kind: SourceLive, Home: home.Dir})
	}
	for i := range accounts {
		record := &accounts[i]
		if record.Forgotten || record.Kind.Live {
			continue
		}
		if record.Kind.HomeReadOnly != nil {
			sources = append(sources, Source{Kind: SourceHomeReadOnly, Home: record.Kind.HomeReadOnly.Dir, Record: record})
			continue
		}
		if owned := Owned(record); owned != nil {
			dir, err := paths.CodexNamespaceDir(owned.User(), owned.Account())
			if err != nil {
				return sources, err
			}
			sources = append(sources, Source{Kind: SourceOwned, Home: dir, Record: record, Owned: owned, Evidence: DaemonEvidence(ctx, dir)})
		} else {
			sources = append(sources, Source{Kind: SourceInvalidOwned, Record: record})
		}
	}
	return sources, liveError
}

// OrphanKind describes filesystem evidence with no corresponding owned record.
type OrphanKind string

const (
	// OrphanNamespace is a directory unclaimed by the registry.
	OrphanNamespace OrphanKind = "namespace_without_record"
	// OrphanRecord is an owned record without a credential leaf.
	OrphanRecord OrphanKind = "record_without_credentials"
	// OrphanScratch is an isolated login directory past its deadline.
	OrphanScratch OrphanKind = "stale_scratch"
)

// Orphan reports names only; none of these names authorize a write.
type Orphan struct {
	Kind    OrphanKind
	User    string
	Account string
	Name    string
	Age     time.Duration
}

// Orphans lists real directories and never follows directory symlinks.
func Orphans(ctx context.Context, paths *config.Paths, accounts []config.CodexAccountRecord, now time.Time) ([]Orphan, error) {
	owned := make(map[[2]string]bool)
	var order [][2]string
	for i := range accounts {
		if record := Owned(&accounts[i]); record != nil {
			pair := [2]string{record.User(), record.Account()}
			owned[pair] = true
			order = append(order, pair)
		}
	}
	var found []Orphan
	users, err := directories(ctx, paths.CodexRoot())
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		if strings.HasPrefix(user, ".") {
			continue
		}
		accounts, err := directories(ctx, filepath.Join(paths.CodexRoot(), user))
		if err != nil {
			return nil, err
		}
		for _, account := range accounts {
			if !owned[[2]string{user, account}] {
				found = append(found, Orphan{Kind: OrphanNamespace, User: user, Account: account})
			}
		}
	}
	for _, pair := range order {
		dir, err := paths.CodexNamespaceDir(pair[0], pair[1])
		if err != nil {
			return nil, err
		}
		_, err = os.Lstat(filepath.Join(dir, ShownName()))
		if errors.Is(err, os.ErrNotExist) {
			found = append(found, Orphan{Kind: OrphanRecord, User: pair[0], Account: pair[1]})
		} else if err != nil {
			return nil, err
		}
	}
	scratch, err := directories(ctx, paths.CodexScratchRoot())
	if err != nil {
		return nil, err
	}
	for _, name := range scratch {
		if !strings.HasPrefix(name, ScratchPrefix) {
			continue
		}
		info, err := os.Lstat(filepath.Join(paths.CodexScratchRoot(), name))
		if err != nil {
			return nil, err
		}
		age := max(now.Sub(info.ModTime()), 0)
		if age > ScratchStaleAfter {
			found = append(found, Orphan{Kind: OrphanScratch, Name: name, Age: age})
		}
	}
	return found, nil
}

func directories(ctx context.Context, dir string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
