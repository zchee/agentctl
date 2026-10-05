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
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestUseGuardedRefreshSave(t *testing.T) {
	tests := map[string]struct {
		changed  bool
		absent   bool
		lock     bool
		migrated bool
		replay   bool
		pending  bool
		want     string
	}{
		"success: unchanged own file saves minted pair": {},
		"success: pending replay moves baseline":        {replay: true},
		"error: newer file is not overwritten":          {changed: true, want: "it changed while this swap was preparing"},
		"error: vanished file is not recreated":         {absent: true, want: "derived from is gone"},
		"error: vendor peer lock forbids saving":        {lock: true, want: "a Claude Code session holds"},
		"error: unlisted migrated item forbids saving":  {migrated: true, want: "has migrated into the keychain"},
		"error: rename failure parks and warns":         {pending: true, want: "is parked in"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t).WithKeychain()
			fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
			paths, record := refreshRecord(t, fixture)
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, entry := range fixture.Environ() {
				key, value, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
					t.Setenv(key, value)
				}
			}
			t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
			dir := fixture.NamespaceDir(testutil.Acct, testutil.Org)
			original := fixture.Blob("original-access", "original-refresh", testutil.ExpiredAt())
			file := fixture.WriteCredentials(testutil.Acct, testutil.Org, original)
			current, err := claude.ParseBlob([]byte(original))
			if err != nil {
				t.Fatal(err)
			}
			before, err := current.Digests()
			if err != nil {
				t.Fatal(err)
			}
			nextBlob := fixture.Blob("minted-access", "minted-refresh", testutil.FreshAt())
			next, err := claude.ParseBlob([]byte(nextBlob))
			if err != nil {
				t.Fatal(err)
			}
			if test.changed {
				fixture.WriteCredentials(testutil.Acct, testutil.Org, nextBlob)
			}
			if test.absent {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
			if test.lock {
				if err := os.Mkdir(filepath.Join(dir, secret.RefreshLockName), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.migrated {
				fixture.KeychainItem(secret.ForeignServiceName(record.Kind.Owned.ExportSHA8), nextBlob)
			}
			if test.replay {
				if err := os.WriteFile(filepath.Join(dir, secret.PendingFile), []byte(nextBlob), 0o600); err != nil {
					t.Fatal(err)
				}
				metadata, err := json.Marshal(map[string]any{"derived_from_access_sha256": before.AccessSHA256, "derived_from_refresh_sha256": before.RefreshSHA256, "new_expires_at": next.ExpiresAtMillis})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, secret.PendingMetaFile), metadata, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.pending {
				t.Setenv("AGENTCTL_FAULT", "rename_fail")
			}
			err = useGuardedWriteBack(t.Context(), paths, record, next, before)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("save error=%v want=%q", err, test.want)
			}
			if err != nil && (strings.Contains(err.Error(), "minted-access") || strings.Contains(err.Error(), "original-refresh")) {
				t.Fatalf("save error leaks token: %v", err)
			}
			if test.absent {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatalf("gone file was recreated: %v", err)
				}
				return
			}
			if test.pending {
				read, err := secret.ReadFile(filepath.Join(dir, secret.PendingFile), secret.MaxCredentialsBytes)
				if err != nil || !read.Present {
					t.Fatalf("pending pair missing: %v", err)
				}
				if strings.Contains(string(read.Bytes), "minted-access") == false {
					t.Fatal("pending file lacks minted grant")
				}
			}
			stored := rereadCredential(dir)
			if stored == nil {
				t.Fatal("stored credential missing")
			}
			got, err := stored.Digests()
			if err != nil {
				t.Fatal(err)
			}
			want := before
			if test.want == "" || test.changed {
				want, err = next.Digests()
				if err != nil {
					t.Fatal(err)
				}
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Fatalf("wrong stored pair: %s", diff)
			}
			fixture.AssertKeychainReadOnly()
		})
	}
}

func TestUseAdoptedRefreshSaveRefusesChangedOrPendingCopy(t *testing.T) {
	tests := map[string]struct {
		changed bool
		pending bool
		absent  bool
		want    string
	}{
		"success: unchanged adopted copy saves pair":    {},
		"error: changed adopted copy remains untouched": {changed: true, want: "changed since this undo"},
		"error: pending write blocks adopted refresh":   {pending: true, want: "unresolved pending write"},
		"error: vanished adopted copy remains absent":   {absent: true, want: "can no longer be read"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
			paths, _ := refreshRecord(t, fixture)
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			dir := fixture.NamespaceDir(testutil.Acct, testutil.Org)
			original := fixture.Blob("old-access", "old-refresh", testutil.ExpiredAt())
			fixture.WriteCredentials(testutil.Acct, testutil.Org, original)
			before, err := claude.ParseBlob([]byte(original))
			if err != nil {
				t.Fatal(err)
			}
			digests, err := before.Digests()
			if err != nil {
				t.Fatal(err)
			}
			next, err := claude.ParseBlob([]byte(fixture.Blob("new-access", "new-refresh", testutil.FreshAt())))
			if err != nil {
				t.Fatal(err)
			}
			parked := before
			if test.changed {
				parked = next
			}
			blob, err := useSealedBlob(parked)
			if err != nil {
				t.Fatal(err)
			}
			if !test.absent {
				if _, err := secret.WriteAdopted(t.Context(), paths, dir, blob); err != nil {
					t.Fatal(err)
				}
			}
			if test.pending {
				if err := os.WriteFile(filepath.Join(dir, secret.PendingFile), []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = useWriteBackAdopted(t.Context(), paths, dir, next, digests)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
			read, err := secret.ReadAdopted(dir)
			if err != nil {
				t.Fatal(err)
			}
			if test.absent {
				if read.Present {
					t.Fatal("vanished adopted copy was recreated")
				}
				return
			}
			found, err := claude.ParseBlob(read.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			got, err := found.Digests()
			if err != nil {
				t.Fatal(err)
			}
			want := digests
			if test.want == "" || test.changed {
				want, err = next.Digests()
				if err != nil {
					t.Fatal(err)
				}
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Fatal(diff)
			}
			own := rereadCredential(dir)
			if own == nil {
				t.Fatal("own home was lost")
			}
			ownDigest, err := own.Digests()
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(digests, ownDigest); diff != "" {
				t.Fatalf("own file was overwritten: %s", diff)
			}
		})
	}
}
