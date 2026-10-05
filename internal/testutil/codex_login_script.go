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
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
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
