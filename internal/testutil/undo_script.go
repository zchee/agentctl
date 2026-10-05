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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io/fs"
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
)

func init() { registerScriptCmd("undo-setup", undoSetup) }

func undoSetup(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) > 1 || len(args) == 1 && args[0] != "own" && args[0] != "own-alias" {
		ts.Fatalf("usage: undo-setup [own|own-alias]")
	}
	if original := ts.Getenv("UNDO_SECURITY_ORIGINAL"); original != "" {
		ts.Setenv("AGENTCTL_SECURITY_BIN", original)
	}
	root, configDir := ts.Getenv("SWAP_ROOT"), ts.Getenv("AGENTCTL_CONFIG_DIR")
	if len(args) == 1 && args[0] == "own-alias" {
		canonicalRoot := root + "-canonical"
		ts.Check(os.Rename(root, canonicalRoot))
		ts.Check(os.Symlink(canonicalRoot, root))
	}
	owner, incoming, third := ts.Getenv("OWNER"), ts.Getenv("INCOMING"), "55555555-5555-4555-8555-555555555555"
	ownerDir := filepath.Join(configDir, "claude", owner, Org)
	incomingDir := filepath.Join(configDir, "claude", incoming, Org)
	thirdDir := filepath.Join(configDir, "claude", third, Org)
	items := ts.Getenv("AGCTL_FAKE_SECURITY_ITEMS")
	service := LiveService
	if ts.Getenv("CLAUDE_SECURESTORAGE_CONFIG_DIR") != "" {
		service = MigrationService(ownerDir)
	}
	item := filepath.Join(items, KeychainAccount, ItemFileName(service))
	write := func(path string, body []byte) {
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, body, 0o600))
	}
	document := func(value any) []byte { body, err := json.Marshal(value); ts.Check(err); return body }
	blob := func(name, account string, expiry int64) []byte {
		oauth := struct {
			Access  string         `json:"accessToken"`
			Refresh string         `json:"refreshToken"`
			Expiry  int64          `json:"expiresAt"`
			Scopes  []string       `json:"scopes"`
			Account jsontext.Value `json:"tokenAccount,omitzero"`
		}{Access: "SENTINEL-access-" + name, Refresh: "SENTINEL-refresh-" + name, Expiry: expiry, Scopes: []string{"user:inference", "user:profile"}}
		if account != "-" {
			oauth.Account = document(struct {
				UUID          string  `json:"uuid"`
				Email         *string `json:"emailAddress"`
				Org           string  `json:"organizationUuid"`
				OrgName       *string `json:"organizationName"`
				Workspace     *string `json:"workspaceId"`
				WorkspaceName *string `json:"workspaceName"`
			}{UUID: account, Org: Org})
		}
		return document(struct {
			OAuth any `json:"claudeAiOauth"`
		}{OAuth: oauth})
	}
	if len(args) == 0 {
		if _, err := os.Stat(item); err == nil {
			ts.Check(os.Remove(filepath.Join(ownerDir, ".credentials.json")))
		} else if !os.IsNotExist(err) {
			ts.Check(err)
		}
	}
	write(filepath.Join(thirdDir, ".credentials.json"), blob("third", third, FreshAt()))
	registryPath := filepath.Join(configDir, "config.json")
	var registry map[string]any
	ts.Check(json.Unmarshal([]byte(ts.ReadFile(registryPath)), &registry))
	spelling := ExportSpelling(thirdDir)
	registry["accounts"] = append(registry["accounts"].([]any), map[string]any{"account_uuid": third, "organization_uuid": Org, "kind": map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339)})
	write(registryPath, document(registry))
	control := filepath.Join(root, "undo-profile-control")
	write(control, nil)
	canonical, err := filepath.EvalSymlinks(ownerDir)
	ts.Check(err)
	canonicalItem := filepath.Join(items, KeychainAccount, ItemFileName(MigrationService(canonical)))
	for key, value := range map[string]string{
		"UNDO_OWNER_DIR": ownerDir, "UNDO_INCOMING_DIR": incomingDir, "UNDO_THIRD_DIR": thirdDir, "THIRD": third,
		"UNDO_ITEM": item, "UNDO_CONFIG": filepath.Join(ts.Getenv("HOME"), ".claude.json"), "UNDO_REGISTRY": registryPath,
		"UNDO_PROFILE_CONTROL": control, "UNDO_CANONICAL_ITEM": canonicalItem,
		"UNDO_OWNER_ITEM": filepath.Join(items, KeychainAccount, ItemFileName(MigrationService(ownerDir))),
		"UNDO_RESUME":     filepath.Join(root, "undo-resume"),
	} {
		ts.Setenv(key, value)
	}
	var remoteTree map[string]string
	ts.SetCmd("undo-remote", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 || args[0] != "seed" && args[0] != "check" {
			ts.Fatalf("usage: undo-remote <seed|check>")
		}
		dir := filepath.Join(ts.Getenv("HOME"), ".claude", "sessions")
		if args[0] == "seed" {
			write(filepath.Join(dir, "4242.json"), document(map[string]any{"pid": os.Getpid(), "name": "undo-remote-session", "cwd": "private-working-directory", "tmux": "private-tmux", "sessionId": "private-local-session", "bridgeSessionId": "private-bridge-session"}))
			write(filepath.Join(dir, "session.key"), []byte("registry sibling"))
			write(filepath.Join(dir, "nested", "unchanged"), []byte("nested bytes"))
			ts.Check(os.Symlink("4242.json", filepath.Join(dir, "alias.json")))
		}
		tree := make(map[string]string)
		ts.Check(filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			value := info.Mode().String()
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				value += target
			} else if !entry.IsDir() {
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value += string(body)
			}
			tree[path] = value
			return nil
		}))
		if args[0] == "seed" {
			remoteTree = tree
		} else if !gocmp.Equal(tree, remoteTree) {
			ts.Fatalf("session registry changed during a swap")
		}
	})
	var posts, profiles atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			say(w, `{"access_token":"SENTINEL-access-outgoing-rotated","refresh_token":"SENTINEL-refresh-outgoing-rotated","token_type":"Bearer","expires_in":28800,"scope":"user:inference user:profile"}`)
			return
		}
		profiles.Add(1)
		account := owner
		header := r.Header.Get("Authorization")
		if strings.Contains(header, "incoming") {
			account = incoming
		} else if strings.Contains(header, "third") || strings.Contains(header, "stranger") {
			account = third
		}
		mode, readErr := os.ReadFile(control)
		if readErr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.TrimSpace(string(mode)) == "down-owner" && account == owner {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		say(w, `{"account":{"uuid":%q,"email":"user@example.invalid"},"organization":{"uuid":%q,"name":"Example"}}`, account, Org)
	}))
	ts.Defer(server.Close)
	ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+"/token")
	ts.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", server.URL+"/profile")
	ts.SetCmd("undo-blob", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 3 || len(args) > 4 {
			ts.Fatalf("usage: undo-blob <path> <name> <account|-> [expired|newer]")
		}
		expiry := FreshAt()
		if len(args) == 4 {
			switch args[3] {
			case "expired":
				expiry = ExpiredAt()
			case "newer":
				expiry += 3_600_000
			default:
				ts.Fatalf("unknown expiry %q", args[3])
			}
		}
		write(args[0], blob(args[1], args[2], expiry))
	})
	ts.SetCmd("undo-credential", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: undo-credential <path> <name>")
		}
		var value struct {
			OAuth struct {
				Access  string `json:"accessToken"`
				Refresh string `json:"refreshToken"`
			} `json:"claudeAiOauth"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[0])), &value))
		if value.OAuth.Access != "SENTINEL-access-"+args[1] || value.OAuth.Refresh != "SENTINEL-refresh-"+args[1] {
			ts.Fatalf("credential at %s does not hold the expected pair", args[0])
		}
	})
	ts.SetCmd("undo-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: undo-check <writes> <posts> <profiles>")
		}
		want := make([]int, 3)
		for i, arg := range args {
			n, err := strconv.Atoi(arg)
			ts.Check(err)
			want[i] = n
		}
		writes := 0
		for line := range strings.SplitSeq(ts.ReadFile(filepath.Join(root, "security.log")), "\n") {
			if line == "-i" {
				writes++
			}
			if strings.Contains(line, "SENTINEL") || strings.HasPrefix(line, "delete-generic-password") {
				ts.Fatalf("unsafe security argv")
			}
		}
		if writes != want[0] || int(posts.Load()) != want[1] || int(profiles.Load()) != want[2] {
			ts.Fatalf("writes/posts/profiles=%d/%d/%d; want=%v", writes, posts.Load(), profiles.Load(), want)
		}
		if data, err := os.ReadFile(ts.Getenv("SWAP_AUDIT")); err == nil && (bytes.Contains(data, []byte("SENTINEL")) || bytes.Contains(data, []byte("@"))) {
			ts.Fatalf("audit contains credential or email material")
		}
		for _, dir := range []string{ownerDir, incomingDir, thirdDir, filepath.Join(ts.Getenv("HOME"), ".claude")} {
			entries, err := os.ReadDir(dir)
			ts.Check(err)
			for _, entry := range entries {
				if strings.Contains(entry.Name(), ".tmp.") || entry.Name() == ".oauth_refresh.lock" || entry.Name() == ".credentials.json.lock" {
					ts.Fatalf("unreleased temporary or peer lock at %s", filepath.Join(dir, entry.Name()))
				}
			}
		}
	})
	ts.SetCmd("undo-audit", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: undo-audit <unknown-forward|unknown-undo|tail|missing-copy>")
		}
		path := ts.Getenv("SWAP_AUDIT")
		data := ts.ReadFile(path)
		lines := strings.Split(strings.TrimSpace(data), "\n")
		var entries []map[string]any
		for _, line := range lines {
			if line == "" {
				continue
			}
			var entry map[string]any
			ts.Check(json.Unmarshal([]byte(line), &entry))
			entries = append(entries, entry)
		}
		switch args[0] {
		case "unknown-forward":
			entries[0]["outcome"] = "unknown"
		case "unknown-undo":
			entry := map[string]any{}
			ts.Check(json.Unmarshal(document(entries[0]), &entry))
			entry["direction"], entry["outcome"] = "undo", "unknown"
			entry["from_digest8"], entry["to_digest8"] = entry["to_digest8"], entry["from_digest8"]
			entries = append(entries, entry)
		case "tail":
			for range 300 {
				entry := map[string]any{}
				ts.Check(json.Unmarshal(document(entries[0]), &entry))
				entry["target"] = "namespace:0123abcd"
				entry["from_digest8"], entry["to_digest8"] = "deadbeef", "cafebabe"
				delete(entry, "incoming_identity")
				entries = append(entries, entry)
			}
		case "missing-copy":
			entries[0]["from_digest8"] = "deadbeef"
		default:
			ts.Fatalf("unknown audit mutation %q", args[0])
		}
		var output []byte
		for _, entry := range entries {
			output = append(output, document(entry)...)
			output = append(output, '\n')
		}
		write(path, output)
	})
	ts.SetCmd("undo-audit-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 1 {
			ts.Fatalf("usage: undo-audit-check <direction:account|direction:->...")
		}
		var writes []map[string]any
		for line := range strings.SplitSeq(ts.ReadFile(ts.Getenv("SWAP_AUDIT")), "\n") {
			if line == "" {
				continue
			}
			var entry map[string]any
			ts.Check(json.Unmarshal([]byte(line), &entry))
			if entry["event"] == "write" {
				writes = append(writes, entry)
			}
		}
		if len(writes) != len(args) {
			ts.Fatalf("audit writes=%d; want=%d", len(writes), len(args))
		}
		for i, arg := range args {
			direction, account, ok := strings.Cut(arg, ":")
			if !ok || writes[i]["direction"] != direction || writes[i]["outcome"] != "applied" {
				ts.Fatalf("audit write %d has wrong direction or outcome", i)
			}
			if i > 0 && direction == "undo" {
				previous := writes[i-1]
				if writes[i]["from_digest8"] != previous["to_digest8"] || previous["from_digest8"] != nil && writes[i]["to_digest8"] != previous["from_digest8"] {
					ts.Fatalf("audit write %d does not reverse the preceding digest pair", i)
				}
			}
			if account != "-" {
				identity, ok := writes[i]["incoming_identity"].(map[string]any)
				if !ok || len(identity) != 2 || identity["account_uuid"] != account || identity["organization_uuid"] != Org {
					ts.Fatalf("audit write %d has wrong installed identity", i)
				}
			}
		}
	})
	ts.SetCmd("undo-move-record", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: undo-move-record")
		}
		var current map[string]any
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(registryPath)), &current))
		accounts := current["accounts"].([]any)
		for _, value := range accounts {
			account := value.(map[string]any)
			if account["account_uuid"] == owner {
				account["kind"].(map[string]any)["export_spelling"] = filepath.Join(root, "previous-namespace")
			}
		}
		write(registryPath, document(current))
	})
	linkedConfig := false
	ts.SetCmd("undo-config", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 1 || len(args) > 2 || len(args) == 2 && args[1] != "link" {
			ts.Fatalf("usage: undo-config <account> [link]")
		}
		path := ts.Getenv("UNDO_CONFIG")
		if len(args) == 2 {
			target := filepath.Join(root, "config-target.json")
			ts.Check(os.Rename(path, target))
			ts.Check(os.Symlink(target, path))
			linkedConfig = true
			path = target
		}
		body, err := json.Marshal(map[string]any{"theme": "dark", "oauthAccount": map[string]string{"accountUuid": args[0], "organizationUuid": Org}}, json.Deterministic(true), jsontext.WithIndent("  "))
		ts.Check(err)
		write(path, body)
	})
	ts.SetCmd("undo-config-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 1 {
			ts.Fatalf("usage: undo-config-check <account> [expected-backup-file...]")
		}
		path := ts.Getenv("UNDO_CONFIG")
		var value struct {
			Theme   string `json:"theme"`
			Account struct {
				Account string `json:"accountUuid"`
				Org     string `json:"organizationUuid"`
				Fetched int64  `json:"profileFetchedAt"`
			} `json:"oauthAccount"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(path)), &value))
		if value.Theme != "dark" || value.Account.Account != args[0] || value.Account.Org != Org || value.Account.Fetched <= 0 {
			ts.Fatalf("config does not preserve the theme and fetched account identity")
		}
		if linkedConfig {
			info, err := os.Lstat(path)
			ts.Check(err)
			if info.Mode()&os.ModeSymlink == 0 {
				ts.Fatalf("config symlink was replaced")
			}
		}
		if len(args) > 1 {
			dir := filepath.Join(ts.Getenv("HOME"), ".claude", "backups")
			entries, err := os.ReadDir(dir)
			ts.Check(err)
			if len(entries) != len(args)-1 {
				ts.Fatalf("config backup count=%d; want=%d", len(entries), len(args)-1)
			}
			for i, entry := range entries {
				if ts.ReadFile(filepath.Join(dir, entry.Name())) != ts.ReadFile(args[i+1]) {
					ts.Fatalf("config backup %d differs from its input bytes", i)
				}
			}
		}
	})
	ts.SetCmd("undo-security", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 || args[0] != "peer" && args[0] != "hang" {
			ts.Fatalf("usage: undo-security <peer|hang>")
		}
		wrapper := filepath.Join(root, "undo-security")
		body := "#!/bin/sh\n"
		if args[0] == "peer" {
			body += "\"$AGCTL_FAKE_UNDO_ORIGINAL\" \"$@\"\nstatus=$?\nif [ \"$1\" = 'find-generic-password' ]; then\n  touch \"$AGCTL_FAKE_UNDO_RESUME.reached\"\n  while [ ! -e \"$AGCTL_FAKE_UNDO_RESUME\" ]; do sleep 0.01; done\nfi\nexit $status\n"
		} else {
			body += "if [ \"$1\" = '-i' ]; then printf '%s\\n' '-i' >> \"$AGCTL_FAKE_SECURITY_LOG\"; sleep 5; fi\nexec \"$AGCTL_FAKE_UNDO_ORIGINAL\" \"$@\"\n"
		}
		write(wrapper, []byte(body))
		ts.Check(os.Chmod(wrapper, 0o700))
		ts.Setenv("UNDO_SECURITY_ORIGINAL", ts.Getenv("AGENTCTL_SECURITY_BIN"))
		ts.Setenv("AGCTL_FAKE_UNDO_ORIGINAL", ts.Getenv("AGENTCTL_SECURITY_BIN"))
		ts.Setenv("AGCTL_FAKE_UNDO_RESUME", ts.Getenv("UNDO_RESUME"))
		ts.Setenv("AGENTCTL_SECURITY_BIN", wrapper)
	})
	ts.SetCmd("undo-age", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: undo-age <path>")
		}
		old := time.Now().Add(-10 * time.Minute)
		ts.Check(os.Chtimes(args[0], old, old))
	})
}
