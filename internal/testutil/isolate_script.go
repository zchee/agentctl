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
	json "encoding/json/v2"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
)

func init() { registerScriptCmd("isolatefixture", isolateFixture) }

func isolateFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: isolatefixture <name> <empty|full|linked|busy|stale|badhash|occupant|symlink>")
	}
	root := ts.MkAbs(filepath.Join("isolate-cases", args[0]))
	home := filepath.Join(root, "home")
	store := filepath.Join(root, "config")
	ns := filepath.Join(store, "claude", "account", "org")
	session := filepath.Join(store, "claude-sessions", "account", "org")
	live := filepath.Join(home, ".claude.json")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{home, store, ns, bin} {
		ts.Check(os.MkdirAll(dir, 0o700))
	}
	for key, value := range map[string]string{"HOME": home, "AGENTCTL_CONFIG_DIR": store, "SESSION": session, "NAMESPACE": ns, "LIVE": live, "ISOLATE_ROOT": root, "ARGV_LOG": filepath.Join(root, "argv.log"), "CLAUDE_CONFIG_DIR": "", "CLAUDE_SECURESTORAGE_CONFIG_DIR": "", "CLAUDE_CODE_OAUTH_TOKEN": "", "AGENTCTL_KEYCHAIN_BACKEND": "none"} {
		ts.Setenv(key, value)
	}
	ts.Setenv("PATH", bin+string(os.PathListSeparator)+ts.Getenv("PATH"))
	write := func(path, text string) {
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, []byte(text), 0o600))
	}
	hash := Sha8(ns)
	if args[1] == "badhash" {
		hash = "deadbeef"
	}
	doc := map[string]any{"version": 1, "accounts": []any{map[string]any{"account_uuid": "account", "organization_uuid": "org", "kind": map[string]any{"kind": "owned", "export_spelling": ns, "export_sha8": hash}, "forgotten": false, "created_at": "2026-09-08T00:00:00Z"}}, "forgotten_services": []string{}}
	data, err := json.Marshal(doc)
	ts.Check(err)
	write(filepath.Join(store, "config.json"), string(data))
	write(live, `{"hasCompletedOnboarding":true,"theme":"dark","editorMode":"vim","oauthAccount":{"accessToken":"sk-ant-private"},"mcpServers":{"private":{}},"userID":"private"}`)
	write(filepath.Join(ns, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"sk-ant-at-rest"}}`)
	write(filepath.Join(store, "claude", "keychain-writes.jsonl"), "{\"event\":\"write\",\"outcome\":\"applied\"}\n")
	capture := "#!/bin/sh\n: > \"$ARGV_LOG\"\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$ARGV_LOG\"; done\n"
	for _, name := range []string{"claude", "capture", "notclaude"} {
		path := filepath.Join(bin, name)
		write(path, capture)
		ts.Check(os.Chmod(path, 0o700))
	}
	var lockMtime time.Time
	switch args[1] {
	case "empty", "badhash":
	case "full", "occupant":
		for _, name := range []string{"settings.json", "CLAUDE.md"} {
			write(filepath.Join(home, ".claude", name), "preferences")
		}
		for _, name := range []string{"skills", "projects", "shell-snapshots", "file-history", "sessions", "session-env", "backups"} {
			ts.Check(os.MkdirAll(filepath.Join(home, ".claude", name), 0o700))
		}
		write(filepath.Join(home, ".claude", "history.jsonl"), "private history")
		if args[1] == "occupant" {
			write(filepath.Join(session, "settings.json"), "foreign occupant")
		}
	case "symlink":
		target := filepath.Join(root, "outside")
		ts.Check(os.MkdirAll(target, 0o700))
		ts.Check(os.MkdirAll(filepath.Dir(session), 0o700))
		ts.Check(os.Symlink(target, session))
	case "linked", "busy", "stale":
		real := filepath.Join(home, ".claude-real", ".claude.json")
		ts.Check(os.MkdirAll(filepath.Dir(real), 0o700))
		ts.Check(os.Rename(live, real))
		ts.Check(os.Symlink(real, live))
		if args[1] != "linked" {
			ts.Check(os.Mkdir(live+".lock", 0o700))
			if args[1] == "stale" {
				old := time.Now().Add(-time.Minute)
				ts.Check(os.Chtimes(live+".lock", old, old))
			}
			info, err := os.Stat(live + ".lock")
			ts.Check(err)
			lockMtime = info.ModTime()
		}
	default:
		ts.Fatalf("unknown isolation fixture %q", args[1])
	}
	before := isolateTree(ts, root)
	ts.SetCmd("isolate-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) == 0 {
			ts.Fatalf("usage: isolate-check <layout|unchanged|env|lock|seed> [arguments]")
		}
		switch args[0] {
		case "env":
			if len(args) != 3 {
				ts.Fatalf("usage: isolate-check env <actual> <oracle>")
			}
			got := strings.NewReplacer(ns, "@NAMESPACE@", session, "@SESSION@").Replace(ts.ReadFile(args[1]))
			if diff := gocmp.Diff(ts.ReadFile(args[2]), got); diff != "" {
				ts.Fatalf("environment (-want +got):\n%s", diff)
			}
		case "layout":
			if len(args) != 2 {
				ts.Fatalf("usage: isolate-check layout <empty|full|fresh|no-mcp>")
			}
			names := []string{".claude.json"}
			if args[1] != "no-mcp" {
				names = append(names, "mcp.json")
			}
			if args[1] == "full" || args[1] == "fresh" {
				names = append(names, "settings.json", "CLAUDE.md", "skills")
			}
			if args[1] == "full" {
				names = append(names, "projects", "shell-snapshots", "file-history", "sessions", "session-env")
			}
			entries, err := os.ReadDir(session)
			ts.Check(err)
			var got []string
			for _, entry := range entries {
				got = append(got, entry.Name())
			}
			slices.Sort(names)
			slices.Sort(got)
			if diff := gocmp.Diff(names, got); diff != "" {
				ts.Fatalf("session entries: %s", diff)
			}
			for _, name := range names {
				path := filepath.Join(session, name)
				info, err := os.Lstat(path)
				ts.Check(err)
				if name == ".claude.json" {
					if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
						ts.Fatalf("seed mode=%v", info.Mode())
					}
					continue
				}
				if info.Mode()&os.ModeSymlink == 0 {
					ts.Fatalf("%s is not a symlink", path)
				}
				want := filepath.Join(home, ".claude", name)
				if name == "mcp.json" {
					want = live
				}
				want, err = filepath.EvalSymlinks(want)
				ts.Check(err)
				target, err := os.Readlink(path)
				ts.Check(err)
				if target != want {
					ts.Fatalf("%s points to %s, want %s", path, target, want)
				}
			}
			info, err := os.Stat(session)
			ts.Check(err)
			if info.Mode().Perm() != 0o700 {
				ts.Fatalf("session mode=%v", info.Mode())
			}
		case "unchanged":
			after := isolateTree(ts, root)
			for path, data := range before {
				if !bytes.Equal(data, after[path]) {
					ts.Fatalf("existing file changed: %s", path)
				}
			}
			for path, data := range after {
				if !bytes.Equal(data, before[path]) && (bytes.Contains(data, []byte("sk-ant-")) || bytes.Contains(data, []byte("mcpServers")) || bytes.Contains(data, []byte("oauthAccount"))) {
					ts.Fatalf("new session data contains excluded values: %s", path)
				}
			}
		case "lock":
			if lockMtime.IsZero() {
				if _, err := os.Lstat(live + ".lock"); !os.IsNotExist(err) {
					ts.Fatalf("config lock remains: %v", err)
				}
			} else {
				info, err := os.Stat(live + ".lock")
				ts.Check(err)
				if !info.IsDir() || !info.ModTime().Equal(lockMtime) {
					ts.Fatalf("peer config lock changed")
				}
			}
		case "seed":
			data, err := os.ReadFile(filepath.Join(session, ".claude.json"))
			ts.Check(err)
			var doc map[string]any
			ts.Check(json.Unmarshal(data, &doc))
			want := map[string]any{"hasCompletedOnboarding": true, "theme": "dark", "editorMode": "vim"}
			if diff := gocmp.Diff(want, doc); diff != "" {
				ts.Fatalf("seed: %s", diff)
			}
		default:
			ts.Fatalf("unknown isolation check %q", args[0])
		}
	})
}

func isolateTree(ts *testscript.TestScript, root string) map[string][]byte {
	files := map[string][]byte{}
	ts.Check(filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = data
		}
		return nil
	}))
	return files
}
