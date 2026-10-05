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
	"context"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"
)

func init() {
	registerScriptCmd("doctorfixture", doctorScriptFixture)
	registerScriptCmd("doctor-state", doctorScriptState)
}

func doctorScriptWrite(ts *testscript.TestScript, path string, document any) {
	data, err := json.Marshal(document)
	ts.Check(err)
	ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
	ts.Check(os.WriteFile(path, data, 0o600))
}

func doctorScriptFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 1 || len(args) > 2 {
		ts.Fatalf("usage: doctorfixture <empty|owned|session|rich|leaked|all|audit|config-match|config-mismatch> [parent]")
	}
	kind := args[0]
	parent := ts.MkAbs(".")
	if len(args) == 2 {
		parent = ts.MkAbs(args[1])
	}
	root, err := os.MkdirTemp(parent, "doctor-")
	ts.Check(err)
	config, home := filepath.Join(root, "config"), filepath.Join(root, "home")
	org := Org
	if kind == "all" {
		org = UnknownOrg
	}
	ns := filepath.Join(config, "claude", Acct, org)
	session := filepath.Join(config, "claude-sessions", Acct, org)
	live := filepath.Join(home, ".claude")
	audit := filepath.Join(config, "claude", "keychain-writes.jsonl")
	for _, dir := range []string{ns, live, filepath.Join(root, "items"), filepath.Join(config, "claude", ".locks")} {
		ts.Check(os.MkdirAll(dir, 0o700))
	}
	for key, value := range map[string]string{"AGENTCTL_CONFIG_DIR": config, "HOME": home, "NS": ns, "SESSION": session, "LIVE": live, "AUDIT": audit, "AGCTL_FAKE_SECURITY_ITEMS": filepath.Join(root, "items"), "AGCTL_FAKE_SECURITY_LOG": filepath.Join(root, "security.log"), "AGCTL_FAKE_SECURITY_DUMP": filepath.Join(root, "dump"), "AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT": "0", "AGENTCTL_KEYCHAIN_BACKEND": "", "LOCK": filepath.Join(ns, ".oauth_refresh.lock"), "HELD": filepath.Join(config, "claude", "held-locks"), "FLOCK": filepath.Join(config, "claude", ".locks", Acct+"."+org+".lock")} {
		ts.Setenv(key, value)
	}
	var accounts []any
	if kind != "empty" {
		spelling := ExportSpelling(ns)
		accounts = append(accounts, map[string]any{"account_uuid": Acct, "organization_uuid": org, "email": Email, "kind": map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339)})
	}
	doctorScriptWrite(ts, filepath.Join(config, "config.json"), map[string]any{"version": 1, "accounts": accounts, "forgotten_services": []string{}})
	blob := map[string]any{"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat01-doctor-secret", "refreshToken": "sk-ant-ort01-doctor-secret", "expiresAt": FreshAt(), "scopes": []string{"user:inference", "user:profile"}, "tokenAccount": map[string]any{"uuid": Acct, "organizationUuid": org, "emailAddress": Email}}}
	if kind != "empty" {
		doctorScriptWrite(ts, filepath.Join(ns, ".credentials.json"), blob)
	}
	ts.Check(os.WriteFile(filepath.Join(root, "dump"), []byte(dumpListing()), 0o600))
	switch kind {
	case "empty", "owned":
	case "session", "rich", "leaked":
		ts.Check(os.MkdirAll(session, 0o700))
		doctorScriptWrite(ts, filepath.Join(home, ".claude.json"), map[string]any{"mcpServers": map[string]any{}})
		doctorScriptWrite(ts, filepath.Join(session, ".claude.json"), map[string]any{"hasCompletedOnboarding": true})
		ts.Check(os.Symlink(filepath.Join(home, ".claude.json"), filepath.Join(session, "mcp.json")))
		if kind == "session" {
			return
		}
		doctorScriptWrite(ts, filepath.Join(live, "settings.json"), map[string]any{})
		ts.Check(os.Symlink(filepath.Join(live, "settings.json"), filepath.Join(session, "settings.json")))
		if kind == "leaked" {
			ts.Check(os.WriteFile(filepath.Join(session, "CLAUDE.md"), []byte("occupied"), 0o600))
			doctorScriptWrite(ts, filepath.Join(session, ".claude.json"), map[string]any{"theme": "dark", "oauthAccount": map[string]any{"uuid": "secret-leaked-value"}})
			return
		}
		for _, dir := range []string{"projects", "statsig"} {
			ts.Check(os.MkdirAll(filepath.Join(live, dir), 0o700))
		}
		for _, file := range []string{"history.jsonl", "todos.json"} {
			ts.Check(os.WriteFile(filepath.Join(live, file), nil, 0o600))
		}
		doctorScriptWrite(ts, filepath.Join(home, ".claude.json"), map[string]any{"mcpServers": map[string]any{"with-env": map[string]any{"env": map[string]any{"TOKEN": "secret-one"}}, "with-headers": map[string]any{"headers": map[string]any{"Authorization": "secret-two"}}, "plain": map[string]any{"env": map[string]any{}}}})
		old := time.Now().Add(-time.Minute)
		ts.Check(os.Chtimes(filepath.Join(session, ".claude.json"), old, old))
	case "audit":
		ts.Check(os.WriteFile(audit, nil, 0o600))
		ts.Check(os.Chmod(audit, 0o644))
	case "config-match", "config-mismatch":
		account := Acct
		if kind == "config-mismatch" {
			account = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		}
		actual := filepath.Join(home, "actual-config.json")
		doctorScriptWrite(ts, actual, map[string]any{"oauthAccount": map[string]any{"accountUuid": account, "organizationUuid": Org}})
		ts.Check(os.Symlink(actual, filepath.Join(home, ".claude.json")))
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		data, err := json.Marshal(map[string]any{"ts": stamp, "monotonic_ms": 1, "agctl_pid": 1, "event": "config_write", "outcome": "applied", "account": map[string]any{"account_uuid": Acct, "organization_uuid": Org}, "hold_ms": 4})
		ts.Check(err)
		ts.Check(os.WriteFile(audit, append(data, '\n'), 0o600))
	case "all":
		for _, file := range []string{".credentials.json.pending", ".pending.meta", ".credentials.json.tmp.0123abcd"} {
			doctorScriptWrite(ts, filepath.Join(ns, file), map[string]any{})
		}
		service := LiveService + "-deadbeef"
		for _, name := range []string{LiveService, service} {
			doctorScriptWrite(ts, filepath.Join(root, "items", KeychainAccount, ItemFileName(name)), blob)
		}
		ts.Check(os.WriteFile(filepath.Join(root, "dump"), []byte(dumpListing(LiveService, service)), 0o600))
		path := ts.Getenv("FLOCK")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		ts.Check(err)
		ts.Check(unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB))
		ts.Defer(func() { _ = file.Close() })
		doctorScriptWrite(ts, path, map[string]any{"pid": os.Getpid(), "acquired_at": time.Now().UTC().Format(time.RFC3339Nano)})
		ts.Setenv("HOLDER_PID", strconv.Itoa(os.Getpid()))
	default:
		ts.Fatalf("unknown doctor fixture %q", kind)
	}
}

func doctorScriptState(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 1 || neg && args[0] != "store-block" {
		ts.Fatalf("usage: doctor-state <age|record|mode|store-block|elapsed> [arguments]")
	}
	switch args[0] {
	case "age":
		if len(args) != 2 {
			ts.Fatalf("usage: doctor-state age <path>")
		}
		old := time.Now().Add(-2 * time.Minute)
		ts.Check(os.Chtimes(ts.MkAbs(args[1]), old, old))
	case "record":
		if len(args) != 3 {
			ts.Fatalf("usage: doctor-state record <dead|live|planted> <path>")
		}
		pid := os.Getpid()
		if args[1] != "live" {
			command := exec.CommandContext(ts.Value(scriptContextKey{}).(context.Context), "/usr/bin/true")
			ts.Check(command.Start())
			pid = command.Process.Pid
			ts.Check(command.Wait())
		}
		dir := ts.Getenv("HELD")
		if args[1] == "planted" {
			dir = filepath.Join(ts.Getenv("HOME"), "planted-held")
			ts.Check(os.Symlink(dir, ts.Getenv("HELD")))
		}
		path := filepath.Join(dir, strconv.Itoa(pid)+".json")
		doctorScriptWrite(ts, path, map[string]any{"agctl_pid": pid, "tree": "live", "store_dir": ts.Getenv("LIVE"), "paths": []string{args[2]}, "taken_at": time.Now().UTC().Format(time.RFC3339Nano)})
		ts.Setenv("RECORD", path)
	case "mode":
		if len(args) != 3 {
			ts.Fatalf("usage: doctor-state mode <path> <octal>")
		}
		want, err := strconv.ParseUint(args[2], 8, 32)
		ts.Check(err)
		info, err := os.Stat(ts.MkAbs(args[1]))
		ts.Check(err)
		if info.Mode().Perm() != os.FileMode(want) {
			ts.Fatalf("mode = %o, want %o", info.Mode().Perm(), want)
		}
	case "store-block":
		if len(args) != 2 {
			ts.Fatalf("usage: doctor-state store-block <report>")
		}
		report := ts.ReadFile(args[1])
		block, _, _ := strings.Cut(report, "\n\n")
		uuid := regexp.MustCompile(`[[:xdigit:]]{8}(-[[:xdigit:]]{4}){3}-[[:xdigit:]]{12}`)
		lines := strings.Split(block, "\n")
		exposesIdentity := false
		for _, line := range lines {
			value := line
			switch {
			case strings.HasPrefix(line, "  config dir"), strings.HasPrefix(line, "  namespace root"):
				continue
			case strings.HasPrefix(line, "  registry"), strings.HasPrefix(line, "  audit log"):
				if _, state, ok := strings.Cut(line, " ("); ok {
					value = state
				}
			case strings.HasPrefix(line, "  claude config"):
				if _, verdict, ok := strings.Cut(line, "); "); ok {
					value = verdict
				}
			}
			if strings.Contains(value, "@") || uuid.MatchString(value) {
				exposesIdentity = true
			}
		}
		if exposesIdentity != neg {
			ts.Fatalf("store block identity exposure = %t; want %t", exposesIdentity, neg)
		}
		for i, line := range lines {
			if strings.HasPrefix(line, "  audit log") && (i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "  claude config")) {
				ts.Fatalf("configuration row does not follow audit row: %s", block)
			}
		}
	case "elapsed":
		if len(args) != 2 {
			ts.Fatalf("usage: doctor-state elapsed <start|check>")
		}
		if args[1] == "start" {
			ts.Setenv("DOCTOR_STARTED", strconv.FormatInt(time.Now().UnixNano(), 10))
			return
		}
		started, err := strconv.ParseInt(ts.Getenv("DOCTOR_STARTED"), 10, 64)
		ts.Check(err)
		if elapsed := time.Since(time.Unix(0, started)); elapsed < 11*time.Second {
			ts.Fatalf("heartbeat interval was not observed: %s", elapsed)
		}
	default:
		ts.Fatalf("unknown doctor state action %q", args[0])
	}
}
