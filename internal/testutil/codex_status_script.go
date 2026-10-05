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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
)

const (
	codexStatusUser        = "user-AbCdEf0123456789"
	codexStatusAccount     = "11111111-2222-4333-8444-555555555555"
	codexStatusLiveAccount = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	codexStatusEmail       = "codex-owner@example.invalid"
)

func init() { registerScriptCmd("codex-status-fixture", codexStatusFixture) }

func codexStatusJWT(ts *testscript.TestScript, claims any, signature string) string {
	body, err := json.Marshal(claims)
	ts.Check(err)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(body) + "." + signature
}

func codexStatusAuth(ts *testscript.TestScript, account string, expired, fedramp bool) map[string]any {
	expiry := time.Now().Add(24 * time.Hour).Unix()
	if expired {
		expiry = 1000
	}
	return map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      codexStatusJWT(ts, map[string]any{"email": codexStatusEmail, "https://api.openai.com/auth": map[string]any{"chatgpt_user_id": codexStatusUser, "chatgpt_account_id": account, "chatgpt_plan_type": "pro", "chatgpt_account_is_fedramp": fedramp}}, "agentctl-test-codex-jwt-sig"),
			"access_token":  codexStatusJWT(ts, map[string]any{"exp": expiry}, "agentctl-test-codex-at-sig"),
			"refresh_token": "agentctl-test-codex-rt-0001", "account_id": account,
		}, "last_refresh": "2026-09-16T00:00:00Z",
	}
}

func codexStatusFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: codex-status-fixture <kind>")
	}
	kind := args[0]
	sequence, _ := strconv.Atoi(ts.Getenv("C_SEQUENCE"))
	sequence++
	ts.Setenv("C_SEQUENCE", strconv.Itoa(sequence))
	root := ts.MkAbs(filepath.Join("codex-status-cases", fmt.Sprintf("%s-%d", kind, sequence)))
	home, config := filepath.Join(root, "home"), filepath.Join(root, "config")
	ns := filepath.Join(config, "codex", codexStatusUser, codexStatusAccount)
	live := filepath.Join(home, ".codex")
	for _, dir := range []string{home, config} {
		ts.Check(os.MkdirAll(dir, 0o700))
	}
	for key, value := range map[string]string{
		"HOME": home, "AGENTCTL_CONFIG_DIR": config, "TZ": "UTC",
		"CODEX_HOME": "", "AGENTCTL_FAULT": "", "AGENTCTL_FAULT_RESUME": filepath.Join(root, "resume"),
		"AGENTCTL_LOG": "warn",
		"C_USER":       codexStatusUser, "C_ACCOUNT": codexStatusAccount, "C_EMAIL": codexStatusEmail,
		"C_NAMESPACE": ns, "C_LIVE": live, "C_CONFIG": config,
		"C_MARKER":                 filepath.Join(config, "codex", ".state", codexStatusUser+"+"+codexStatusAccount+".refresh"),
		"C_AUDIT":                  filepath.Join(config, "codex", "writes.jsonl"),
		"AGCTL_FAKE_SECURITY_DUMP": filepath.Join(root, "dump"), "AGCTL_FAKE_SECURITY_LOG": filepath.Join(root, "security.log"),
	} {
		ts.Setenv(key, value)
	}
	ts.Check(os.WriteFile(filepath.Join(root, "dump"), nil, 0o600))
	write := func(path string, value any) {
		body, err := json.Marshal(value)
		ts.Check(err)
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, body, 0o600))
	}
	expired := strings.Contains(kind, "expired") || strings.Contains(kind, "never")
	doc := codexStatusAuth(ts, codexStatusAccount, expired, kind == "fedramp")
	owned := strings.Contains(kind, "owned") || kind == "twins" || strings.Contains(kind, "never") || kind == "accounts" || kind == "torn"
	records := []map[string]any{}
	if owned {
		policy := "auto"
		if strings.Contains(kind, "never") {
			policy = "never"
		}
		records = append(records, map[string]any{"chatgpt_user_id": codexStatusUser, "chatgpt_account_id": codexStatusAccount, "email": codexStatusEmail, "plan_type": "pro", "kind": map[string]any{"kind": "owned", "export_spelling": ns, "refresh": policy}, "forgotten": false, "created_at": "2026-09-17T00:00:00Z"})
		write(filepath.Join(ns, "auth.json"), doc)
	}
	switch kind {
	case "owned", "owned-expired", "never", "accounts":
	case "empty":
	case "empty-home":
		ts.Check(os.MkdirAll(live, 0o700))
	case "torn":
		ts.Check(os.WriteFile(filepath.Join(ns, "auth.json"), []byte(`{"tokens":{"access_to`), 0o600))
		ts.Check(os.MkdirAll(live, 0o700))
		ts.Check(os.WriteFile(filepath.Join(live, "auth.json"), []byte(`{"tokens":{"access_to`), 0o600))
	case "twins":
		write(filepath.Join(live, "auth.json"), codexStatusAuth(ts, codexStatusLiveAccount, false, false))
	case "live", "live-expired", "fedramp":
		write(filepath.Join(live, "auth.json"), doc)
	case "apikey", "apikey-never":
		write(filepath.Join(live, "auth.json"), map[string]any{"OPENAI_API_KEY": "agentctl-test-codex-ak-0001"})
	default:
		ts.Fatalf("unknown Codex status fixture %q", kind)
	}
	write(filepath.Join(config, "config.json"), map[string]any{"version": 2, "accounts": []any{}, "forgotten_services": []string{}, "codex_accounts": records})

	var mu sync.Mutex
	usageCode, tokenCode, usageCalls, tokenCalls := 200, 200, 0, 0
	accounts := map[string]int{}
	headers := []http.Header{}
	grant := map[string]any{"access_token": codexStatusJWT(ts, map[string]any{"exp": time.Now().Add(10 * 24 * time.Hour).Unix()}, "agentctl-test-codex-at-new"), "refresh_token": "agentctl-test-codex-rt-0002", "id_token": codexStatusJWT(ts, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": codexStatusUser, "chatgpt_account_id": codexStatusAccount}}, "agentctl-test-codex-jwt-new"), "expires_in": 864000}
	grantBody, err := json.Marshal(grant)
	ts.Check(err)
	usageBody, err := json.Marshal(map[string]any{"plan_type": "pro", "email": "agentctl-test-codex-email-0001", "user_id": codexStatusUser, "rate_limit": map[string]any{"allowed": true, "limit_reached": false, "primary_window": map[string]any{"used_percent": 42.0, "limit_window_seconds": 18000, "reset_after_seconds": 3600}, "secondary_window": map[string]any{"used_percent": 7.0, "limit_window_seconds": 604800, "reset_after_seconds": 86400}}, "credits": map[string]any{"has_credits": true, "unlimited": false, "balance": "0"}})
	ts.Check(err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls++
			accounts[r.Header.Get("Chatgpt-Account-Id")]++
			headers = append(headers, r.Header.Clone())
			w.WriteHeader(usageCode)
			if usageCode == 200 {
				var body map[string]any
				if err := json.Unmarshal(usageBody, &body); err != nil {
					panic(err)
				}
				body["account_id"] = r.Header.Get("Chatgpt-Account-Id")
				encoded, err := json.Marshal(body)
				if err != nil {
					panic(err)
				}
				_, _ = w.Write(encoded)
			}
		case "/oauth/token":
			tokenCalls++
			w.WriteHeader(tokenCode)
			switch tokenCode {
			case 200:
				_, _ = w.Write(grantBody)
			case 400:
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	ts.Defer(server.Close)
	ts.Setenv("AGENTCTL_CODEX_USAGE_URL", server.URL)
	ts.Setenv("AGENTCTL_CODEX_TOKEN_URL", server.URL+"/oauth/token")
	ts.Setenv("C_TOKEN_URL", server.URL+"/oauth/token")
	ts.SetCmd("codex-status-response", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: codex-status-response <usage|token> <code>")
		}
		code, err := strconv.Atoi(args[1])
		ts.Check(err)
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "usage":
			usageCode = code
		case "token":
			tokenCode = code
		default:
			ts.Fatalf("unknown response %q", args[0])
		}
	})
	ts.SetCmd("codex-status-calls", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: codex-status-calls <usage-count> <token-count>")
		}
		mu.Lock()
		defer mu.Unlock()
		for i, got := range []int{usageCalls, tokenCalls} {
			want, err := strconv.Atoi(args[i])
			ts.Check(err)
			if got != want {
				ts.Fatalf("request count %d = %d, want %d", i, got, want)
			}
		}
	})
	ts.SetCmd("codex-status-headers", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: codex-status-headers <fedramp-boolean>")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(headers) != 1 {
			ts.Fatalf("headers count = %d, want 1", len(headers))
		}
		h := headers[0]
		if !strings.HasPrefix(h.Get("Authorization"), "Bearer ") || h.Get("Accept") != "application/json" || h.Get("Chatgpt-Account-Id") != codexStatusAccount || h.Get("User-Agent") == "" {
			ts.Fatalf("required usage headers missing")
		}
		expected := ""
		if args[0] == "true" {
			expected = "true"
		}
		if h.Get("X-Openai-Fedramp") != expected {
			ts.Fatalf("FedRAMP header = %q, want %q", h.Get("X-Openai-Fedramp"), expected)
		}
	})
	ts.SetCmd("codex-account-setup", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: codex-account-setup <seed|no-namespace|claude-row|readonly-rows>")
		}
		switch args[0] {
		case "seed":
			for _, name := range []string{"auth.json.pending", "auth.pending.meta", "auth.json.tmp.0badc0de"} {
				write(filepath.Join(ns, name), map[string]any{})
			}
			write(ts.Getenv("C_MARKER"), map[string]any{})
		case "no-namespace":
			ts.Check(os.Remove(filepath.Join(ns, "auth.json")))
			ts.Check(os.Remove(ns))
		case "claude-row", "readonly-rows":
			var registry map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(config, "config.json"))), &registry))
			if args[0] == "claude-row" {
				registry["accounts"] = []any{map[string]any{"account_uuid": Acct, "organization_uuid": Org, "email": "claude-only@example.invalid", "kind": map[string]any{"kind": "owned", "export_spelling": "/x", "export_sha8": "deadbeef"}, "forgotten": false, "created_at": "2026-09-17T00:00:00Z"}}
			} else {
				template := records[0]
				liveRow := map[string]any{}
				imported := map[string]any{}
				for key, value := range template {
					liveRow[key] = value
					imported[key] = value
				}
				liveRow["chatgpt_user_id"] = "user-live"
				liveRow["chatgpt_account_id"] = "acct-live"
				liveRow["email"] = "live@example.invalid"
				liveRow["kind"] = map[string]any{"kind": "live"}
				imported["chatgpt_user_id"] = "user-imported"
				imported["chatgpt_account_id"] = "acct-imported"
				imported["email"] = "imported@example.invalid"
				imported["kind"] = map[string]any{"kind": "home_read_only", "dir": live}
				registry["codex_accounts"] = []any{liveRow, imported}
				write(filepath.Join(live, "auth.json"), doc)
			}
			write(filepath.Join(config, "config.json"), registry)
		default:
			ts.Fatalf("unknown account setup %q", args[0])
		}
	})
	manifests := map[string][]codexStatusEntry{}
	ts.SetCmd("codex-manifest", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: codex-manifest <save|check> <directory>")
		}
		dir := ts.MkAbs(args[1])
		entries, err := codexStatusManifest(dir)
		ts.Check(err)
		switch args[0] {
		case "save":
			manifests[dir] = entries
		case "check":
			previous, ok := manifests[dir]
			if !ok {
				ts.Fatalf("no saved manifest for %s", dir)
			}
			if diff := gocmp.Diff(previous, entries); diff != "" {
				ts.Fatalf("namespace manifest changed (-before +after):\n%s", diff)
			}
		default:
			ts.Fatalf("unknown manifest operation %q", args[0])
		}
	})
	ts.SetCmd("codex-status-action", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: codex-status-action <wait|resume|rotate|pending|daemon|term|int>")
		}
		switch args[0] {
		case "wait":
			ts.Check(waitCodexMarker(ts.Getenv("AGENTCTL_FAULT_RESUME")+".reached", 5*time.Second))
		case "resume":
			ts.Check(os.WriteFile(ts.Getenv("AGENTCTL_FAULT_RESUME"), []byte("go"), 0o600))
		case "rotate", "pending":
			replacement := codexStatusAuth(ts, codexStatusAccount, false, false)
			replacement["tokens"].(map[string]any)["refresh_token"] = "agentctl-test-codex-rt-0009"
			path := filepath.Join(ns, "auth.json")
			if args[0] == "pending" {
				path += ".pending"
			}
			write(path, replacement)
		case "live":
			write(filepath.Join(live, "auth.json"), codexStatusAuth(ts, codexStatusAccount, false, false))
		case "daemon":
			write(filepath.Join(ns, "app-server-daemon", "app-server.pid"), map[string]any{"pid": os.Getpid()})
		case "term", "int":
			background := ts.BackgroundCmds()
			if len(background) != 1 {
				ts.Fatalf("expected one background process, got %d", len(background))
			}
			signal := syscall.SIGTERM
			if args[0] == "int" {
				signal = syscall.SIGINT
			}
			ts.Check(background[0].Process.Signal(signal))
		default:
			ts.Fatalf("unknown Codex status action %q", args[0])
		}
	})
	ts.SetCmd("codex-status-check", codexStatusCheck)
}

type codexStatusEntry struct {
	Name           string
	Mode           fs.FileMode
	Size, Modified int64
	Digest         string
}

func codexStatusManifest(dir string) ([]codexStatusEntry, error) {
	var entries []codexStatusEntry
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		value := codexStatusEntry{Name: relative, Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime().UnixNano()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(data)
			value.Digest = hex.EncodeToString(digest[:])
		}
		entries = append(entries, value)
		return nil
	})
	return entries, err
}

func waitCodexMarker(path string, budget time.Duration) error {
	timeout := time.NewTimer(budget)
	defer timeout.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-tick.C:
		case <-timeout.C:
			return fmt.Errorf("the process never reached marker %s", path)
		}
	}
}

func codexStatusCheck(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 {
		ts.Fatalf("usage: codex-status-check <json|audit|needles|namespace> <file>")
	}
	switch args[0] {
	case "marker":
		var state map[string]any
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &state))
		if len(args) != 4 {
			ts.Fatalf("marker requires field and expected value")
		}
		if args[3] == "object" {
			if _, ok := state[args[2]].(map[string]any); !ok {
				ts.Fatalf("marker field %s is not an object", args[2])
			}
		} else if fmt.Sprint(state[args[2]]) != args[3] {
			ts.Fatalf("marker field %s differs: %v", args[2], state[args[2]])
		}
	case "stored":
		if len(args) != 3 {
			ts.Fatalf("stored requires file and token suffix")
		}
		var document struct {
			Tokens struct {
				Refresh string `json:"refresh_token"`
			} `json:"tokens"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &document))
		if document.Tokens.Refresh != "agentctl-test-codex-rt-"+args[2] {
			ts.Fatalf("stored refresh grant differs")
		}
	case "staged":
		entries, err := os.ReadDir(ts.MkAbs(args[1]))
		ts.Check(err)
		staged := []string{}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".tmp.") {
				staged = append(staged, entry.Name())
			}
		}
		if len(staged) != 1 {
			ts.Fatalf("expected one recoverable staged grant, got %d", len(staged))
		}
		codexStatusCheck(ts, false, []string{"stored", filepath.Join(args[1], staged[0]), "0002"})
	case "needles":
		for _, file := range args[1:] {
			text := ts.ReadFile(file)
			for _, needle := range []string{"agentctl-test-codex-at-", "agentctl-test-codex-rt-", "agentctl-test-codex-ak-", "agentctl-test-codex-jwt-", "agentctl-test-codex-email-", "eyJ", "Bearer ", "bearer "} {
				if strings.Contains(text, needle) {
					ts.Fatalf("output %s leaked %q", file, needle)
				}
			}
		}
	case "namespace":
		entries, err := os.ReadDir(ts.MkAbs(args[1]))
		ts.Check(err)
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if diff := gocmp.Diff([]string{"auth.json"}, names); diff != "" {
			ts.Fatalf("namespace entries (-want +got):\n%s", diff)
		}
	case "json":
		var document map[string]any
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &document))
		rows, ok := document["rows"].([]any)
		if !ok {
			ts.Fatalf("missing rows")
		}
		if len(args) < 3 {
			ts.Fatalf("usage: codex-status-check json <file> <row-count> [state]")
		}
		expected, err := strconv.Atoi(args[2])
		ts.Check(err)
		if len(rows) != expected {
			ts.Fatalf("rows = %d, want %d", len(rows), expected)
		}
		for _, value := range rows {
			row := value.(map[string]any)
			if row["provider"] != "codex" {
				ts.Fatalf("wrong provider: %v", row)
			}
			if len(args) > 3 && row["state"] != args[3] {
				ts.Fatalf("state = %v, want %s", row["state"], args[3])
			}
			identity := row["identity"].(map[string]any)
			if expected == 2 && row["id"] != fmt.Sprint(identity["user_id"], "/", identity["account_id"]) {
				ts.Fatalf("duplicate user id not qualified: %v", row)
			}
			if raw, ok := document["raw"].(map[string]any); ok {
				body, ok := raw[row["id"].(string)].(map[string]any)
				if !ok {
					ts.Fatalf("raw missing own row id")
				}
				if body["account_id"] != identity["account_id"] {
					ts.Fatalf("raw account differs from its row")
				}
				if _, exists := body["email"]; exists {
					ts.Fatalf("raw retained email")
				}
			}
		}
	case "audit":
		lines := strings.FieldsFunc(ts.ReadFile(args[1]), func(r rune) bool { return r == '\n' })
		if len(lines) != 1 {
			ts.Fatalf("audit lines = %d, want 1", len(lines))
		}
		var event map[string]any
		ts.Check(json.Unmarshal([]byte(lines[0]), &event))
		if strings.ContainsRune(lines[0], '@') || event["provider"] != "codex" {
			ts.Fatalf("audit carries an address or wrong provider")
		}
		info, err := os.Stat(ts.MkAbs(args[1]))
		ts.Check(err)
		if info.Mode().Perm() != 0o600 {
			ts.Fatalf("audit mode is not 0600")
		}
	default:
		ts.Fatalf("unknown Codex status check %q", args[0])
	}
}
