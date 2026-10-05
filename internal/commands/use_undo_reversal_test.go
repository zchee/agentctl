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
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func undoFixture(t *testing.T) (*config.Paths, *config.Registry) {
	t.Helper()
	paths := config.NewPaths(t.TempDir())
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatal(err)
	}
	registry := &config.Registry{Version: 1}
	for _, account := range []string{"restored", "installed", "third"} {
		dir := paths.NamespaceDir(account, "org")
		record, err := config.NewRecord(account, "org", config.AccountKindOwned(dir, claude.SHA8(dir)))
		if err != nil {
			t.Fatal(err)
		}
		registry.Accounts = append(registry.Accounts, record)
	}
	return paths, registry
}

func undoPrefix(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:4])
}

func writeUndoCredential(t *testing.T, paths *config.Paths, record config.AccountRecord, name, access, refresh, account string) {
	t.Helper()
	dir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": int64(4102444800000)}
	if account != "" {
		inner["tokenAccount"] = map[string]string{"uuid": account, "organizationUuid": "org"}
	}
	data, err := json.Marshal(map[string]any{"claudeAiOauth": inner})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func auditUndoEntry(t *testing.T, paths *config.Paths, event *secret.WriteEvent) secret.Undoable {
	t.Helper()
	if _, err := secret.AuditAppend(t.Context(), paths, secret.NewAuditEntry(event)); err != nil {
		t.Fatal(err)
	}
	tail, err := secret.TailAuditLog(paths, int(^uint(0)>>1))
	if err != nil {
		t.Fatal(err)
	}
	return secret.SelectUndo(tail)
}

func TestNamespacedReversal(t *testing.T) {
	tests := map[string]struct {
		missing, mismatch, firstWrite, installedCopy, noOwner, foreignStore, badSuffix bool
		unknownIdentity                                                                bool
		wantError                                                                      string
	}{
		"success: displaced identity owns reversal":             {},
		"success: unidentified copy belongs to store":           {unknownIdentity: true},
		"success: first write displaced plaintext":              {firstWrite: true},
		"error: adopted copy missing":                           {missing: true, wantError: "no longer in"},
		"error: adopted digest mismatch":                        {mismatch: true, wantError: "cannot match to that swap"},
		"error: first write copy contains installed credential": {firstWrite: true, installedCopy: true, wantError: "wrote rather than the one it displaced"},
		"error: namespace no longer owned":                      {noOwner: true, wantError: "no account agentctl currently owns"},
		"error: store is read-only":                             {foreignStore: true, wantError: "no account agentctl currently owns"},
		"error: invalid namespace suffix":                       {badSuffix: true, wantError: "no account agentctl currently owns"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, registry := undoFixture(t)
			store := registry.Accounts[1]
			from := new(undoPrefix("displaced"))
			if tt.firstWrite {
				from = nil
			}
			sha := store.Kind.Owned.ExportSHA8
			entry := auditUndoEntry(t, paths, &secret.WriteEvent{Target: secret.NamespaceTarget(sha), FromDigest8: from, ToDigest8: undoPrefix("incoming"), Outcome: secret.WriteApplied})
			if tt.badSuffix {
				registry.Accounts[1].Kind.Owned.ExportSHA8 = "ABCDEF12"
				entry.SHA8 = "ABCDEF12"
			}
			if !tt.missing {
				access, account := "displaced", "restored"
				if tt.mismatch {
					access = "other"
				}
				if tt.installedCopy {
					access = "incoming"
				}
				if tt.unknownIdentity {
					account = ""
				}
				writeUndoCredential(t, paths, store, secret.AdoptedFile, access, "refresh", account)
			}
			if tt.noOwner {
				registry.Accounts = registry.Accounts[:1]
			}
			if tt.foreignStore {
				registry.Accounts[1].Kind = config.AccountKindLive()
			}
			got, err := namespacedReversal(t.Context(), paths, registry, entry)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			owner := "restored"
			if tt.unknownIdentity {
				owner = "installed"
			}
			if diff := gocmp.Diff(owner, got.owner.AccountUUID); diff != "" {
				t.Errorf("owner (-want +got): %s", diff)
			}
			if got.live || got.store == nil || got.store.AccountUUID != "installed" || got.undone != nil || got.source.kind != useSourceAdopted || got.source.dir != paths.NamespaceDir("installed", "org") || got.inherited != store.Kind.Owned.ExportSpelling {
				t.Fatalf("wrong reversal: %+v", got)
			}
		})
	}
}

func TestLiveReversal(t *testing.T) {
	tests := map[string]struct {
		own, adopted, rotated, duplicate, wrongIdentity, readOnly, missingFrom, missingInstalled, malformed bool
		wantSource                                                                                          useSourceKind
		wantError                                                                                           string
	}{
		"success: owned credential":                            {own: true, wantSource: useSourceOwn},
		"success: adopted credential":                          {adopted: true, wantSource: useSourceAdopted},
		"success: identical copies prefer adopted":             {own: true, adopted: true, wantSource: useSourceAdopted},
		"error: same access with rotated refresh is ambiguous": {own: true, adopted: true, rotated: true, wantError: "is in both"},
		"error: two owned namespaces hold matching copies":     {adopted: true, duplicate: true, wantError: "is in both"},
		"error: copy in another account's namespace":           {own: true, wrongIdentity: true, wantError: "not in its own account's namespace"},
		"error: no displaced copy":                             {wantError: "not in its own account's namespace"},
		"error: non-owned copy does not qualify":               {own: true, readOnly: true, wantError: "not in its own account's namespace"},
		"error: no displaced digest":                           {own: true, missingFrom: true, wantError: "recorded no displaced credential"},
		"error: no installed identity":                         {own: true, missingInstalled: true, wantError: "does not record which account it installed"},
		"error: malformed candidate is ignored":                {malformed: true, wantError: "not in its own account's namespace"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, registry := undoFixture(t)
			event := &secret.WriteEvent{Target: secret.TargetLive, FromDigest8: new(undoPrefix("displaced")), ToDigest8: undoPrefix("incoming"), Outcome: secret.WriteApplied, Direction: secret.DirectionForward, IncomingIdentity: &secret.IncomingIdentity{AccountUUID: "installed", OrganizationUUID: new("org")}}
			if tt.missingFrom {
				event.FromDigest8 = nil
			}
			if tt.missingInstalled {
				event.IncomingIdentity = nil
			}
			entry := auditUndoEntry(t, paths, event)
			account := "restored"
			if tt.wrongIdentity {
				account = "third"
			}
			if tt.duplicate {
				account = ""
			}
			if tt.own {
				refresh := "refresh"
				if tt.rotated {
					refresh = "rotated"
				}
				writeUndoCredential(t, paths, registry.Accounts[0], secret.CredentialsFile, "displaced", refresh, account)
			}
			if tt.adopted {
				writeUndoCredential(t, paths, registry.Accounts[0], secret.AdoptedFile, "displaced", "refresh", account)
			}
			if tt.duplicate {
				writeUndoCredential(t, paths, registry.Accounts[2], secret.AdoptedFile, "displaced", "refresh", "")
			}
			if tt.readOnly {
				registry.Accounts[0].Kind = config.AccountKindLive()
			}
			if tt.malformed {
				dir := paths.NamespaceDir("restored", "org")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, secret.AdoptedFile), []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := liveReversal(t.Context(), paths, registry, entry)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.live || got.store != nil || got.inherited != "" || got.owner.AccountUUID != "restored" || got.source.kind != tt.wantSource || got.undone == nil || got.undone.installed.AccountUUID != "installed" {
				t.Fatalf("wrong reversal: %+v", got)
			}
		})
	}
}
