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
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
)

func TestWatchReadOnlyPass(t *testing.T) {
	tests := map[string]struct {
		expired, forced  bool
		status, wantGets int
		state            codexprovider.StateKind
	}{
		"success: ordinary tick serves fresh cache":       {status: 200, wantGets: 1, state: codexprovider.StateOK},
		"success: forced tick bypasses cache":             {forced: true, status: 200, wantGets: 2, state: codexprovider.StateOK},
		"success: expired owned grant is never refreshed": {expired: true, status: 200, state: codexprovider.StateExpired},
		"error: unauthorized usage never refreshes":       {status: 401, wantGets: 2, state: codexprovider.StateUnauthorized},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			paths, err := config.Resolve(filepath.Join(root, "config"))
			if err != nil {
				t.Fatal(err)
			}
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := paths.EnsureCodexDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			namespace, err := paths.CodexNamespaceDir("watch-user", "watch-account")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(namespace, 0o700); err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(24 * time.Hour).Unix()
			if test.expired {
				expiry = 1000
			}
			jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry))) + ".watch-test"
			auth := []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"watch-test-refresh","account_id":"watch-account"}}`, jwt))
			authPath := filepath.Join(namespace, "auth.json")
			if err := os.WriteFile(authPath, auth, 0o600); err != nil {
				t.Fatal(err)
			}
			pending := filepath.Join(namespace, "auth.json.pending")
			if err := os.WriteFile(pending, []byte(`{"parked":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := config.UpdateRegistry(t.Context(), paths, func(registry *config.Registry) {
				registry.CodexAccounts = []config.CodexAccountRecord{{ChatGPTUserID: "watch-user", ChatGPTAccountID: "watch-account", Kind: config.CodexKindOwned(namespace, config.RefreshAuto), CreatedAt: "2026-09-17T00:00:00Z"}}
			}); err != nil {
				t.Fatal(err)
			}
			var gets, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
				} else {
					gets.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000}}}`))
			}))
			defer server.Close()
			t.Setenv("AGENTCTL_CODEX_TOKEN_URL", server.URL+"/oauth/token")
			status := &Status{Env: &codexprovider.Env{Home: filepath.Join(root, "home")}, Client: codexprovider.NewUsageClient(server.URL, "watch-test", time.Second)}
			watch := Watch{NewStatus: func() *Status { return status }}
			if _, err := watch.collect(t.Context(), paths, false); err != nil {
				t.Fatal(err)
			}
			rows, err := watch.collect(t.Context(), paths, test.forced)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("watch rows=%d, want 1", len(rows))
			}
			if diff := gocmp.Diff(test.state, rows[0].State.Kind); diff != "" {
				t.Fatalf("watch state (-want +got):\n%s", diff)
			}
			if gets.Load() != int32(test.wantGets) || posts.Load() != 0 {
				t.Fatalf("watch sent GET=%d POST=%d, want GET=%d POST=0", gets.Load(), posts.Load(), test.wantGets)
			}
			for path, want := range map[string][]byte{authPath: auth, pending: []byte(`{"parked":true}`)} {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatalf("watch changed %s (-want +got):\n%s", path, diff)
				}
			}
			if files, err := os.ReadDir(filepath.Join(paths.CodexRoot(), ".state")); err != nil || len(files) != 0 {
				t.Fatalf("watch created refresh state: entries=%v error=%v", files, err)
			}
		})
	}
}
