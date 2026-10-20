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
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestMigratedRefreshNamesMembersRewrittenAfterItsWrite(t *testing.T) {
	const canary = "SIBLING-CANARY-9f31"
	tests := map[string]struct {
		// rewrite is applied to the stored document while the refresh is
		// paused after its write; empty leaves the item as written.
		from, to  string
		wantNote  string
		wantCheck secret.SiblingsReadback
	}{
		"success: a login another process adds after the write is named": {
			from:      `"two":{}`,
			to:        `"two":{},"three":{}`,
			wantNote:  "the keychain item's members mcpOAuth did not read back as written: another process rewrote them after this write",
			wantCheck: secret.SiblingsDiffer,
		},
		"success: an item left as written adds no note": {
			wantCheck: secret.SiblingsEqual,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world, item, _ := migratedWorld(t)
			paths, _ := refreshRecord(t, world.fixture)
			itemPath := world.fixture.KeychainItemPath(item.service)
			seeded := withMembers(t, world.fixture.Blob("old-access", "old-refresh", testutil.ExpiredAt()), `"mcpOAuth":{"one":{"accessToken":"`+canary+`"},"two":{}},"pluginSecrets":"p"`)
			if err := os.WriteFile(itemPath, []byte(seeded), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := world.status.readRefreshItem(t.Context(), item.service)
			if err != nil {
				t.Fatal(err)
			}
			world.status.Refresher = refreshOAuth(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`))
			})
			resume := filepath.Join(t.TempDir(), "resume")
			t.Setenv("AGENTCTL_FAULT", "pause_"+fault.MigratedAfterWrite)
			t.Setenv("AGENTCTL_FAULT_RESUME", resume)

			done := make(chan refreshOutcome, 1)
			go func() { done <- world.status.refreshMigrated(t.Context(), paths, item, current) }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(resume + ".reached"); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the refresh never reached the pause after its write")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if tt.from != "" {
				written, err := os.ReadFile(itemPath)
				if err != nil {
					t.Fatal(err)
				}
				rewritten := strings.Replace(string(written), tt.from, tt.to, 1)
				if rewritten == string(written) {
					t.Fatal("the written item does not hold the member to rewrite")
				}
				if err := os.WriteFile(itemPath, []byte(rewritten), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(resume, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			result := <-done

			// The account object read back as written, so the refresh is
			// applied whatever the members say.
			if result.lockState != "migrated_refreshed" || result.state != nil || result.credentials == nil {
				t.Fatalf("refresh result = %+v", result)
			}
			if diff := gocmp.Diff(tt.wantNote, result.note); diff != "" {
				t.Errorf("note (-want +got):\n%s", diff)
			}
			tail, err := secret.TailAuditLog(paths, 8)
			if err != nil || len(tail.Entries) == 0 {
				t.Fatalf("no audit entry: %v", err)
			}
			event, ok := tail.Entries[len(tail.Entries)-1].Event.(*secret.WriteEvent)
			if !ok {
				t.Fatalf("last audit entry is %T", tail.Entries[len(tail.Entries)-1].Event)
			}
			if event.Outcome != secret.WriteApplied || event.SiblingsReadback != tt.wantCheck || event.Transport != secret.TransportArgv {
				t.Errorf("audit = %+v", event)
			}
			if strings.Contains(result.note, canary) {
				t.Error("the note carries a member's value")
			}
			for _, path := range []string{secret.AuditLogPath(paths), os.Getenv("AGCTL_FAKE_SECURITY_LOG")} {
				if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), canary) {
					t.Errorf("%s carries a carried member's value", filepath.Base(path))
				}
			}
		})
	}
}
