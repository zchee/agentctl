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

package testutil

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
)

func init() {
	registerScriptCmd("swap-orgs-setup", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: swap-orgs-setup")
		}
		account := ts.Getenv("INCOMING")
		configDir := ts.Getenv("AGENTCTL_CONFIG_DIR")
		org1, org2 := "12121212-1111-4111-8111-121212121212", "23232323-2222-4222-8222-232323232323"
		dir1, dir2 := filepath.Join(configDir, "claude", account, org1), filepath.Join(configDir, "claude", account, org2)
		write := func(path string, value any) {
			ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
			body, err := json.Marshal(value)
			ts.Check(err)
			ts.Check(os.WriteFile(path, body, 0o600))
		}
		owned := func(org, dir string) any {
			spelling := ExportSpelling(dir)
			return map[string]any{"account_uuid": account, "organization_uuid": org, "kind": map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339)}
		}
		write(filepath.Join(configDir, "config.json"), map[string]any{"version": 1, "accounts": []any{owned(org1, dir1), owned(org2, dir2)}, "forgotten_services": []string{}})
		blob := func(name, org string, expiry int64) any {
			return map[string]any{"claudeAiOauth": map[string]any{"accessToken": "SENTINEL-access-" + name, "refreshToken": "SENTINEL-refresh-" + name, "expiresAt": expiry, "scopes": []string{"user:inference", "user:profile"}, "tokenAccount": map[string]any{"uuid": account, "organizationUuid": org, "emailAddress": nil, "organizationName": nil, "workspaceId": nil, "workspaceName": nil}}}
		}
		source := filepath.Join(dir1, ".credentials.json")
		write(source, blob("org1", org1, FreshAt()))
		ts.Check(os.MkdirAll(dir2, 0o700))
		write(ts.Getenv("UNDO_ITEM"), blob("org2", org2, FreshAt()+3_600_000))
		for key, value := range map[string]string{"ORGS_SOURCE": source, "ORGS_DIR1": dir1, "ORGS_DIR2": dir2, "ORGS_1": org1, "ORGS_2": org2} {
			ts.Setenv(key, value)
		}
		var posts, asked1, asked2 atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posts.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var org string
			switch r.Header.Get("Authorization") {
			case "Bearer SENTINEL-access-org1":
				org = org1
				asked1.Add(1)
			case "Bearer SENTINEL-access-org2":
				org = org2
				asked2.Add(1)
			default:
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			say(w, `{"account":{"uuid":%q,"email":"org@example.invalid"},"organization":{"uuid":%q,"name":"Example"}}`, account, org)
		}))
		ts.Defer(server.Close)
		ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+"/token")
		ts.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", server.URL+"/profile")
		ts.SetCmd("swap-orgs-check", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 0 {
				ts.Fatalf("usage: swap-orgs-check")
			}
			if posts.Load() != 0 || asked1.Load() != 2 || asked2.Load() != 2 {
				ts.Fatalf("organization profiles/POST counts=%d/%d/%d; want 2/2/0", asked1.Load(), asked2.Load(), posts.Load())
			}
			for _, dir := range []string{dir1, dir2} {
				for _, path := range []string{filepath.Join(dir, ".oauth_refresh.lock"), dir + ".lock", filepath.Join(dir, ".storage-write.lock")} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						ts.Fatalf("organization namespace retains a peer artifact: %v", err)
					}
				}
			}
			for line := range strings.SplitSeq(ts.ReadFile(ts.Getenv("AGCTL_FAKE_SECURITY_LOG")), "\n") {
				if strings.Contains(line, "SENTINEL") || strings.HasPrefix(line, "delete-generic-password") {
					ts.Fatalf("unsafe security argv")
				}
			}
		})
	})
}
