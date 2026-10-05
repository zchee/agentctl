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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

// fakeKeychain is an in-memory keychain for in-process tests: the same
// scripted answers the fake security(1) gives the end-to-end scripts,
// without a child process. Reads are answered from the item map and a
// missing item is the not-found error, exactly as the real transport
// classifies it.
type fakeKeychain struct {
	state secret.KeychainState
	items map[string][]byte
}

// unlockedKeychain returns a readable keychain holding these items.
func unlockedKeychain(items map[string][]byte) *fakeKeychain {
	return &fakeKeychain{state: secret.KeychainStateUnlocked, items: items}
}

func (f *fakeKeychain) Preflight(context.Context) secret.KeychainStatus {
	return secret.KeychainStatus{State: f.state}
}

func (f *fakeKeychain) ListServices(_ context.Context, prefix string) ([]secret.ServiceEntry, error) {
	var entries []secret.ServiceEntry
	for service := range f.items {
		if strings.HasPrefix(service, prefix) {
			entries = append(entries, secret.ServiceEntry{Service: service, Account: "tester"})
		}
	}
	return entries, nil
}

func (f *fakeKeychain) Read(_ context.Context, service string) (*secret.Secret, error) {
	blob, ok := f.items[service]
	if !ok {
		return nil, secret.ErrItemNotFound
	}
	return secret.NewSecret(blob)
}

// seededStore is one temporary store with its paths and environment.
type seededStore struct {
	fixture *testutil.Fixture
	paths   *config.Paths
	env     claude.EnvView
}

func seedStore(t *testing.T) *seededStore {
	t.Helper()
	fixture := testutil.New(t)
	return &seededStore{
		fixture: fixture,
		paths:   config.NewPaths(fixture.ConfigDir()),
		env:     claude.EnvWithHome(fixture.Home()),
	}
}

// accounts returns the command value under test, capturing output in buf.
func (s *seededStore) accounts(reader secret.Reader, buf *bytes.Buffer) *Accounts {
	return &Accounts{Paths: s.paths, Env: &s.env, Reader: reader, Out: buf}
}

func TestAccountsList(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		seed     func(t *testing.T, store *seededStore) (*fakeKeychain, []string)
		all      bool
		contains []string
		absent   []string
	}{
		"success: shows the kind, the source and the export spelling": {
			seed: func(t *testing.T, store *seededStore) (*fakeKeychain, []string) {
				t.Helper()
				store.fixture.WriteRegistry([]any{store.fixture.OwnedRecord(testutil.Acct, testutil.Org)})
				store.fixture.WriteCredentials(testutil.Acct, testutil.Org, store.fixture.Blob("sk-ant-oat01-owned", "sk-ant-ort01-owned", testutil.FreshAt()))
				spelling := testutil.ExportSpelling(store.fixture.NamespaceDir(testutil.Acct, testutil.Org))
				return unlockedKeychain(nil), []string{spelling}
			},
			contains: []string{
				"Id", "Account", "Org", "Kind", "Source", "State", "Location",
				testutil.Email,
				"owned",
				"file",
			},
			absent: []string{"sk-ant-"},
		},
		"success: hides a forgotten row and a foreign item until --all": {
			seed: func(t *testing.T, store *seededStore) (*fakeKeychain, []string) {
				t.Helper()
				store.fixture.WriteRegistryDocument(map[string]any{
					"version":            1,
					"accounts":           []any{},
					"forgotten_services": []string{claude.LiveService + "-6cdd6b98"},
				})
				return unlockedKeychain(map[string][]byte{
					claude.LiveService + "-6cdd6b98":      []byte("{}"),
					"claude-switcher:someone@example.com": []byte("{}"),
				}), nil
			},
			contains: []string{"2 entries hidden (--all)"},
			absent:   []string{"6cdd6b98", "claude-switcher:someone@example.com"},
		},
		"success: --all reveals the hidden rows and drops the footer": {
			seed: func(t *testing.T, store *seededStore) (*fakeKeychain, []string) {
				t.Helper()
				store.fixture.WriteRegistryDocument(map[string]any{
					"version":            1,
					"accounts":           []any{},
					"forgotten_services": []string{claude.LiveService + "-6cdd6b98"},
				})
				return unlockedKeychain(map[string][]byte{
					claude.LiveService + "-6cdd6b98":      []byte("{}"),
					"claude-switcher:someone@example.com": []byte("{}"),
				}), nil
			},
			all:      true,
			contains: []string{claude.LiveService + "-6cdd6b98", "forgotten", "claude-switcher:someone@example.com", "foreign"},
			absent:   []string{"hidden (--all)"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := seedStore(t)
			reader, extra := tt.seed(t, store)

			var buf bytes.Buffer
			if err := store.accounts(reader, &buf).List(t.Context(), tt.all); err != nil {
				t.Fatalf("List: %v", err)
			}
			out := buf.String()
			t.Logf("list output:\n%s", out)
			for _, want := range append(tt.contains, extra...) {
				if !strings.Contains(out, want) {
					t.Errorf("the listing should contain %q", want)
				}
			}
			for _, banned := range tt.absent {
				if strings.Contains(out, banned) {
					t.Errorf("the listing should not contain %q", banned)
				}
			}
		})
	}
}

func TestListRecord(t *testing.T) {
	t.Parallel()

	email := testutil.Email
	orgName := "Acme"
	tests := map[string]struct {
		row  claude.AccountRow
		want []string
	}{
		"success: a full owned row": {
			row: claude.AccountRow{
				ID: testutil.Acct,
				Record: config.AccountRecord{
					AccountUUID:      testutil.Acct,
					OrganizationUUID: testutil.Org,
					Email:            &email,
					OrgName:          &orgName,
					Kind:             config.AccountKindOwned("/store/ns", "deadbeef"),
				},
				State:  claude.StateOfOK(),
				Source: claude.SourceFile,
			},
			want: []string{testutil.Acct, testutil.Email, "Acme", "owned", "file", "ok", "/store/ns"},
		},
		"success: a row with no email and no org name falls back": {
			row: claude.AccountRow{
				ID: "live",
				Record: config.AccountRecord{
					AccountUUID:      testutil.Acct,
					OrganizationUUID: testutil.Org,
					Kind:             config.AccountKindLive(),
				},
				State:  claude.StateOfNeedsLogin(),
				Source: claude.SourceKeychain,
			},
			want: []string{"live", "—", testutil.Org, "live", "keychain", "needs login", "Claude Code-credentials"},
		},
		"success: a foreign row names its owner in the location": {
			row: claude.AccountRow{
				ID: "claude-switcher:someone@example.com",
				Record: config.AccountRecord{
					Kind: config.AccountKindForeign("claude-switcher"),
				},
				State:  claude.StateOfForeign("claude-switcher"),
				Source: claude.SourceKeychain,
			},
			want: []string{"claude-switcher:someone@example.com", "—", "", "foreign", "keychain", "foreign (claude-switcher)", "claude-switcher"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := listRecord(&tt.row, "Claude Code-credentials")
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("listRecord mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveAccountRow(t *testing.T) {
	t.Parallel()

	email := testutil.Email
	label := "work"
	rows := []claude.AccountRow{
		{
			ID: testutil.Acct,
			Record: config.AccountRecord{
				AccountUUID:      testutil.Acct,
				OrganizationUUID: testutil.Org,
				Email:            &email,
				Label:            &label,
			},
		},
		{
			ID: testutil.Acct + "/" + testutil.UnknownOrg,
			Record: config.AccountRecord{
				AccountUUID:      testutil.Acct,
				OrganizationUUID: testutil.UnknownOrg,
				Email:            &email,
			},
		},
	}

	tests := map[string]struct {
		id      string
		wantOrg string
		wantErr []string
	}{
		"success: the key spelling is never ambiguous": {
			id:      testutil.Acct + "/" + testutil.UnknownOrg,
			wantOrg: testutil.UnknownOrg,
		},
		"success: a label matches its one row": {
			id:      "work",
			wantOrg: testutil.Org,
		},
		"error: a shared uuid names both spellings": {
			id: testutil.Acct,
			wantErr: []string{
				"matches 2 rows",
				testutil.Acct + "/" + testutil.Org,
				testutil.Acct + "/" + testutil.UnknownOrg,
			},
		},
		"error: a shared email names both spellings": {
			id:      testutil.Email,
			wantErr: []string{"matches 2 rows"},
		},
		"error: an unknown id points at list --all": {
			id:      "nobody@example.invalid",
			wantErr: []string{"no account matches `nobody@example.invalid`", "agentctl claude accounts list --all"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, err := resolveAccountRow(rows, tt.id)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("resolveAccountRow(%q) should fail", tt.id)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error %q should contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAccountRow(%q): %v", tt.id, err)
			}
			if row.Record.OrganizationUUID != tt.wantOrg {
				t.Errorf("resolved organization = %q, want %q", row.Record.OrganizationUUID, tt.wantOrg)
			}
		})
	}
}

func TestAccountsShow(t *testing.T) {
	t.Parallel()

	service := claude.LiveService + "-6cdd6b98"

	tests := map[string]struct {
		seed     func(t *testing.T, store *seededStore)
		id       string
		contains []string
		absent   []string
		wantErr  []string
	}{
		"success: reports the namespace without reporting a token": {
			seed: func(t *testing.T, store *seededStore) {
				t.Helper()
				store.fixture.WriteRegistry([]any{store.fixture.OwnedRecord(testutil.Acct, testutil.Org)})
				blob := store.fixture.Blob("sk-ant-oat01-owned", "sk-ant-ort01-owned", testutil.FreshAt())
				store.fixture.WriteCredentials(testutil.Acct, testutil.Org, blob)
				nsDir := store.fixture.NamespaceDir(testutil.Acct, testutil.Org)
				if err := os.WriteFile(filepath.Join(nsDir, secret.PendingFile), []byte(blob), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nsDir, secret.CredentialsFile+".tmp.0123abcd"), []byte(blob), 0o600); err != nil {
					t.Fatal(err)
				}
				lockPath := store.fixture.LockPath(testutil.Acct, testutil.Org)
				if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
					t.Fatal(err)
				}
				body := fmt.Sprintf(`{"pid": %d, "pid_start_time": null, "acquired_at": "2026-09-08T00:00:00Z"}`, os.Getpid())
				if err := os.WriteFile(lockPath, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			id: testutil.Acct,
			contains: []string{
				testutil.Email,
				"kind               owned",
				"access expires",
				"pending write",
				".tmp.0123abcd",
				fmt.Sprintf("pid %d (alive), taken 2026-09-08T00:00:00Z", os.Getpid()),
			},
			absent: []string{"sk-ant-"},
		},
		"success: names the service and never passes it off as an account": {
			seed: func(t *testing.T, store *seededStore) {
				t.Helper()
				record := config.AccountRecord{
					AccountUUID:      service,
					OrganizationUUID: testutil.UnknownOrg,
					Kind:             config.AccountKindConfigDirReadOnly("/elsewhere/.claude", service, false),
					CreatedAt:        "2026-09-08T00:00:00Z",
				}
				if err := config.UpdateRegistry(t.Context(), store.paths, func(r *config.Registry) { r.Upsert(record) }); err != nil {
					t.Fatal(err)
				}
			},
			id: service,
			contains: []string{
				"service            " + service,
				"keyed by its service name",
				"kind               config_dir",
			},
			absent: []string{"account            " + service},
		},
		"success: still prints the account uuid of an identified read-only row": {
			seed: func(t *testing.T, store *seededStore) {
				t.Helper()
				store.fixture.WriteRegistry([]any{store.fixture.ConfigDirRecord("99999999-8888-7777-6666-555555555555", "cccccccc-dddd-eeee-ffff-000000000000", service)})
			},
			id: "read-only@example.com",
			contains: []string{
				"account            99999999-8888-7777-6666-555555555555",
				"service            " + service,
			},
			absent: []string{"keyed by its service name"},
		},
		"error: an ambiguous id names the unambiguous spellings": {
			seed: func(t *testing.T, store *seededStore) {
				t.Helper()
				store.fixture.WriteRegistry([]any{
					store.fixture.OwnedRecord(testutil.Acct, testutil.Org),
					store.fixture.OwnedRecord(testutil.Acct, testutil.UnknownOrg),
				})
			},
			id: testutil.Acct,
			wantErr: []string{
				testutil.Acct + "/" + testutil.Org,
				testutil.Acct + "/" + testutil.UnknownOrg,
			},
		},
		"error: the recommended spelling then resolves": {
			seed: func(t *testing.T, store *seededStore) {
				t.Helper()
				store.fixture.WriteRegistry([]any{
					store.fixture.OwnedRecord(testutil.Acct, testutil.Org),
					store.fixture.OwnedRecord(testutil.Acct, testutil.UnknownOrg),
				})
			},
			id:       testutil.Acct + "/" + testutil.UnknownOrg,
			contains: []string{testutil.UnknownOrg},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := seedStore(t)
			tt.seed(t, store)

			var buf bytes.Buffer
			err := store.accounts(unlockedKeychain(nil), &buf).Show(t.Context(), tt.id)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Show(%q) should fail; output:\n%s", tt.id, buf.String())
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error %q should contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Show(%q): %v", tt.id, err)
			}
			out := buf.String()
			t.Logf("show output:\n%s", out)
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("the report should contain %q", want)
				}
			}
			for _, banned := range tt.absent {
				if strings.Contains(out, banned) {
					t.Errorf("the report should not contain %q", banned)
				}
			}
		})
	}
}

func TestLocation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		kind config.AccountKind
		want string
	}{
		"success: an owned row carries its export spelling": {
			kind: config.AccountKindOwned("/store/claude/acct/org", "deadbeef"),
			want: "/store/claude/acct/org",
		},
		"success: a read-only row carries its service": {
			kind: config.AccountKindConfigDirReadOnly("/elsewhere/.claude", "Claude Code-credentials-deadbeef", false),
			want: "Claude Code-credentials-deadbeef",
		},
		"success: the live row carries the live service": {
			kind: config.AccountKindLive(),
			want: "Claude Code-credentials",
		},
		"success: a foreign row carries its owner": {
			kind: config.AccountKindForeign("claude-switcher"),
			want: "claude-switcher",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			record := config.AccountRecord{Kind: tt.kind}
			if got := location(&record, "Claude Code-credentials"); got != tt.want {
				t.Errorf("location = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()

	fresh := time.Now().Add(2 * time.Hour).UnixMilli()
	expired := time.Now().Add(-2 * time.Hour).UnixMilli()
	unusable := int64(1) << 62

	tests := map[string]struct {
		millis   *int64
		contains []string
	}{
		"success: a missing expiry says it is not recorded": {
			millis:   nil,
			contains: []string{"— (not recorded)"},
		},
		"success: a future expiry carries a countdown": {
			millis:   &fresh,
			contains: []string{" (in "},
		},
		"success: a past expiry says expired": {
			millis:   &expired,
			contains: []string{" (expired)"},
		},
		"success: an impossible timestamp is printed raw": {
			millis:   &unusable,
			contains: []string{"(not a usable timestamp)"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := expiry(tt.millis)
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("expiry = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestListTableFooter(t *testing.T) {
	t.Parallel()

	rows := []claude.AccountRow{
		{ID: "front-row", Record: config.AccountRecord{Kind: config.AccountKindLive()}, State: claude.StateOfNeedsLogin(), Source: claude.SourceNone, VisibleByDefault: true},
		{ID: "back-row", Record: config.AccountRecord{Kind: config.AccountKindForeign("claude-switcher")}, State: claude.StateOfForeign("claude-switcher"), Source: claude.SourceKeychain},
	}

	tests := map[string]struct {
		all      bool
		contains []string
		absent   []string
	}{
		"success: one hidden row is counted in the singular": {
			contains: []string{"front-row", "1 entry hidden (--all)"},
			absent:   []string{"back-row"},
		},
		"success: --all shows every row and no footer": {
			all:      true,
			contains: []string{"front-row", "back-row"},
			absent:   []string{"hidden (--all)"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := listTable(rows, "Claude Code-credentials", tt.all)
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("the table should contain %q:\n%s", want, out)
				}
			}
			for _, banned := range tt.absent {
				if strings.Contains(out, banned) {
					t.Errorf("the table should not contain %q:\n%s", banned, out)
				}
			}
		})
	}
}
