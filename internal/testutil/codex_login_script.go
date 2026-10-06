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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"
)

func init() { registerScriptCmd("codex-import-fixture", codexImportFixture) }

type codexImportEntry struct {
	Name     string
	Mode     fs.FileMode
	Size     int64
	Modified int64
	Digest   string
}

func codexImportManifest(dir string) ([]codexImportEntry, error) {
	var entries []codexImportEntry
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
		value := codexImportEntry{Name: relative, Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime().UnixNano()}
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

func codexImportFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: codex-import-fixture <basic|sealed|dry|live|flags|keyring|auto-listed|auto-unlisted>")
	}
	kind := args[0]
	root := ts.MkAbs(filepath.Join("codex-import", kind))
	home := filepath.Join(root, "source")
	store := filepath.Join(root, "store")
	for _, dir := range []string{home, store, filepath.Join(root, "home")} {
		ts.Check(os.MkdirAll(dir, 0o700))
	}
	ts.Setenv("AGENTCTL_CONFIG_DIR", store)
	ts.Setenv("HOME", filepath.Join(root, "home"))
	ts.Setenv("CODEX_HOME", "")
	ts.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "none")
	if kind == "live" {
		home = filepath.Join(root, "home", ".codex")
		ts.Check(os.MkdirAll(home, 0o700))
	}
	sourceRoot, err := repoRoot()
	ts.Check(err)
	data, err := os.ReadFile(filepath.Join(sourceRoot, "fixtures", "codex", "auth-codex-format.json"))
	ts.Check(err)
	if slices.Contains([]string{"keyring", "auto-listed"}, kind) {
		data = []byte("{ never parsed")
	}
	ts.Check(os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600))
	mode := "file"
	if kind == "keyring" {
		mode = "keyring"
	}
	if strings.HasPrefix(kind, "auto-") {
		mode = "auto"
	}
	ts.Check(os.WriteFile(filepath.Join(home, "config.toml"), []byte("cli_auth_credentials_store = \""+mode+"\"\n"), 0o600))
	canonical, err := filepath.EvalSymlinks(home)
	ts.Check(err)
	if strings.HasPrefix(kind, "auto-") {
		ts.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
		digest := sha256.Sum256([]byte(canonical))
		account := "cli|" + hex.EncodeToString(digest[:8])
		if kind == "auto-unlisted" {
			account = "cli|0000000000000000"
		}
		listing := fmt.Sprintf("keychain: \"login.keychain-db\"\nversion: 512\nclass: \"genp\"\nattributes:\n    \"acct\"<blob>=\"%s\"\n    \"svce\"<blob>=\"Codex Auth\"\n", account)
		ts.Check(os.WriteFile(ts.Getenv("AGCTL_FAKE_SECURITY_DUMP"), []byte(listing), 0o600))
	}
	if kind == "sealed" {
		for _, name := range []string{"auth.json", "config.toml"} {
			ts.Check(os.Chmod(filepath.Join(home, name), 0o400))
		}
		ts.Check(os.Chmod(home, 0o500))
		ts.Defer(func() { ts.Check(os.Chmod(home, 0o700)) })
	}
	ts.Setenv("CODEX_IMPORT_HOME", home)
	ts.Setenv("CODEX_IMPORT_REAL", canonical)
	ts.Setenv("CODEX_IMPORT_LINK", filepath.Join(root, "linked"))
	ts.Setenv("CODEX_IMPORT_MISSING", filepath.Join(root, "missing"))
	ts.Setenv("CODEX_IMPORT_FILE", filepath.Join(root, "file"))
	if kind == "flags" {
		ts.Check(os.Symlink(home, ts.Getenv("CODEX_IMPORT_LINK")))
		ts.Check(os.WriteFile(ts.Getenv("CODEX_IMPORT_FILE"), []byte("x"), 0o600))
	}
	sourceBefore, err := codexImportManifest(home)
	ts.Check(err)
	storeBefore, err := codexImportManifest(store)
	ts.Check(err)
	var registryBefore []byte
	var registryModified int64
	var usageCalls atomic.Int64
	if kind == "live" {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" {
				http.NotFound(w, r)
				return
			}
			usageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":42.0,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":7.0,"limit_window_seconds":604800,"reset_after_seconds":86400}},"credits":{"has_credits":true,"unlimited":false,"balance":"0"}}`)
		}))
		ts.Defer(server.Close)
		ts.Setenv("AGENTCTL_CODEX_USAGE_URL", server.URL)
	}
	ts.SetCmd("codex-import-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: codex-import-check <recorded|unchanged|dry|refused|snapshot|repeated|folded|stale>")
		}
		switch args[0] {
		case "unchanged":
			after, err := codexImportManifest(home)
			ts.Check(err)
			if diff := gocmp.Diff(sourceBefore, after); diff != "" {
				ts.Fatalf("source changed:\n%s", diff)
			}
		case "dry":
			after, err := codexImportManifest(store)
			ts.Check(err)
			if diff := gocmp.Diff(storeBefore, after); diff != "" {
				ts.Fatalf("dry run changed store:\n%s", diff)
			}
		case "refused":
			if _, err := os.Stat(filepath.Join(store, "config.json")); !os.IsNotExist(err) {
				ts.Fatalf("refusal recorded an account: %v", err)
			}
		case "snapshot":
			registryBefore, err = os.ReadFile(filepath.Join(store, "config.json"))
			ts.Check(err)
			info, err := os.Stat(filepath.Join(store, "config.json"))
			ts.Check(err)
			registryModified = info.ModTime().UnixNano()
		case "repeated":
			after, err := os.ReadFile(filepath.Join(store, "config.json"))
			ts.Check(err)
			info, err := os.Stat(filepath.Join(store, "config.json"))
			ts.Check(err)
			if diff := gocmp.Diff(registryBefore, after); diff != "" {
				ts.Fatalf("registry changed:\n%s", diff)
			}
			if info.ModTime().UnixNano() != registryModified {
				ts.Fatalf("registry was rewritten")
			}
		case "recorded":
			var registry struct {
				CodexAccounts []struct {
					ChatGPTUserID    string  `json:"chatgpt_user_id"`
					ChatGPTAccountID string  `json:"chatgpt_account_id"`
					Email            *string `json:"email"`
					PlanType         *string `json:"plan_type"`
					Forgotten        bool    `json:"forgotten"`
					CreatedAt        string  `json:"created_at"`
					Kind             struct {
						Kind string `json:"kind"`
						Dir  string `json:"dir"`
					} `json:"kind"`
				} `json:"codex_accounts"`
			}
			document := []byte(ts.ReadFile(filepath.Join(store, "config.json")))
			ts.Check(json.Unmarshal(document, &registry))
			if len(registry.CodexAccounts) != 1 {
				ts.Fatalf("records=%d", len(registry.CodexAccounts))
			}
			row := registry.CodexAccounts[0]
			_, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
			ts.Check(err)
			if row.ChatGPTUserID != "user-0001" || row.ChatGPTAccountID != "11111111-2222-4333-8444-555555555555" || row.Email == nil || *row.Email != "codex-user@example.invalid" || row.PlanType == nil || *row.PlanType != "pro" || row.Forgotten || row.Kind.Kind != "home_read_only" || row.Kind.Dir != canonical {
				ts.Fatalf("wrong metadata: %+v", row)
			}
			for _, needle := range []string{"agctl-test-codex-rt-", "agctl-test-codex-at-", "eyJ", "Bearer "} {
				if strings.Contains(string(document), needle) {
					ts.Fatalf("registry contains credential material")
				}
			}
			if _, err := os.Stat(filepath.Join(store, "codex")); !os.IsNotExist(err) {
				ts.Fatalf("import created a Codex tree: %v", err)
			}
		case "folded", "stale":
			var report struct {
				Rows []struct {
					Kind  string         `json:"kind"`
					State string         `json:"state"`
					Note  *string        `json:"note"`
					Usage jsontext.Value `json:"usage"`
				} `json:"rows"`
			}
			ts.Check(json.Unmarshal([]byte(ts.ReadFile("codex-status.json")), &report))
			found := false
			for _, row := range report.Rows {
				if row.Kind != "home_read_only" {
					continue
				}
				found = true
				if args[0] == "folded" {
					if row.Note == nil || !strings.Contains(*row.Note, "same credential as live") || row.State == "stale_sibling_of_live" {
						ts.Fatalf("imported row was not folded")
					}
				} else if row.State != "stale_sibling_of_live" || string(row.Usage) != "null" {
					ts.Fatalf("imported row is not stale without usage")
				}
			}
			if !found {
				ts.Fatalf("missing imported row")
			}
			if usageCalls.Load() == 0 {
				ts.Fatalf("live usage was not fetched")
			}
		default:
			ts.Fatalf("unknown check %q", args[0])
		}
	})
	ts.SetCmd("codex-import-switch", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: codex-import-switch")
		}
		original, err := os.ReadFile(filepath.Join(sourceRoot, "fixtures", "codex", "auth-codex-format.json"))
		ts.Check(err)
		// Replacing the workspace id makes the imported record name a stale sibling.
		original = []byte(strings.ReplaceAll(string(original), "11111111-2222-4333-8444-555555555555", "66666666-7777-4888-8999-aaaaaaaaaaaa"))
		ts.Check(os.WriteFile(filepath.Join(home, "auth.json"), original, 0o600))
	})
}

func init() {
	registerScriptCmd("codex-login-fixture", codexLoginFixture)
	registerScriptCmd("codex-login-oracle-control", codexLoginOracleControl)
}

func codexLoginOutputViolation(text string) (string, bool) {
	for _, pair := range []struct{ name, needle string }{{"the access token", "agctl-test-codex-at-"}, {"the refresh token", "agctl-test-codex-rt-"}, {"the api key", "agctl-test-codex-ak-"}, {"the JWT signature", "agctl-test-codex-login-sig"}, {"a JWT header", "eyJ"}, {"a Bearer header", "Bearer "}, {"a bearer header", "bearer "}, {"a hostile lock", "$(id)"}, {"a hostile escape", "\x1b]0;"}} {
		if offset := strings.Index(text, pair.needle); offset >= 0 {
			return fmt.Sprintf("`%s` at byte %d", pair.name, offset), true
		}
	}
	return "", false
}

func codexLoginOutputError(name, stdout, stderr string) error {
	for _, stream := range []struct{ name, text string }{{"stdout", stdout}, {"stderr", stderr}} {
		if problem, ok := codexLoginOutputViolation(stream.text); ok {
			return fmt.Errorf("%s: %s carries the needle %s", name, stream.name, problem)
		}
	}
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.Contains(line, "unaudited write receipt:") {
			return fmt.Errorf("%s: the binary dropped a Codex write receipt before the audit log: %s", name, line)
		}
	}
	return nil
}

func codexLoginOracleControl(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: codex-login-oracle-control <needle|receipt>")
	}
	switch args[0] {
	case "needle":
		got := codexLoginOutputError("positive-control", "before agctl-test-codex-rt-0001 after", "")
		if got == nil || got.Error() != "positive-control: stdout carries the needle `the refresh token` at byte 7" {
			ts.Fatalf("needle oracle did not reject by name and offset")
		}
	case "receipt":
		got := codexLoginOutputError("positive-control-receipt", "", "agentctl unaudited write receipt: Delete was dropped before codex::audit::append\n")
		if got == nil || !strings.Contains(got.Error(), "the binary dropped a Codex write receipt before the audit log") {
			ts.Fatalf("receipt oracle failed to reject")
		}
	default:
		ts.Fatalf("unknown oracle control %q", args[0])
	}
	ts.Check(codexLoginOutputError("positive-control-clean", "nothing to see", "nor here"))
}

func codexLoginHasReceipt(text, outcome, user, account string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		var entry struct {
			Provider string `json:"provider"`
			Outcome  string `json:"outcome"`
			User     string `json:"user_id"`
			Account  string `json:"account_id"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Provider == "codex" && entry.Outcome == outcome && entry.User == user && entry.Account == account {
			return true
		}
	}
	return false
}

func codexLoginFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: codex-login-fixture <kind>")
	}
	kind := args[0]
	root := ts.MkAbs(filepath.Join("codex-login", kind))
	home, store := filepath.Join(root, "home"), filepath.Join(root, "store")
	ts.Check(os.MkdirAll(home, 0o700))
	ts.Check(os.MkdirAll(store, 0o700))
	for key, value := range map[string]string{"HOME": home, "CODEX_HOME": filepath.Join(root, "decoy"), "AGENTCTL_CONFIG_DIR": store, "AGENTCTL_FAULT": "", "AGENTCTL_KEYCHAIN_BACKEND": "none", "AGENTCTL_FAKE_CODEX_LOG": filepath.Join(root, "codex.log"), "AGENTCTL_FAKE_CODEX_AUTH": filepath.Join(root, "vendor-auth.json"), "AGENTCTL_FAKE_CODEX_SLEEP": "", "AGENTCTL_FAKE_CODEX_EXIT": "", "AGENTCTL_FAKE_CODEX_DAEMON_DIR": "", "AGENTCTL_FAKE_CODEX_NO_RESIDUE": "", "AGENTCTL_FAKE_CODEX_HELD_LOCK": "", "AGENTCTL_FAKE_CODEX_KEYCHAIN_GAIN": "", "AGENTCTL_FAKE_CODEX_KEYCHAIN_GAIN_ACCOUNT": "", "AGENTCTL_FAKE_CODEX_ODD_LOCK": "", "AGENTCTL_FAKE_CODEX_TOUCH": "", "AGENTCTL_FAKE_CODEX_SENTINEL": "", "AGCTL_FAKE_SECURITY_DUMP_EXIT_IF": "", "AGCTL_FAKE_SECURITY_DUMP_EXIT": "0"} {
		ts.Setenv(key, value)
	}
	source, err := repoRoot()
	ts.Check(err)
	auth, err := os.ReadFile(filepath.Join(source, "fixtures", "codex", "auth-codex-format.json"))
	ts.Check(err)
	vendor := ts.Getenv("AGENTCTL_FAKE_CODEX_AUTH")
	ts.Check(os.WriteFile(vendor, auth, 0o600))
	bin, err := os.ReadFile(filepath.Join(source, "fixtures", "fake-codex.sh"))
	ts.Check(err)
	child := filepath.Join(root, "fake-codex.sh")
	ts.Check(os.WriteFile(child, bin, 0o700))
	ts.Setenv("AGENTCTL_CODEX_BIN", child)
	user, account := "user-0001", "11111111-2222-4333-8444-555555555555"
	ns := filepath.Join(store, "codex", user, account)
	authPath, auditPath := filepath.Join(ns, "auth.json"), filepath.Join(store, "codex", "writes.jsonl")
	scratchRoot := filepath.Join(store, "codex", ".scratch")
	ts.Setenv("CODEX_LOGIN_STORE", store)
	ts.Setenv("CODEX_LOGIN_AUTH", authPath)
	write := func(path string, body []byte) {
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, body, 0o600))
	}
	var original []byte
	var originalInfo os.FileInfo
	var registryBefore []byte
	var registryBeforeInfo os.FileInfo
	expectedRows := 1
	var sentinelBefore []codexImportEntry
	sentinel := filepath.Join(root, "sentinel")
	var expectedGained []string
	switch kind {
	case "normal", "copy", "allowlist", "proxies", "cwd", "argv", "audit", "reset", "first", "other", "adopt", "snapshot-symlink", "snapshot-file", "snapshot-inplace", "signal", "closed-stdout":
	case "failed":
		ts.Setenv("AGENTCTL_FAKE_CODEX_EXIT", "17")
	case "apikey":
		var doc map[string]jsontext.Value
		ts.Check(json.Unmarshal(auth, &doc))
		doc["auth_mode"] = jsontext.Value(`"apikey"`)
		body, err := json.Marshal(doc)
		ts.Check(err)
		write(vendor, body)
	case "daemon":
		ts.Setenv("AGENTCTL_FAKE_CODEX_DAEMON_DIR", "1")
	case "held":
		ts.Setenv("AGENTCTL_FAKE_CODEX_HELD_LOCK", "1")
	case "listings", "gained", "hostile-gained", "listing-failed":
		ts.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
		write(ts.Getenv("AGCTL_FAKE_SECURITY_DUMP"), []byte("keychain: \"login.keychain-db\"\nversion: 512\n"))
		write(ts.Getenv("AGCTL_FAKE_SECURITY_LOG"), nil)
		if kind == "gained" || kind == "hostile-gained" {
			ts.Setenv("AGENTCTL_FAKE_CODEX_KEYCHAIN_GAIN", ts.Getenv("AGCTL_FAKE_SECURITY_DUMP"))
			if kind == "hostile-gained" {
				ts.Setenv("AGENTCTL_FAKE_CODEX_KEYCHAIN_GAIN_ACCOUNT", "cli|$(id)")
			} else {
				expectedGained = []string{"cli|0123456789abcdef"}
			}
		}
		if kind == "listing-failed" {
			ts.Setenv("AGENTCTL_FAKE_CODEX_TOUCH", filepath.Join(root, "after-child"))
			ts.Setenv("AGCTL_FAKE_SECURITY_DUMP_EXIT_IF", filepath.Join(root, "after-child"))
		}
	case "rename-failed":
		original = bytes.ReplaceAll(auth, []byte("agctl-test-codex-rt-0001"), []byte("agctl-test-codex-rt-prior"))
		write(authPath, original)
		originalInfo, err = os.Stat(authPath)
		ts.Check(err)
		ts.Setenv("AGENTCTL_FAULT", "codex_install_rename_fail")
	case "symlink-root":
		write(filepath.Join(sentinel, "agctl-codex-login-deadbeef", "auth.json"), []byte("sentinel"))
		old := time.Now().Add(-30 * time.Minute)
		ts.Check(os.Chtimes(filepath.Join(sentinel, "agctl-codex-login-deadbeef"), old, old))
		ts.Check(os.MkdirAll(filepath.Dir(scratchRoot), 0o700))
		ts.Check(os.Symlink(sentinel, scratchRoot))
		sentinelBefore, err = codexImportManifest(sentinel)
		ts.Check(err)
	case "closed-stderr", "blocked-marker":
		write(filepath.Join(store, "codex", ".state", user+"+"+account+".refresh", "block"), []byte("marker blocker"))
	case "odd-hostile":
		ts.Setenv("AGENTCTL_FAKE_CODEX_ODD_LOCK", "odd-$(id).lock")
	case "odd-safe":
		ts.Setenv("AGENTCTL_FAKE_CODEX_ODD_LOCK", "ordinary.lock")
	default:
		ts.Fatalf("unknown login kind %q", kind)
	}
	if kind == "allowlist" || kind == "proxies" {
		for key, value := range map[string]string{"LANG": "ja_JP.UTF-8", "LC_CTYPE": "ja_JP.UTF-8", "TERM": "xterm-256color", "HTTP_PROXY": "http://proxy.invalid:8080", "HTTPS_PROXY": "http://proxy.invalid:8443", "NO_PROXY": "localhost", "ALL_PROXY": "socks5://proxy.invalid:1080", "http_proxy": "http://lower.invalid:8080", "https_proxy": "http://lower.invalid:8443", "no_proxy": "localhost,127.0.0.1", "all_proxy": "socks5://lower.invalid:1080", "SSL_CERT_FILE": "/a/cert.pem", "SSL_CERT_DIR": "/a/certs", "CODEX_API_KEY": "decoy-api-key", "CODEX_ACCESS_TOKEN": "decoy-access", "CODEX_REFRESH_TOKEN_URL_OVERRIDE": "https://decoy.invalid", "CODEX_APP_SERVER_LOGIN_CLIENT_ID": "decoy-client", "OPENAI_API_KEY": "decoy-openai", "AWS_SECRET_ACCESS_KEY": "decoy-aws", "UNRELATED_SECRET": "decoy-other"} {
			ts.Setenv(key, value)
		}
	}
	if kind == "closed-stdout" {
		write(filepath.Join(home, ".codex", "auth.json"), auth)
		ts.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	ts.SetCmd("codex-login-run", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: codex-login-run <mode> <exit>")
		}
		want, err := strconv.Atoi(args[1])
		ts.Check(err)
		codexLoginProcess(ts, args[0], want, scratchRoot, authPath, auth)
	})
	ts.SetCmd("codex-login-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: codex-login-check <installed|refused|unchanged|audit|reset|second|other|adopt|drop-record>")
		}
		if args[0] == "snapshot" {
			original, err = os.ReadFile(authPath)
			ts.Check(err)
			originalInfo, err = os.Stat(authPath)
			ts.Check(err)
			registryBefore, err = os.ReadFile(filepath.Join(store, "config.json"))
			ts.Check(err)
			registryBeforeInfo, err = os.Stat(filepath.Join(store, "config.json"))
			ts.Check(err)
			write(vendor, bytes.ReplaceAll(auth, []byte("agctl-test-codex-rt-0001"), []byte("agctl-test-codex-rt-new")))
			return
		}
		if args[0] == "drop-record" {
			var registry map[string]jsontext.Value
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(store, "config.json"))), &registry))
			registry["codex_accounts"] = jsontext.Value(`[]`)
			body, err := json.Marshal(registry)
			ts.Check(err)
			write(filepath.Join(store, "config.json"), body)
			return
		}
		if args[0] == "reset" {
			var marker struct {
				Schema   uint32 `json:"schema"`
				Count    uint8  `json:"did_not_help"`
				Resent   bool   `json:"resent"`
				Inflight *any   `json:"inflight"`
				FloorMin uint32 `json:"floor_min"`
			}
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(store, "codex", ".state", user+"+"+account+".refresh"))), &marker))
			if marker.FloorMin != 60 || marker.Count != 0 || marker.Resent || marker.Inflight != nil {
				ts.Fatalf("relogin did not reset the marker")
			}
		}
		ts.Check(codexLoginOutputError(kind, ts.ReadFile("login-out.txt"), ts.ReadFile("login-err.txt")))
		if kind != "symlink-root" {
			leaves, err := os.ReadDir(scratchRoot)
			ts.Check(err)
			if len(leaves) != 0 {
				ts.Fatalf("login left %d scratch homes", len(leaves))
			}
		} else {
			after, err := codexImportManifest(sentinel)
			ts.Check(err)
			if diff := gocmp.Diff(sentinelBefore, after); diff != "" {
				ts.Fatalf("login swept behind a refused root:\n%s", diff)
			}
		}
		if args[0] == "refused" {
			if _, err := os.Stat(authPath); !os.IsNotExist(err) {
				ts.Fatalf("refusal installed a credential: %v", err)
			}
			if _, err := os.Stat(filepath.Join(store, "config.json")); !os.IsNotExist(err) {
				ts.Fatalf("refusal wrote registry: %v", err)
			}
			if len(expectedGained) > 0 {
				text := ts.ReadFile(auditPath)
				for _, account := range expectedGained {
					if !strings.Contains(text, account) || !codexLoginHasReceipt(text, "login_keychain_gained", "none", "none") {
						ts.Fatalf("gained item was not audited")
					}
				}
			}
			return
		}
		if args[0] == "unchanged" {
			data, err := os.ReadFile(authPath)
			ts.Check(err)
			info, err := os.Stat(authPath)
			ts.Check(err)
			if !bytes.Equal(data, original) || originalInfo == nil || !os.SameFile(info, originalInfo) || info.ModTime() != originalInfo.ModTime() {
				ts.Fatalf("failed install changed prior grant")
			}
			if registryBefore != nil {
				after, err := os.ReadFile(filepath.Join(store, "config.json"))
				ts.Check(err)
				afterInfo, err := os.Stat(filepath.Join(store, "config.json"))
				ts.Check(err)
				if !bytes.Equal(after, registryBefore) || afterInfo.ModTime() != registryBeforeInfo.ModTime() {
					ts.Fatalf("refused overwrite changed registry")
				}
			}
			return
		}
		data, err := os.ReadFile(authPath)
		ts.Check(err)
		var installed, expected map[string]jsontext.Value
		ts.Check(json.Unmarshal(data, &installed))
		ts.Check(json.Unmarshal(auth, &expected))
		for _, name := range []string{"tokens", "auth_mode"} {
			if !bytes.Equal(bytes.TrimSpace(installed[name]), bytes.TrimSpace(expected[name])) {
				var x, y any
				ts.Check(json.Unmarshal(installed[name], &x))
				ts.Check(json.Unmarshal(expected[name], &y))
				if !gocmp.Equal(x, y) {
					ts.Fatalf("installed %s differs", name)
				}
			}
		}
		vendorInfo, err := os.Stat(vendor)
		ts.Check(err)
		storedInfo, err := os.Stat(authPath)
		ts.Check(err)
		if os.SameFile(vendorInfo, storedInfo) || storedInfo.Mode().Perm() != 0o600 {
			ts.Fatalf("install was not a private copy")
		}
		audit, err := os.ReadFile(auditPath)
		ts.Check(err)
		if !codexLoginHasReceipt(string(audit), "login_install", user, account) {
			ts.Fatalf("installed credential has no write receipt")
		}
		if args[0] == "audit" && !codexLoginHasReceipt(string(audit), "login_overwrite", user, account) {
			ts.Fatalf("overwrite has no receipt")
		}
		var registry struct {
			Rows []struct {
				User string `json:"chatgpt_user_id"`
				Acct string `json:"chatgpt_account_id"`
				Kind struct {
					Kind    string `json:"kind"`
					Refresh string `json:"refresh"`
					Export  string `json:"export_spelling"`
				} `json:"kind"`
				Created string `json:"created_at"`
			} `json:"codex_accounts"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(store, "config.json"))), &registry))
		if len(registry.Rows) != expectedRows {
			ts.Fatalf("owned registry row count differs")
		}
		found := false
		for _, row := range registry.Rows {
			if row.User == user && row.Acct == account {
				found = true
				if row.Kind.Kind != "owned" || row.Kind.Export != filepath.Dir(authPath) {
					ts.Fatalf("owned registry metadata differs")
				}
				_, err = time.Parse(time.RFC3339Nano, row.Created)
				ts.Check(err)
			}
		}
		if !found {
			ts.Fatalf("owned registry identity is missing")
		}
		if kind == "listings" {
			dump := ts.ReadFile(ts.Getenv("AGCTL_FAKE_SECURITY_LOG"))
			if strings.Count(dump, "dump-keychain") != 2 || strings.Contains(dump, "find-generic-password") {
				ts.Fatalf("login did not take precisely two listing-only calls")
			}
		}
		if slices.Contains([]string{"allowlist", "proxies", "cwd", "argv", "normal"}, kind) {
			log := ts.ReadFile(ts.Getenv("AGENTCTL_FAKE_CODEX_LOG"))
			if !strings.Contains(log, "arg -c\narg cli_auth_credentials_store=\"file\"\narg login\n") {
				ts.Fatalf("vendor argv differs")
			}
			if !strings.Contains(log, "cwd "+scratchRoot+"/") || !strings.Contains(log, "env CODEX_HOME\n") {
				ts.Fatalf("vendor home or cwd differs")
			}
			if !strings.Contains(log, "residue tmp/arg0/") {
				ts.Fatalf("ordinary residue was not exercised")
			}
			for _, name := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "UNRELATED_SECRET"} {
				if strings.Contains(log, "env "+name+"\n") {
					ts.Fatalf("child saw forbidden environment name %s", name)
				}
			}
			if kind == "proxies" {
				for _, name := range []string{"http_proxy", "https_proxy", "no_proxy", "all_proxy"} {
					if !strings.Contains(log, "env "+name+"\n") {
						ts.Fatalf("child lost lowercase proxy %s", name)
					}
				}
			}
		}
	})
	ts.SetCmd("codex-login-other-identity", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: codex-login-other-identity")
		}
		oldPath := authPath
		before, err := os.ReadFile(oldPath)
		ts.Check(err)
		user = "user-other-0002"
		account = "22222222-3333-4444-8555-666666666666"
		var doc map[string]jsontext.Value
		ts.Check(json.Unmarshal(auth, &doc))
		var tokens map[string]jsontext.Value
		ts.Check(json.Unmarshal(doc["tokens"], &tokens))
		for _, name := range []string{"id_token", "access_token"} {
			var token string
			ts.Check(json.Unmarshal(tokens[name], &token))
			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				ts.Fatalf("fixture JWT has no payload")
			}
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			ts.Check(err)
			var claims map[string]jsontext.Value
			ts.Check(json.Unmarshal(payload, &claims))
			var identity map[string]jsontext.Value
			ts.Check(json.Unmarshal(claims["https://api.openai.com/auth"], &identity))
			identity["chatgpt_user_id"], err = json.Marshal(user)
			ts.Check(err)
			identity["user_id"], err = json.Marshal(user)
			ts.Check(err)
			identity["chatgpt_account_id"], err = json.Marshal(account)
			ts.Check(err)
			claims["https://api.openai.com/auth"], err = json.Marshal(identity)
			ts.Check(err)
			payload, err = json.Marshal(claims)
			ts.Check(err)
			parts[1] = base64.RawURLEncoding.EncodeToString(payload)
			tokens[name], err = json.Marshal(strings.Join(parts, "."))
			ts.Check(err)
		}
		tokens["account_id"], err = json.Marshal(account)
		ts.Check(err)
		doc["tokens"], err = json.Marshal(tokens)
		ts.Check(err)
		auth, err = json.Marshal(doc)
		ts.Check(err)
		write(vendor, auth)
		ns = filepath.Join(store, "codex", user, account)
		authPath = filepath.Join(ns, "auth.json")
		expectedRows = 2
		ts.SetCmd("codex-login-prior-unchanged", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 0 {
				ts.Fatalf("usage: codex-login-prior-unchanged")
			}
			after, err := os.ReadFile(oldPath)
			ts.Check(err)
			if !bytes.Equal(before, after) {
				ts.Fatalf("other-account login changed prior credential")
			}
		})
	})
	ts.SetCmd("codex-login-terminal-marker", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: codex-login-terminal-marker")
		}
		write(filepath.Join(store, "codex", ".state", user+"+"+account+".refresh"), []byte(`{"schema":1,"floor_min":240,"did_not_help":3,"resent":true}`))
	})
}

func codexLoginProcess(ts *testscript.TestScript, mode string, want int, scratchRoot, authPath string, original []byte) {
	ctx, cancel := context.WithTimeout(ts.Value(scriptContextKey{}).(context.Context), 20*time.Second)
	defer cancel()
	binary, err := os.Executable()
	ts.Check(err)
	cmd := exec.CommandContext(ctx, binary, "codex", "login")
	cmd.Args[0] = "agentctl"
	cmd.Dir = ts.MkAbs(".")
	for _, name := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "AGENTCTL_CONFIG_DIR", "CODEX_HOME", "AGENTCTL_LOG", "AGENTCTL_KEYCHAIN_BACKEND", "AGENTCTL_SECURITY_BIN", "AGCTL_FAKE_SECURITY_LOG", "AGCTL_FAKE_SECURITY_ITEMS", "AGCTL_FAKE_SECURITY_DUMP", "AGCTL_FAKE_SECURITY_DUMP_EXIT_IF", "AGENTCTL_CODEX_BIN", "AGENTCTL_FAKE_CODEX_LOG", "AGENTCTL_FAKE_CODEX_AUTH", "AGENTCTL_FAKE_CODEX_DAEMON_DIR", "AGENTCTL_FAKE_CODEX_HELD_LOCK", "AGENTCTL_FAKE_CODEX_EXIT", "AGENTCTL_FAKE_CODEX_ODD_LOCK"} {
		cmd.Env = append(cmd.Env, name+"="+ts.Getenv(name))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	paused := strings.HasPrefix(mode, "snapshot-") || mode == "signal"
	resume := ts.MkAbs("login-resume-" + mode)
	if paused {
		cmd.Env = append(cmd.Env, "AGENTCTL_FAULT=pause_codex_login_before_install", "AGENTCTL_FAULT_RESUME="+resume)
	}
	var closedReader, closedWriter *os.File
	if mode == "closed-stdout" || mode == "closed-stderr" {
		closedReader, closedWriter, err = os.Pipe()
		ts.Check(err)
		ts.Check(closedReader.Close())
		defer func() { _ = closedWriter.Close() }()
		if mode == "closed-stdout" {
			cmd.Stdout = closedWriter
		} else {
			cmd.Stderr = closedWriter
		}
	}
	ts.Check(cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if paused {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(resume + ".reached"); err == nil {
				break
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				ts.Fatalf("login never reached verified pause")
			}
		}
		leaves, err := os.ReadDir(scratchRoot)
		ts.Check(err)
		if len(leaves) != 1 {
			ts.Fatalf("paused login has %d homes", len(leaves))
		}
		scratch := filepath.Join(scratchRoot, leaves[0].Name(), "auth.json")
		data, err := os.ReadFile(scratch)
		ts.Check(err)
		if !bytes.Equal(data, original) {
			ts.Fatalf("pause happened before vendor wrote its document")
		}
		switch mode {
		case "snapshot-symlink":
			ts.Check(os.Remove(scratch))
			alternate := ts.MkAbs("alternate-auth.json")
			ts.Check(os.WriteFile(alternate, []byte(`{"auth_mode":"apikey"}`), 0o600))
			ts.Check(os.Symlink(alternate, scratch))
		case "snapshot-file":
			ts.Check(os.Remove(scratch))
			ts.Check(os.WriteFile(scratch, []byte(`{"auth_mode":"apikey"}`), 0o600))
		case "snapshot-inplace":
			ts.Check(os.WriteFile(scratch, []byte(`{"auth_mode":"apikey"}`), 0o600))
		case "signal":
			ts.Check(cmd.Process.Signal(unix.SIGTERM))
		}
		if mode != "signal" {
			ts.Check(os.WriteFile(resume, nil, 0o600))
		}
	}
	err = cmd.Wait()
	exit := 0
	if err != nil {
		if status, ok := errors.AsType[*exec.ExitError](err); ok {
			exit = status.ExitCode()
		} else {
			ts.Check(err)
		}
	}
	ts.Check(codexLoginOutputError(mode, stdout.String(), stderr.String()))
	if exit != want {
		ts.Fatalf("login exit %d, want %d; stderr: %s", exit, want, stderr.String())
	}
	if mode == "signal" {
		leaves, err := os.ReadDir(scratchRoot)
		ts.Check(err)
		if len(leaves) != 1 {
			ts.Fatalf("signal unexpectedly removed scratch directory")
		}
		if _, err := os.Stat(filepath.Join(scratchRoot, leaves[0].Name(), "auth.json")); !os.IsNotExist(err) {
			ts.Fatalf("signal left registered credential")
		}
		if _, err := os.Stat(authPath); !os.IsNotExist(err) {
			ts.Fatalf("signal installed credential")
		}
	}
	_, _ = io.WriteString(ts.Stdout(), stdout.String())
	_, _ = io.WriteString(ts.Stderr(), stderr.String())
}
