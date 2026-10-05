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
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestRunUndoSharedEngine(t *testing.T) {
	tests := map[string]struct {
		live, firstWrite, override, stranger, alreadyRestored bool
		undos                                                 int
		wantCode                                              int
		wantOutcome                                           string
	}{
		"success: namespace restores displaced grant":         {undos: 1, wantOutcome: "applied"},
		"success: first write undo never recreates plaintext": {firstWrite: true, undos: 1, wantOutcome: "applied"},
		"success: live undo restores owned grant":             {live: true, undos: 1, wantOutcome: "applied"},
		"success: live undo of undo restores installed grant": {live: true, undos: 2, wantOutcome: "applied"},
		"error: live audit target refuses namespace override": {live: true, override: true, undos: 1, wantCode: 13, wantOutcome: "refused"},
		"error: foreign live login is not misfiled":           {live: true, stranger: true, undos: 1, wantCode: 27, wantOutcome: "refused"},
		"success: already restored login writes nothing":      {live: true, alreadyRestored: true, undos: 1, wantOutcome: "already_active"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t).WithKeychain()
			fixture.WriteRegistry([]any{fixture.OwnedRecord("owner", "org"), fixture.OwnedRecord("incoming", "org")})
			paths := config.NewPaths(fixture.ConfigDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			ownerDir := fixture.NamespaceDir("owner", "org")
			outgoing := fixture.IdentifiedBlob("outgoing-access", "outgoing-refresh", testutil.FreshAt(), "owner", "org")
			incoming := fixture.IdentifiedBlob("incoming-access", "incoming-refresh", testutil.FreshAt(), "incoming", "org")
			fixture.WriteCredentials("incoming", "org", incoming)
			fixture.WriteCredentials("owner", "org", outgoing)
			service := claude.LiveService + "-" + claude.SHA8(ownerDir)
			if tt.live {
				service = claude.LiveService
				if err := os.MkdirAll(filepath.Join(fixture.Home(), ".claude"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if !tt.firstWrite {
				fixture.KeychainItem(service, outgoing)
			}
			fixture.AllowWrite(service)
			for _, entry := range fixture.Environ() {
				key, value, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
					t.Setenv(key, value)
				}
			}
			t.Setenv("HOME", fixture.Home())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
			t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
			t.Setenv(claude.SecureStorageEnv, "")
			if !tt.live {
				t.Setenv(claude.SecureStorageEnv, claude.ExportSpelling(ownerDir))
			}
			var profiles atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				profiles.Add(1)
				account := "owner"
				if strings.Contains(r.Header.Get("Authorization"), "incoming-access") {
					account = "incoming"
				} else if strings.Contains(r.Header.Get("Authorization"), "stranger-access") {
					account = "stranger"
				}
				if _, err := fmt.Fprintf(w, `{"account":{"uuid":%q,"email":"test@example.invalid"},"organization":{"uuid":"org"}}`, account); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			t.Setenv(claude.ProfileURLEnv, server.URL)
			var out, stderr bytes.Buffer
			process := SessionProcess{Out: &out, Err: &stderr}
			globals := cli.Globals{ConfigDir: fixture.ConfigDir()}
			if err := process.RunLive(t.Context(), globals, cli.ClaudeUseOptions{ID: "incoming", Live: true, Yes: true, JSON: true}); err != nil {
				t.Fatalf("forward swap: %v; out=%s; stderr=%s", err, out.String(), stderr.String())
			}
			if tt.override {
				t.Setenv(claude.SecureStorageEnv, claude.ExportSpelling(ownerDir))
			}
			if tt.stranger {
				fixture.KeychainItem(service, fixture.IdentifiedBlob("stranger-access", "stranger-refresh", testutil.FreshAt(), "stranger", "org"))
			}
			if tt.alreadyRestored {
				fixture.KeychainItem(service, outgoing)
			}
			read := func(path string) string {
				t.Helper()
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			beforeItem := read(fixture.KeychainItemPath(service))
			beforeAudit := read(secret.AuditLogPath(paths))
			beforeCalls := fixture.SecurityLog()
			beforeProfiles := profiles.Load()
			for range tt.undos {
				out.Reset()
				stderr.Reset()
				err := process.RunUndo(t.Context(), globals, cli.ClaudeUseOptions{Undo: true, Yes: true, JSON: true})
				if got := errs.ExitCode(err); got != tt.wantCode {
					t.Fatalf("exit=%d want=%d; err=%v; out=%s; stderr=%s", got, tt.wantCode, err, out.String(), stderr.String())
				}
				if !strings.Contains(out.String(), `"outcome": "`+tt.wantOutcome+`"`) {
					t.Fatalf("missing outcome %q: %s", tt.wantOutcome, out.String())
				}
			}
			if tt.wantCode != 0 || tt.alreadyRestored {
				if diff := gocmp.Diff(beforeItem, read(fixture.KeychainItemPath(service))); diff != "" {
					t.Errorf("item changed (-want +got): %s", diff)
				}
				if tt.wantCode != 0 {
					if diff := gocmp.Diff(beforeAudit, read(secret.AuditLogPath(paths))); diff != "" {
						t.Errorf("audit changed (-want +got): %s", diff)
					}
				}
				if tt.override {
					if diff := gocmp.Diff(beforeCalls, fixture.SecurityLog()); diff != "" {
						t.Errorf("override spawned security (-want +got): %s", diff)
					}
					if profiles.Load() != beforeProfiles || !strings.Contains(out.String(), `"refusal": "E"`) || !strings.Contains(out.String(), `"config": null`) {
						t.Errorf("override did not stop before profile/config work: %s", out.String())
					}
				} else {
					writes := 0
					for _, call := range fixture.SecurityLog()[len(beforeCalls):] {
						if call == "-i" {
							writes++
						}
					}
					if writes != 0 {
						t.Errorf("unexpected undo writes: %d", writes)
					}
				}
			} else {
				want := "outgoing-access"
				if tt.undos == 2 {
					want = "incoming-access"
				}
				if !strings.Contains(read(fixture.KeychainItemPath(service)), want) {
					t.Errorf("restored item does not contain %q", want)
				}
			}
			if tt.firstWrite {
				if _, err := os.Stat(fixture.CredentialsPath("owner", "org")); !os.IsNotExist(err) {
					t.Errorf("undo recreated plaintext store: %v", err)
				}
			}
		})
	}
}
