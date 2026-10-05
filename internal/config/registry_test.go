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

package config

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/lockfile"
)

func TestDocumentedTimings(t *testing.T) {
	t.Parallel()

	// The wait is the registry's own budget; the retry interval and the
	// create-attempt bound come with the shared flock implementation the
	// registry locks through, and are asserted here because this is the
	// budget the registry's callers experience.
	tests := map[string]struct {
		got  time.Duration
		want time.Duration
	}{
		"success: registry wait":            {got: RegistryLockWait, want: 5 * time.Second},
		"success: registry retry":           {got: lockfile.RetryInterval, want: 250 * time.Millisecond},
		"success: registry create attempts": {got: time.Duration(lockfile.CreateAttempts), want: 8},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("documented lock budget mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func testStore(t *testing.T) *Paths {
	t.Helper()
	return NewPaths(filepath.Join(t.TempDir(), "agctl"))
}

func ownedRecord(t *testing.T, acct, org string) AccountRecord {
	t.Helper()
	rec, err := NewRecord(acct, org, AccountKindOwned("/store/claude/"+acct+"/"+org, "aaaaaaaa"))
	if err != nil {
		t.Fatalf("NewRecord(%q, %q) = %v", acct, org, err)
	}
	email := acct + "@example.com"
	rec.Email = &email
	return rec
}

func codexRow(user, acct string) CodexAccountRecord {
	email := user + "@example.com"
	plan := "plus"
	return CodexAccountRecord{ChatGPTUserID: user, ChatGPTAccountID: acct, Email: &email, PlanType: &plan, Kind: CodexKindLive(), CreatedAt: "2026-09-17T00:00:00Z"}
}

func TestAnAbsentFileLoadsAsAnEmptyRegistry(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	registry, err := LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatalf("LoadRegistry() = %v: an absent file is not an error", err)
	}
	if registry.Version != RegistryVersion {
		t.Errorf("version = %d, want %d", registry.Version, RegistryVersion)
	}
	if len(registry.Accounts) != 0 {
		t.Errorf("accounts = %v, want none", registry.Accounts)
	}
}

func TestSavingAndLoadingRoundTripsEveryKind(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	live, err := NewRecord("acct-2", UnknownOrg, AccountKindLive())
	if err != nil {
		t.Fatalf("NewRecord(live) = %v", err)
	}
	configDir, err := NewRecord("acct-3", "org-3", AccountKindConfigDirReadOnly("/elsewhere", "Claude Code-credentials-5cdc535f", true))
	if err != nil {
		t.Fatalf("NewRecord(config dir) = %v", err)
	}
	foreign, err := NewRecord("acct-4", "org-4", AccountKindForeign("claude-switcher"))
	if err != nil {
		t.Fatalf("NewRecord(foreign) = %v", err)
	}

	var saved Registry
	err = UpdateRegistry(t.Context(), paths, func(registry *Registry) {
		registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
		registry.Upsert(live)
		registry.Upsert(configDir)
		registry.Upsert(foreign)
		saved = *registry
	})
	if err != nil {
		t.Fatalf("UpdateRegistry() = %v", err)
	}

	loaded, err := LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatalf("LoadRegistry() = %v", err)
	}
	if diff := gocmp.Diff(saved.Accounts, loaded.Accounts); diff != "" {
		t.Errorf("accounts mismatch (-saved +loaded):\n%s", diff)
	}
}

func TestTheSavedFileIs0600AndTheLockFileStays(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	if err := UpdateRegistry(t.Context(), paths, func(*Registry) {}); err != nil {
		t.Fatalf("UpdateRegistry() = %v", err)
	}

	info, err := os.Stat(paths.ConfigFile())
	if err != nil {
		t.Fatalf("Stat(%q) = %v", paths.ConfigFile(), err)
	}
	if mode := info.Mode().Perm(); mode != FileMode {
		t.Errorf("registry mode = %o, want %o", mode, FileMode)
	}
	if _, err := os.Stat(paths.ConfigLock()); err != nil {
		t.Errorf("the lock file is created once and never unlinked: %v", err)
	}
}

func TestSavingTwiceLeavesNoTemporaryFile(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	if err := UpdateRegistry(t.Context(), paths, func(*Registry) {}); err != nil {
		t.Fatalf("first UpdateRegistry() = %v", err)
	}
	err := UpdateRegistry(t.Context(), paths, func(registry *Registry) {
		registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
	})
	if err != nil {
		t.Fatalf("second UpdateRegistry() = %v", err)
	}

	entries, err := os.ReadDir(paths.ConfigDir())
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v", paths.ConfigDir(), err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp.") {
			t.Errorf("left behind: %s", entry.Name())
		}
	}
}

func TestARegistryWithoutCodexRowsSerializesExactlyAsItDidBeforeTheyExisted(t *testing.T) {
	t.Parallel()

	// The compatibility promise, stated as bytes: a Codex-unaware build
	// reads this document, and a build that added and then removed a
	// Codex row hands it back unchanged.
	registry := &Registry{Version: RegistryVersion}
	registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
	claudeOnly, err := registry.document()
	if err != nil {
		t.Fatalf("document() = %v", err)
	}
	if strings.Contains(string(claudeOnly), "codex_accounts") {
		t.Errorf("an empty list is not written:\n%s", claudeOnly)
	}
	if !strings.Contains(string(claudeOnly), `"version": 1`) {
		t.Errorf("missing the derived version:\n%s", claudeOnly)
	}

	registry.CodexAccounts = append(registry.CodexAccounts, codexRow("user-01", "acct-01"))
	withCodex, err := registry.document()
	if err != nil {
		t.Fatalf("document() = %v", err)
	}
	if !strings.Contains(string(withCodex), `"version": 2`) {
		t.Errorf("a Codex row raises the version:\n%s", withCodex)
	}
	if !strings.Contains(string(withCodex), "codex_accounts") {
		t.Errorf("the Codex rows are written:\n%s", withCodex)
	}

	registry.CodexAccounts = registry.CodexAccounts[:0]
	again, err := registry.document()
	if err != nil {
		t.Fatalf("document() = %v", err)
	}
	if diff := gocmp.Diff(string(claudeOnly), string(again)); diff != "" {
		t.Errorf("removing the last Codex row must return the file to exactly what it was (-before +after):\n%s", diff)
	}
}

func TestTheVersionWrittenIsTheOneTheContentImpliesWhateverTheFieldHeld(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		registry    *Registry
		wantVersion string
	}{
		"success: a codex row raises a version-1 document to 2": {
			registry:    &Registry{Version: RegistryVersion, CodexAccounts: []CodexAccountRecord{codexRow("user-01", "acct-01")}},
			wantVersion: `"version": 2`,
		},
		"success: a document with no codex rows goes back to 1 however it was stamped": {
			registry:    &Registry{Version: RegistryVersionCodex},
			wantVersion: `"version": 1`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			document, err := tt.registry.document()
			if err != nil {
				t.Fatalf("document() = %v", err)
			}
			if !strings.Contains(string(document), tt.wantVersion) {
				t.Errorf("document should contain %q:\n%s", tt.wantVersion, document)
			}
		})
	}
}

func TestSavingStampsTheVersionOnDiskAndLoadingAcceptsBoth(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	err := UpdateRegistry(t.Context(), paths, func(registry *Registry) {
		registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
		registry.CodexAccounts = append(registry.CodexAccounts, codexRow("user-01", "acct-01"))
	})
	if err != nil {
		t.Fatalf("UpdateRegistry() = %v", err)
	}

	loaded, err := LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatalf("LoadRegistry() = %v: version 2 is readable", err)
	}
	if loaded.Version != RegistryVersionCodex {
		t.Errorf("version = %d, want %d", loaded.Version, RegistryVersionCodex)
	}
	if diff := gocmp.Diff([]CodexAccountRecord{codexRow("user-01", "acct-01")}, loaded.CodexAccounts); diff != "" {
		t.Errorf("codex accounts mismatch (-want +got):\n%s", diff)
	}

	err = UpdateRegistry(t.Context(), paths, func(registry *Registry) {
		registry.CodexAccounts = nil
	})
	if err != nil {
		t.Fatalf("UpdateRegistry() = %v", err)
	}
	loaded, err = LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatalf("LoadRegistry() = %v: version 1 is readable", err)
	}
	if loaded.Version != RegistryVersion {
		t.Errorf("version = %d, want %d", loaded.Version, RegistryVersion)
	}
	if len(loaded.CodexAccounts) != 0 {
		t.Errorf("codex accounts should be gone, got %v", loaded.CodexAccounts)
	}
}

func TestAVersion1FileHoldingCodexRowsIsRefused(t *testing.T) {
	t.Parallel()

	// The two halves of such a file disagree. Reading it as version 1
	// would mean a later write silently dropping the rows; reading it as
	// version 2 would mean trusting a stamp that is demonstrably wrong.
	// Neither is an answer, so it is refused with both facts named.
	paths := testStore(t)
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}
	registry := &Registry{Version: RegistryVersionCodex, CodexAccounts: []CodexAccountRecord{codexRow("user-01", "acct-01")}}
	document, err := registry.document()
	if err != nil {
		t.Fatalf("document() = %v", err)
	}
	// Rewrite the stamp so the content and the version disagree.
	tampered := strings.Replace(string(document), `"version": 2`, `"version": 1`, 1)
	if err := os.WriteFile(paths.ConfigFile(), []byte(tampered), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}

	_, err = LoadRegistry(t.Context(), paths)
	if err == nil {
		t.Fatalf("LoadRegistry() = nil, want a refusal: the disagreement is not guessed at")
	}
	message := err.Error()
	if !strings.Contains(message, "version 1") {
		t.Errorf("the message should name the stamp: %q", message)
	}
	if !strings.Contains(message, "Codex account") {
		t.Errorf("the message should name the rows: %q", message)
	}
}

func TestAFutureVersionIsRefusedRatherThanMisread(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte(`{"version": 99, "accounts": []}`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}

	_, err := LoadRegistry(t.Context(), paths)
	if err == nil {
		t.Fatalf("LoadRegistry() = nil: a newer schema should not be guessed at")
	}
	if !strings.Contains(err.Error(), "version 99") {
		t.Errorf("the message should name the version: %q", err)
	}
}

func TestACorruptFileIsRefused(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if _, err := LoadRegistry(t.Context(), paths); err == nil {
		t.Errorf("LoadRegistry() = nil, want an error")
	}
}

func TestUpsertReplacesByAccountAndOrganization(t *testing.T) {
	t.Parallel()

	registry := &Registry{Version: RegistryVersion}
	registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
	registry.Upsert(ownedRecord(t, "acct-1", "org-2"))
	if len(registry.Accounts) != 2 {
		t.Fatalf("a second organization is a second namespace: got %d records", len(registry.Accounts))
	}

	replacement := ownedRecord(t, "acct-1", "org-1")
	label := "work"
	replacement.Label = &label
	registry.Upsert(replacement)
	if len(registry.Accounts) != 2 {
		t.Errorf("the same key replaces rather than duplicating: got %d records", len(registry.Accounts))
	}
	rec := registry.Get("acct-1", "org-1")
	if rec == nil || rec.Label == nil || *rec.Label != "work" {
		t.Errorf("Get(acct-1, org-1) = %+v, want the replaced record", rec)
	}
}

func TestResolveIDAcceptsAUUIDAPairALabelAndAnEmail(t *testing.T) {
	t.Parallel()

	registry := &Registry{Version: RegistryVersion}
	registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
	labelled := ownedRecord(t, "acct-2", "org-2")
	label := "work"
	labelled.Label = &label
	registry.Upsert(labelled)

	for _, id := range []string{"acct-1", "acct-1/org-1", "acct-1@example.com"} {
		rec, err := registry.ResolveID(id)
		if err != nil {
			t.Fatalf("ResolveID(%q) = %v", id, err)
		}
		if acct, org := rec.Key(); acct != "acct-1" || org != "org-1" {
			t.Errorf("ResolveID(%q) = (%s, %s), want (acct-1, org-1)", id, acct, org)
		}
	}
	rec, err := registry.ResolveID("work")
	if err != nil {
		t.Fatalf("ResolveID(work) = %v: a label resolves", err)
	}
	if acct, org := rec.Key(); acct != "acct-2" || org != "org-2" {
		t.Errorf("ResolveID(work) = (%s, %s), want (acct-2, org-2)", acct, org)
	}
}

func TestResolveIDReportsAnAmbiguousAccountWithItsCandidates(t *testing.T) {
	t.Parallel()

	registry := &Registry{Version: RegistryVersion}
	registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
	registry.Upsert(ownedRecord(t, "acct-1", "org-2"))

	_, err := registry.ResolveID("acct-1")
	if err == nil {
		t.Fatalf("ResolveID(acct-1) = nil: two organizations are ambiguous")
	}
	message := err.Error()
	for _, candidate := range []string{"acct-1/org-1", "acct-1/org-2"} {
		if !strings.Contains(message, candidate) {
			t.Errorf("the message should offer %q: %q", candidate, message)
		}
	}

	rec, err := registry.ResolveID("acct-1/org-2")
	if err != nil {
		t.Fatalf("ResolveID(acct-1/org-2) = %v: the unambiguous spelling still works", err)
	}
	if acct, org := rec.Key(); acct != "acct-1" || org != "org-2" {
		t.Errorf("ResolveID(acct-1/org-2) = (%s, %s)", acct, org)
	}
}

func TestResolveIDReportsAnUnknownID(t *testing.T) {
	t.Parallel()

	registry := &Registry{Version: RegistryVersion}
	if _, err := registry.ResolveID("nobody"); err == nil {
		t.Errorf("ResolveID(nobody) = nil, want an error")
	}
}

func TestDisplayIDShortensToTheUUIDOnlyWhenItIsUnique(t *testing.T) {
	t.Parallel()

	unique := []AccountRecord{ownedRecord(t, "acct-1", "org-1"), ownedRecord(t, "acct-2", "org-2")}
	if got := unique[0].DisplayID(unique); got != "acct-1" {
		t.Errorf("DisplayID = %q, want %q", got, "acct-1")
	}

	duplicated := []AccountRecord{ownedRecord(t, "acct-1", "org-1"), ownedRecord(t, "acct-1", "org-2")}
	if got := duplicated[0].DisplayID(duplicated); got != "acct-1/org-1" {
		t.Errorf("DisplayID = %q, want %q", got, "acct-1/org-1")
	}
}

func TestNewRecordRefusesAnIdentifierThatWouldEscapeTheNamespaceRoot(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{{"..", "org"}, {"acct", "../.."}, {"a/b", "org"}} {
		if _, err := NewRecord(pair[0], pair[1], AccountKindLive()); err == nil {
			t.Errorf("NewRecord(%q, %q) = nil, want a refusal", pair[0], pair[1])
		}
	}
}

func TestNewRecordStampsAnRFC3339CreationTime(t *testing.T) {
	t.Parallel()

	rec := ownedRecord(t, "acct-1", "org-1")
	if !strings.HasSuffix(rec.CreatedAt, "Z") {
		t.Errorf("CreatedAt = %q, want a UTC stamp", rec.CreatedAt)
	}
	if !strings.Contains(rec.CreatedAt, "T") {
		t.Errorf("CreatedAt = %q, want an RFC 3339 stamp", rec.CreatedAt)
	}
	if rec.Forgotten {
		t.Errorf("a new record is not forgotten")
	}
}

func TestTheSerializedShapeTagsTheKind(t *testing.T) {
	t.Parallel()

	rec := ownedRecord(t, "acct-1", "org-1")
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal(record) = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	kind, ok := decoded["kind"].(map[string]any)
	if !ok {
		t.Fatalf("kind member = %T, want an object", decoded["kind"])
	}
	if kind["kind"] != "owned" {
		t.Errorf("kind tag = %v, want owned", kind["kind"])
	}
	if kind["export_sha8"] != "aaaaaaaa" {
		t.Errorf("export_sha8 = %v", kind["export_sha8"])
	}
	if decoded["forgotten"] != false {
		t.Errorf("forgotten = %v, want false", decoded["forgotten"])
	}

	live, err := json.Marshal(AccountKindLive())
	if err != nil {
		t.Fatalf("Marshal(live) = %v", err)
	}
	if diff := gocmp.Diff(`{"kind":"live"}`, string(live)); diff != "" {
		t.Errorf("live kind mismatch (-want +got):\n%s", diff)
	}
	configDir, err := json.Marshal(AccountKindConfigDirReadOnly("/x", "svc", false))
	if err != nil {
		t.Fatalf("Marshal(config dir) = %v", err)
	}
	if !strings.Contains(string(configDir), `"kind":"config_dir_read_only"`) {
		t.Errorf("config dir kind = %s", configDir)
	}
}

func TestTheOnDiskFormatRoundTripsByteForByte(t *testing.T) {
	t.Parallel()

	// Literal documents in the exact shape existing stores hold, written
	// with two-space indentation, a space after each colon, nulls for
	// absent optionals, and the kind tag first. A loaded document must
	// serialize back to the same bytes, or the store is not shared with
	// what wrote it.
	tests := map[string]struct {
		document string
	}{
		"success: a claude-only registry": {
			document: `{
  "version": 1,
  "accounts": [
    {
      "account_uuid": "acct-1",
      "organization_uuid": "org-1",
      "email": "user@example.com",
      "org_name": "Acme",
      "label": null,
      "kind": {
        "kind": "owned",
        "export_spelling": "/store/claude/acct-1/org-1",
        "export_sha8": "5cdc535f"
      },
      "forgotten": false,
      "created_at": "2026-09-08T00:00:00Z"
    },
    {
      "account_uuid": "acct-3",
      "organization_uuid": "org-3",
      "email": "read-only@example.com",
      "org_name": null,
      "label": null,
      "kind": {
        "kind": "config_dir_read_only",
        "dir": "/elsewhere",
        "service": "Claude Code-credentials-5cdc535f",
        "shares_live_dir": false
      },
      "forgotten": false,
      "created_at": "2026-09-08T00:00:00Z"
    },
    {
      "account_uuid": "acct-2",
      "organization_uuid": "_unknown-org",
      "email": null,
      "org_name": null,
      "label": null,
      "kind": {
        "kind": "live"
      },
      "forgotten": true,
      "created_at": "2026-09-08T00:00:00Z"
    },
    {
      "account_uuid": "acct-4",
      "organization_uuid": "org-4",
      "email": null,
      "org_name": null,
      "label": "work",
      "kind": {
        "kind": "foreign",
        "source": "claude-switcher"
      },
      "forgotten": false,
      "created_at": "2026-09-08T00:00:00Z"
    }
  ],
  "forgotten_services": [
    "Claude Code-credentials-deadbeef"
  ]
}`,
		},
		"success: a registry with codex accounts": {
			document: `{
  "version": 2,
  "accounts": [],
  "forgotten_services": [],
  "codex_accounts": [
    {
      "chatgpt_user_id": "user-01",
      "chatgpt_account_id": "acct-01",
      "email": "user-01@example.com",
      "plan_type": "plus",
      "label": null,
      "kind": {
        "kind": "owned",
        "export_spelling": "/store/codex/user-01/acct-01",
        "refresh": "never"
      },
      "forgotten": false,
      "created_at": "2026-09-17T00:00:00Z"
    },
    {
      "chatgpt_user_id": "user-02",
      "chatgpt_account_id": "acct-02",
      "email": null,
      "plan_type": null,
      "label": null,
      "kind": {
        "kind": "home_read_only",
        "dir": "/homes/other-codex"
      },
      "forgotten": false,
      "created_at": "2026-09-17T00:00:00Z"
    },
    {
      "chatgpt_user_id": "user-03",
      "chatgpt_account_id": "acct-03",
      "email": null,
      "plan_type": null,
      "label": null,
      "kind": {
        "kind": "live"
      },
      "forgotten": false,
      "created_at": "2026-09-17T00:00:00Z"
    }
  ]
}`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var registry Registry
			if err := json.Unmarshal([]byte(tt.document), &registry); err != nil {
				t.Fatalf("Unmarshal() = %v", err)
			}
			out, err := registry.document()
			if err != nil {
				t.Fatalf("document() = %v", err)
			}
			if diff := gocmp.Diff(tt.document, string(out)); diff != "" {
				t.Errorf("on-disk bytes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAnAbsentRefreshPolicyDefaultsToAutoAndAnUnknownOneIsRefused(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		kind    string
		want    RefreshPolicy
		wantErr bool
	}{
		"success: an absent policy reads as auto": {
			kind: `{"kind": "owned", "export_spelling": "/x"}`,
			want: RefreshAuto,
		},
		"success: never survives": {
			kind: `{"kind": "owned", "export_spelling": "/x", "refresh": "never"}`,
			want: RefreshNever,
		},
		"error: an unknown policy is refused": {
			kind:    `{"kind": "owned", "export_spelling": "/x", "refresh": "sometimes"}`,
			wantErr: true,
		},
		"error: an unknown kind tag is refused": {
			kind:    `{"kind": "mystery"}`,
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var kind CodexKind
			err := json.Unmarshal([]byte(tt.kind), &kind)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = nil, want an error", tt.kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) = %v", tt.kind, err)
			}
			if kind.Owned == nil {
				t.Fatalf("owned payload missing: %+v", kind)
			}
			if kind.Owned.Refresh != tt.want {
				t.Errorf("refresh = %q, want %q", kind.Owned.Refresh, tt.want)
			}
		})
	}
}

func TestAnUnknownAccountKindTagIsRefused(t *testing.T) {
	t.Parallel()

	var kind AccountKind
	if err := json.Unmarshal([]byte(`{"kind": "mystery"}`), &kind); err == nil {
		t.Errorf("Unmarshal(mystery) = nil, want an error")
	}
}

func TestTwoConcurrentUpdatesBothLand(t *testing.T) {
	t.Parallel()

	// The reason the write is not reachable without the lock: a
	// load-outside/save-inside pair would let each goroutine write a
	// registry it read before the other's record existed, and the later
	// write would erase the earlier one. The update re-reads inside the
	// lock, so the second writer sees the first writer's record and adds
	// to it.
	paths := testStore(t)
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}

	var wg sync.WaitGroup
	errc := make(chan error, 2)
	for _, acct := range []string{"acct-1", "acct-2"} {
		wg.Go(func() {
			errc <- UpdateRegistry(t.Context(), paths, func(registry *Registry) {
				registry.Upsert(ownedRecord(t, acct, "org"))
			})
		})
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatalf("UpdateRegistry() = %v", err)
		}
	}

	loaded, err := LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatalf("LoadRegistry() = %v", err)
	}
	accounts := make([]string, 0, len(loaded.Accounts))
	for i := range loaded.Accounts {
		accounts = append(accounts, loaded.Accounts[i].AccountUUID)
	}
	slices.Sort(accounts)
	if diff := gocmp.Diff([]string{"acct-1", "acct-2"}, accounts); diff != "" {
		t.Errorf("one update overwrote the other (-want +got):\n%s", diff)
	}
}

func TestUpdatePersistsTheChange(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	var count int
	err := UpdateRegistry(t.Context(), paths, func(registry *Registry) {
		registry.Upsert(ownedRecord(t, "acct-1", "org-1"))
		count = len(registry.Accounts)
	})
	if err != nil {
		t.Fatalf("UpdateRegistry() = %v", err)
	}
	if count != 1 {
		t.Errorf("the closure sees the registry it changed: count = %d", count)
	}

	loaded, err := LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatalf("LoadRegistry() = %v", err)
	}
	if rec := loaded.Get("acct-1", "org-1"); rec == nil {
		t.Errorf("the change should persist")
	}
}

func TestARegistryTheReferenceBinaryWroteRoundTripsByteForByte(t *testing.T) {
	t.Parallel()

	// The fixtures under testdata were written by another binary that
	// shares this store format, running against a sandboxed store, so this
	// round trip proves byte compatibility with what real stores hold, not
	// merely with this package's own output.
	tests := map[string]struct {
		fixture string
	}{
		"success: a store hiding one unclaimed service":  {fixture: "registry-forgotten-service.json"},
		"success: a store with everything emptied again": {fixture: "registry-empty.json"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			want, err := os.ReadFile(filepath.Join("testdata", tt.fixture))
			if err != nil {
				t.Fatalf("ReadFile(%q) = %v", tt.fixture, err)
			}
			var registry Registry
			if err := json.Unmarshal(want, &registry); err != nil {
				t.Fatalf("Unmarshal() = %v", err)
			}
			got, err := registry.document()
			if err != nil {
				t.Fatalf("document() = %v", err)
			}
			if diff := gocmp.Diff(string(want), string(got)); diff != "" {
				t.Errorf("on-disk bytes mismatch (-reference +got):\n%s", diff)
			}
		})
	}
}
