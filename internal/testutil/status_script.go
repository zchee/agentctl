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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"

	"github.com/zchee/agentctl/fixtures"
)

// ScriptCmds returns in-process commands whose resources live for one script.
// statusfixture creates an isolated account store and a loopback usage server;
// usage-status selects its HTTP response, usage-calls checks request counts,
// and status-check checks reports, reset cells, and read-only keychain access.
// No helper reads the user's home, keychain, or network credentials.
func ScriptCmds() map[string]func(ts *testscript.TestScript, neg bool, args []string) {
	return map[string]func(*testscript.TestScript, bool, []string){
		"statusfixture": statusFixture,
	}
}

func statusFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: statusfixture <empty|owned|expired-live|locked-live|unknown|sibling|identical|twins|identity|hanging>")
	}
	kind := args[0]
	root := ts.MkAbs(filepath.Join("status-cases", kind))
	for _, dir := range []string{"config", "home", "items"} {
		ts.Check(os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	for key, value := range map[string]string{
		"AGENTCTL_CONFIG_DIR":                filepath.Join(root, "config"),
		"HOME":                               filepath.Join(root, "home"),
		"AGCTL_FAKE_SECURITY_ITEMS":          filepath.Join(root, "items"),
		"AGCTL_FAKE_SECURITY_DUMP":           filepath.Join(root, "dump"),
		"AGCTL_FAKE_SECURITY_LOG":            filepath.Join(root, "security.log"),
		"AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT": "0",
		"AGCTL_FAKE_SECURITY_FIND_EXIT":      "",
		"AGCTL_FAKE_SECURITY_SLEEP":          "",
		"TZ":                                 "UTC",
	} {
		ts.Setenv(key, value)
	}
	now := time.Now().UTC().Truncate(time.Second)
	session := now.Add(time.Hour + 12*time.Minute + 30*time.Second)
	weekly := now.Add(54*time.Hour + 30*time.Minute)
	body, err := fixtures.FS.ReadFile("claude/usage-2026-09-08.json")
	ts.Check(err)
	var document map[string]any
	ts.Check(json.Unmarshal(body, &document))
	document["five_hour"].(map[string]any)["resets_at"] = session.Format(time.RFC3339)
	document["seven_day"].(map[string]any)["resets_at"] = weekly.Format(time.RFC3339)
	for _, raw := range document["limits"].([]any) {
		limit := raw.(map[string]any)
		reset := weekly
		if limit["kind"] == "session" {
			reset = session
		}
		limit["resets_at"] = reset.Format(time.RFC3339)
	}
	body, err = json.Marshal(document)
	ts.Check(err)
	var status atomic.Int64
	status.Store(http.StatusOK)
	var usageCalls, tokenCalls, profileCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case UsagePath:
			usageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			code := int(status.Load())
			if code == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "30")
			}
			w.WriteHeader(code)
			if code == http.StatusOK {
				_, _ = w.Write(body)
			} else {
				_, _ = w.Write([]byte("{}"))
			}
		case TokenPath:
			tokenCalls.Add(1)
			http.Error(w, "unexpected refresh", http.StatusInternalServerError)
		case ProfilePath:
			profileCalls.Add(1)
			http.Error(w, "unexpected profile request", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	ts.Defer(server.Close)
	ts.Setenv("AGENTCTL_CLAUDE_USAGE_URL", server.URL)
	ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+TokenPath)
	ts.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", server.URL+ProfilePath)
	ts.SetCmd("usage-status", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: usage-status <http-status>")
		}
		code, err := strconv.Atoi(args[0])
		ts.Check(err)
		if code < 100 || code > 599 {
			ts.Fatalf("invalid HTTP status: %d", code)
		}
		status.Store(int64(code))
	})
	ts.SetCmd("usage-calls", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: usage-calls <usage-count> <token-count> <profile-count>")
		}
		for i, got := range []int64{usageCalls.Load(), tokenCalls.Load(), profileCalls.Load()} {
			want, err := strconv.ParseInt(args[i], 10, 64)
			ts.Check(err)
			if got != want {
				ts.Fatalf("request count %d = %d, want %d", i, got, want)
			}
		}
	})
	ts.SetCmd("status-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) == 0 {
			ts.Fatalf("usage: status-check <resets|json-resets|identity|same-json|unknown|read-only|bounded|normalized> [files...]")
		}
		checkStatusScript(ts, args, [3]time.Time{now, session, weekly})
	})

	var records []map[string]any
	var services []string
	write := func(path string, value any) {
		body, err := json.Marshal(value)
		ts.Check(err)
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, body, 0o600))
	}
	blob := func(name, acct, org string, expiry int64) map[string]any {
		return map[string]any{"claudeAiOauth": map[string]any{
			"accessToken":      "sk-ant-oat01-SENTINEL-" + name,
			"refreshToken":     "sk-ant-ort01-SENTINEL-" + name,
			"expiresAt":        expiry,
			"scopes":           []string{"user:inference", "user:profile"},
			"subscriptionType": "max",
			"tokenAccount":     map[string]any{"uuid": acct, "organizationUuid": org, "emailAddress": Email, "organizationName": "Acme"},
		}}
	}
	item := func(service string, value any) {
		services = append(services, service)
		write(filepath.Join(root, "items", KeychainAccount, ItemFileName(service)), value)
	}
	owned := func(acct, org, name string) {
		ns := filepath.Join(root, "config", "claude", acct, org)
		spelling := ExportSpelling(ns)
		records = append(records, map[string]any{
			"account_uuid": acct, "organization_uuid": org, "email": Email, "org_name": "Acme",
			"kind":      map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)},
			"forgotten": false, "created_at": now.Format(time.RFC3339),
		})
		write(filepath.Join(ns, ".credentials.json"), blob(name, acct, org, FreshAt()))
	}
	switch kind {
	case "empty":
	case "owned":
		owned(Acct, Org, "owned")
	case "expired-live":
		item(LiveService, blob("live", Acct, Org, ExpiredAt()))
	case "locked-live":
		item(LiveService, blob("live", Acct, Org, FreshAt()))
		ts.Setenv("AGCTL_FAKE_SECURITY_FIND_EXIT", "36")
		write(filepath.Join(root, "home", ".claude", ".credentials.json"), blob("plaintext", "99999999-9999-9999-9999-999999999999", "88888888-8888-8888-8888-888888888888", FreshAt()))
	case "unknown":
		service := LiveService + "-deadbeef"
		legacy := blob("old", Acct, Org, FreshAt())
		delete(legacy["claudeAiOauth"].(map[string]any), "tokenAccount")
		item(service, legacy)
		records = append(records, map[string]any{
			"account_uuid": "44444444-4444-4444-4444-444444444444", "organization_uuid": "55555555-5555-5555-5555-555555555555",
			"email": "read-only@example.com", "kind": map[string]any{"kind": "config_dir_read_only", "dir": "", "service": service, "shares_live_dir": false},
			"forgotten": false, "created_at": now.Format(time.RFC3339),
		})
		write(filepath.Join(root, "home", ".claude.json"), map[string]any{"oauthAccount": map[string]any{
			"accountUuid": "99999999-9999-9999-9999-999999999999", "emailAddress": "borrowed@example.com", "organizationUuid": "88888888-8888-8888-8888-888888888888",
		}})
	case "sibling", "identical":
		real := filepath.Join(root, "home", "real-claude")
		ts.Check(os.MkdirAll(real, 0o700))
		ts.Check(os.Symlink(real, filepath.Join(root, "home", ".claude")))
		canonical, err := filepath.EvalSymlinks(real)
		ts.Check(err)
		live := blob("live", Acct, Org, FreshAt())
		item(LiveService, live)
		if kind == "sibling" {
			live = blob("sibling", Acct, Org, FreshAt())
		}
		item(MigrationService(canonical), live)
	case "twins", "identity":
		item(LiveService, blob("live", Acct, Org, FreshAt()))
		owned(Acct, Org, "owned")
		if kind == "twins" {
			owned("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "ffffffff-0000-4111-8222-333333333333", "stranger")
		}
	case "hanging":
		services = append(services, LiveService)
		ts.Setenv("AGCTL_FAKE_SECURITY_SLEEP", "30")
	default:
		ts.Fatalf("unknown status fixture %q", kind)
	}
	write(filepath.Join(root, "config", "config.json"), map[string]any{"version": 1, "accounts": records, "forgotten_services": []string{}})
	ts.Check(os.WriteFile(filepath.Join(root, "dump"), []byte(dumpListing(services...)), 0o600))
}

func checkStatusScript(ts *testscript.TestScript, args []string, resets [3]time.Time) {
	switch args[0] {
	case "read-only":
		if len(args) != 1 {
			ts.Fatalf("usage: status-check read-only")
		}
		log := ts.ReadFile(ts.Getenv("AGCTL_FAKE_SECURITY_LOG"))
		for line := range strings.SplitSeq(log, "\n") {
			subcommand, _, _ := strings.Cut(line, " ")
			switch subcommand {
			case "", "show-keychain-info", "find-generic-password", "dump-keychain":
			default:
				ts.Fatalf("unexpected keychain command: %s", line)
			}
		}
	case "bounded":
		if len(args) != 1 || time.Since(resets[0]) >= 10*time.Second {
			ts.Fatalf("hanging security exceeded the ten-second process budget")
		}
	case "resets":
		if len(args) != 3 {
			ts.Fatalf("usage: status-check resets <table> <zone>")
		}
		zone, err := time.LoadLocation(args[2])
		ts.Check(err)
		table := ts.ReadFile(args[1])
		var headings, cells []string
		for line := range strings.SplitSeq(table, "\n") {
			if strings.Contains(line, "5h reset") {
				headings = strings.Split(line, "|")
			}
			if strings.Contains(line, Email) {
				cells = strings.Split(line, "|")
				break
			}
		}
		if len(headings) == 0 || len(cells) != len(headings) {
			ts.Fatalf("missing headings or account cells:\n%s", table)
		}
		for i, heading := range headings {
			var want string
			switch strings.TrimSpace(heading) {
			case "5h reset":
				want = expectedResetCell(resets[0], resets[1], zone, "1h12m")
			case "Weekly reset":
				want = expectedResetCell(resets[0], resets[2], zone, "2d6h")
			default:
				continue
			}
			if got := strings.TrimSpace(cells[i]); got != want {
				ts.Fatalf("%s = %q, want %q", heading, got, want)
			}
		}
	case "unknown":
		if len(args) != 2 {
			ts.Fatalf("usage: status-check unknown <table>")
		}
		for line := range strings.SplitSeq(ts.ReadFile(args[1]), "\n") {
			if strings.Contains(line, LiveService+"-deadbeef") {
				if strings.Contains(line, "borrowed@example.com") || strings.Contains(line, "88888888-8888-8888-8888-888888888888") {
					ts.Fatalf("non-live row borrowed the live identity: %s", line)
				}
				return
			}
		}
		ts.Fatalf("missing config-dir row")
	case "normalized":
		if len(args) != 3 {
			ts.Fatalf("usage: status-check normalized <actual> <expected>")
		}
		if diff := gocmp.Diff(string(normalizeEOF([]byte(ts.ReadFile(args[2])))), string(normalizeEOF([]byte(ts.ReadFile(args[1]))))); diff != "" {
			ts.Fatalf("normalized table mismatch (-want +got):\n%s", diff)
		}
	case "json-resets", "identity", "same-json":
		if len(args) != 3 {
			ts.Fatalf("usage: status-check %s <document> <count-or-document>", args[0])
		}
		var document map[string]any
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &document))
		rows := document["rows"].([]any)
		if args[0] == "same-json" {
			var other map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[2])), &other))
			delete(document, "generated_at")
			delete(other, "generated_at")
			if diff := gocmp.Diff(document, other); diff != "" {
				ts.Fatalf("identity flag changed JSON (-plain +folded):\n%s", diff)
			}
			return
		}
		wantRows, err := strconv.Atoi(args[2])
		ts.Check(err)
		if len(rows) != wantRows {
			ts.Fatalf("rows = %d, want %d", len(rows), wantRows)
		}
		marked := 0
		for _, raw := range rows {
			row := raw.(map[string]any)
			if args[0] == "json-resets" {
				for key, want := range map[string]string{"session_reset": resets[1].Format(time.RFC3339), "weekly_reset": resets[2].Format(time.RFC3339), "next_reset": resets[1].Format(time.RFC3339)} {
					if row[key] != want {
						ts.Fatalf("%s = %v, want %s", key, row[key], want)
					}
				}
				continue
			}
			value, exists := row["same_identity_as"]
			if !exists {
				ts.Fatalf("row is missing same_identity_as")
			}
			if row["kind"] == "owned" && row["account_uuid"] == Acct {
				if value != "live" || row["organization_uuid"] != Org {
					ts.Fatalf("owned twin is not marked as live: %v", row)
				}
				marked++
			} else if value != nil {
				ts.Fatalf("unrelated row is marked: %v", row)
			}
		}
		if args[0] == "identity" && marked != 1 {
			ts.Fatalf("marked twin count = %d, want 1", marked)
		}
	default:
		ts.Fatalf("unknown status check %q", args[0])
	}
}

func expectedResetCell(now, reset time.Time, zone *time.Location, countdown string) string {
	now, reset = now.In(zone), reset.In(zone)
	hour := reset.Hour() % 12
	if hour == 0 {
		hour = 12
	}
	meridiem := "AM"
	if reset.Hour() >= 12 {
		meridiem = "PM"
	}
	clock := fmt.Sprintf("%d:%02d %s", hour, reset.Minute(), meridiem)
	if y, m, d := now.Date(); y != reset.Year() || m != reset.Month() || d != reset.Day() {
		days := [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
		clock = fmt.Sprintf("%s %02d:%02d %s", days[reset.Weekday()], hour, reset.Minute(), meridiem)
	}
	return countdown + " (" + clock + ")"
}
