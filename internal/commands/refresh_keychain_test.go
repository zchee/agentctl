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

//go:build agentctl_testing

package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func migratedWorld(t *testing.T) (*statusWorld, *migratedRefreshTarget, *claude.Credentials) {
	t.Helper()
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("old-access", "old-refresh", testutil.ExpiredAt()))
	world.fixture.WithKeychain()
	for _, entry := range world.fixture.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
			t.Setenv(key, value)
		}
	}
	t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
	t.Setenv("AGENTCTL_SECURITY_BIN", world.fixture.SecurityBin())
	t.Setenv("USER", testutil.KeychainAccount)
	paths, record := refreshRecord(t, world.fixture)
	service := secret.ForeignServiceName(record.Kind.Owned.ExportSHA8)
	world.fixture.AllowWrite(service).KeychainItem(service, world.fixture.Blob("old-access", "old-refresh", testutil.ExpiredAt())).Dump(service)
	// AllowWrite changes the fixture environment after the first capture.
	for _, entry := range world.fixture.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") {
			t.Setenv(key, value)
		}
	}
	world.status.Reader = secret.NewReader()
	world.status.Writer = secret.NewKeychainWriter()
	item, note := migratedRefreshItem(paths, record, service, []secret.ServiceEntry{{Service: service, Account: testutil.KeychainAccount}})
	if item == nil {
		t.Fatal(note)
	}
	current, err := world.status.readRefreshItem(t.Context(), service)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(world.fixture.CredentialsPath(testutil.Acct, testutil.Org)); err != nil {
		t.Fatal(err)
	}
	return world, item, current
}

func TestMigratedRefreshPersistsUnderPeerLocks(t *testing.T) {
	world, item, current := migratedWorld(t)
	paths, _ := refreshRecord(t, world.fixture)
	world.status.Refresher = refreshOAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		for _, lock := range []string{secret.RefreshLockName, secret.StorageWriteLockName} {
			if _, err := os.Lstat(filepath.Join(item.nsDir, lock)); !os.IsNotExist(err) {
				t.Errorf("peer lock held during POST: %s (%v)", lock, err)
			}
		}
		_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`))
	})
	result := world.status.refreshMigrated(t.Context(), paths, item, current)
	if result.credentials == nil || result.state != nil || result.lockState != "migrated_refreshed" {
		t.Fatalf("refresh result = %+v", result)
	}
	stored, err := world.status.readRefreshItem(t.Context(), item.service)
	if err != nil || stored.AuthorizationHeader() != "Bearer rotated-access" {
		t.Fatalf("keychain write not applied: %v", err)
	}
	if _, err := os.Lstat(world.fixture.CredentialsPath(testutil.Acct, testutil.Org)); !os.IsNotExist(err) {
		t.Fatalf("plaintext credentials reappeared: %v", err)
	}
	log := strings.Join(world.fixture.SecurityLog(), "\n")
	if !strings.Contains(log, "add-generic-password -U") || strings.Contains(log, "rotated-access") || strings.Contains(log, "delete-generic-password") {
		t.Fatalf("unexpected keychain write log: %s", log)
	}
	audit, err := os.ReadFile(secret.AuditLogPath(paths))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"outcome":"applied"`) {
		t.Fatalf("missing applied audit: %s", audit)
	}
}

func TestMigratedRefreshAdoptsChangedPeerAndDiscardsLaterChanges(t *testing.T) {
	tests := map[string]struct {
		before    bool
		invalid   bool
		wantLock  string
		wantPost  int64
		wantState claude.AccountStateKind
	}{
		"success: a peer refresh before POST is adopted":        {before: true, wantLock: "adopted"},
		"success: invalid grant adopts a peer refresh":          {invalid: true, wantLock: "adopted", wantPost: 1},
		"error: a peer change after POST discards minted grant": {wantLock: "none", wantPost: 1, wantState: claude.StateStale},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world, item, current := migratedWorld(t)
			paths, _ := refreshRecord(t, world.fixture)
			peer := world.fixture.Blob("peer-access", "peer-refresh", testutil.FreshAt())
			writePeer := func() {
				t.Helper()
				if err := os.WriteFile(world.fixture.KeychainItemPath(item.service), []byte(peer), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var posts atomic.Int64
			world.status.Refresher = refreshOAuth(t, func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				writePeer()
				if tt.invalid {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"rotated-access","expires_in":3600}`))
			})
			if tt.before {
				writePeer()
			}
			result := world.status.refreshMigrated(t.Context(), paths, item, current)
			if result.lockState != tt.wantLock || posts.Load() != tt.wantPost {
				t.Fatalf("result = %+v; POST count %d", result, posts.Load())
			}
			if tt.wantState != "" && (result.state == nil || result.state.Kind != tt.wantState) {
				t.Fatalf("state = %v; want %s", result.state, tt.wantState)
			}
			stored, err := world.status.readRefreshItem(t.Context(), item.service)
			if err != nil || stored.AuthorizationHeader() != "Bearer peer-access" {
				t.Fatalf("peer credential was overwritten: %v", err)
			}
			for _, line := range world.fixture.SecurityLog() {
				if strings.HasPrefix(line, "add-generic-password") {
					t.Fatalf("refused refresh wrote keychain: %s", line)
				}
			}
		})
	}
}

func TestStatusMigratedFreshCredentialIsReadWithoutWriting(t *testing.T) {
	world, item, _ := migratedWorld(t)
	blob := world.fixture.Blob("fresh-access", "fresh-refresh", testutil.FreshAt())
	if err := os.WriteFile(world.fixture.KeychainItemPath(item.service), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int64
	world.status.Refresher = refreshOAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	stdout, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly, Refresh: true})
	if err != nil || world.calls.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("status = %v; usage %d, token %d; output %s", err, world.calls.Load(), posts.Load(), stdout)
	}
	for _, line := range world.fixture.SecurityLog() {
		if strings.HasPrefix(line, "add-generic-password") || strings.HasPrefix(line, "delete-generic-password") {
			t.Fatalf("read-only pass wrote keychain: %s", line)
		}
	}
	if _, err := os.Lstat(world.fixture.CredentialsPath(testutil.Acct, testutil.Org)); !os.IsNotExist(err) {
		t.Fatalf("file credential reappeared: %v", err)
	}
}

func TestMigratedRefreshTargetRefusesAmbiguousOrUnownedItems(t *testing.T) {
	world, item, _ := migratedWorld(t)
	paths, record := refreshRecord(t, world.fixture)
	tests := map[string]struct {
		service string
		listing []secret.ServiceEntry
	}{
		"error: live item":       {service: secret.LiveKeychainService},
		"error: another suffix":  {service: secret.ForeignServiceName("deadbeef")},
		"error: duplicate item":  {service: item.service, listing: []secret.ServiceEntry{{Service: item.service}, {Service: item.service}}},
		"error: another account": {service: item.service, listing: []secret.ServiceEntry{{Service: item.service, Account: "other"}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if target, note := migratedRefreshItem(paths, record, tt.service, tt.listing); target != nil || note == "" {
				t.Fatalf("target = %+v; refusal = %q", target, note)
			}
		})
	}
}
