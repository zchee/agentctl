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
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/zchee/agentctl/fixtures"
)

func init() {
	registerScriptCmd("swap-status-list", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-status-list <namespace-dir>")
		}
		ts.Check(os.WriteFile(ts.Getenv("AGCTL_FAKE_SECURITY_DUMP"), []byte(dumpListing(MigrationService(ts.MkAbs(args[0])))), 0o600))
	})
	registerScriptCmd("swap-status-blob", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 5 || args[4] != "fresh" && args[4] != "expired" {
			ts.Fatalf("usage: swap-status-blob <path> <name> <account> <email> <fresh|expired>")
		}
		expiry := FreshAt()
		if args[4] == "expired" {
			expiry = ExpiredAt()
		}
		body, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": "sk-ant-oat01-SENTINEL-" + args[1], "refreshToken": "sk-ant-ort01-SENTINEL-" + args[1],
			"expiresAt": expiry, "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max",
			"tokenAccount": map[string]string{"uuid": args[2], "organizationUuid": Org, "emailAddress": args[3], "organizationName": "Acme"},
		}})
		ts.Check(err)
		ts.Check(os.WriteFile(ts.MkAbs(args[0]), body, 0o600))
	})
	registerScriptCmd("swap-status-row", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: swap-status-row <report> <account> <occupant-email>")
		}
		body := ts.ReadFile(args[0])
		if strings.Contains(body, "SENTINEL") || strings.Contains(body, "sk-ant-") {
			ts.Fatalf("status report contains a fixture credential")
		}
		var doc struct {
			Rows []struct {
				Kind       string  `json:"kind"`
				Account    string  `json:"account_uuid"`
				State      string  `json:"state"`
				OccupiedBy *string `json:"occupied_by"`
				Note       *string `json:"note"`
				Lock       string  `json:"lock_state"`
			} `json:"rows"`
		}
		ts.Check(json.Unmarshal([]byte(body), &doc))
		matched := 0
		for _, row := range doc.Rows {
			if row.Kind != "owned" || row.Account != args[1] {
				continue
			}
			matched++
			if row.State != "adopted" || row.OccupiedBy == nil || *row.OccupiedBy != args[2] || row.Note == nil || *row.Note != "its keychain item is held by another identity" || row.Lock != "none" {
				ts.Fatalf("owned row does not preserve occupied state and unlocked attribution")
			}
		}
		if matched != 1 {
			ts.Fatalf("observed %d owned rows for requested account; want 1", matched)
		}
	})
	registerScriptCmd("swap-status-server", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 || args[0] != "usage" && args[0] != "invalid-grant" {
			ts.Fatalf("usage: swap-status-server <usage|invalid-grant>")
		}
		usage, err := fixtures.FS.ReadFile("claude/usage-2026-09-08.json")
		ts.Check(err)
		var posts atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case UsagePath:
				_, _ = w.Write(usage)
			case TokenPath:
				posts.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			default:
				http.NotFound(w, r)
			}
		}))
		ts.Defer(server.Close)
		ts.Setenv("AGENTCTL_CLAUDE_USAGE_URL", server.URL)
		if args[0] == "invalid-grant" {
			ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+TokenPath)
		}
		ts.SetCmd("swap-status-posts", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 1 {
				ts.Fatalf("usage: swap-status-posts <count>")
			}
			want, err := strconv.ParseInt(args[0], 10, 64)
			ts.Check(err)
			if got := posts.Load(); got != want {
				ts.Fatalf("status refresh POST count=%d; want %d", got, want)
			}
		})
	})
}
