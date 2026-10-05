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
	json "encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/usage"
)

func TestStatusResolvesPendingBeforeCacheAndMissingCredential(t *testing.T) {
	tests := map[string]struct {
		cache   bool
		missing bool
		first   bool
		want    string
		calls   int64
	}{
		"success: a fresh cache does not retain pending tokens": {cache: true, want: "pending replayed"},
		"success: a first write repairs a missing credential":   {missing: true, first: true, want: "pending replayed", calls: 1},
		"error: deletion is not undone by a stale pending":      {missing: true, want: "needs login"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			world.seedOwned(t, world.fixture.Blob("old-access", "old-refresh", testutil.FreshAt()))
			paths, _ := refreshRecord(t, world.fixture)
			dir := paths.NamespaceDir(testutil.Acct, testutil.Org)
			current := rereadCredential(dir)
			digests, err := current.Digests()
			if err != nil {
				t.Fatal(err)
			}
			meta := map[string]any{"created_at": time.Now().UTC().Format(time.RFC3339), "new_expires_at": testutil.FreshAt(), "derived_from_access_sha256": digests.AccessSHA256, "derived_from_refresh_sha256": digests.RefreshSHA256}
			if tt.first {
				meta["derived_from_access_sha256"] = nil
				meta["derived_from_refresh_sha256"] = nil
			}
			data, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, secret.PendingMetaFile), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, secret.PendingFile), []byte(world.fixture.Blob("pending-access", "pending-refresh", testutil.FreshAt())), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.missing {
				if err := os.Remove(world.fixture.CredentialsPath(testutil.Acct, testutil.Org)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.cache {
				entry := usage.NewCacheEntry(time.Now().UnixMilli(), usageBody(t))
				if err := usage.StoreCache(t.Context(), usage.CachePath(paths, testutil.Acct, testutil.Org), &entry); err != nil {
					t.Fatal(err)
				}
			}
			stdout, runErr := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly})
			if tt.missing && !tt.first {
				assertPartial(t, runErr, 1)
			} else if runErr != nil {
				t.Fatalf("status: %v\n%s", runErr, stdout)
			}
			if !strings.Contains(stdout, tt.want) || world.calls.Load() != tt.calls {
				t.Fatalf("output = %s; usage calls = %d; want state %s, calls %d", stdout, world.calls.Load(), tt.want, tt.calls)
			}
			for _, name := range []string{secret.PendingFile, secret.PendingMetaFile} {
				if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("pending material remains: %s (%v)", name, err)
				}
			}
			if tt.missing && !tt.first {
				if _, err := os.Lstat(world.fixture.CredentialsPath(testutil.Acct, testutil.Org)); !os.IsNotExist(err) {
					t.Fatalf("removed account restored: %v", err)
				}
			} else if saved := rereadCredential(dir); saved == nil || saved.AuthorizationHeader() != "Bearer pending-access" || saved.AccessExpired(time.Now().UnixMilli(), claude.RefreshMarginMillis) {
				t.Fatal("pending credential was not replayed")
			}
		})
	}
}
