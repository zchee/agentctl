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
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
)

func init() { registerScriptCmd("seedfixture", seedFixture) }

// seedLiveAccount is the account the live keychain item belongs to: never
// the namespace's own, so an account object that travels is told apart.
const seedLiveAccount = "88888888-8888-4888-8888-888888888888"

// seedOtherAccount is the account a taken-over namespace item names.
const seedOtherAccount = "99999999-9999-4999-8999-999999999999"

// seedFixtureOptions are the shapes one seed case can start from.
var seedFixtureOptions = map[string]string{
	"present":     "the namespace item exists and holds MCP state of its own",
	"taken-over":  "the namespace item exists and names another account",
	"padded":      "the live item's members put the seeded document past the update ceiling",
	"no-live":     "there is no live item",
	"bare-live":   "the live item holds its account object only",
	"no-file":     "the namespace has no owned credential file",
	"file-state":  "the owned file holds MCP state of its own",
	"adopted":     "the namespace holds a swap's parked copy",
	"pending":     "the namespace holds a parked pending write",
	"unknown-org": "the namespace was created without an organization",
}

// seedFixture lays out one isolated-session seed case: an owned namespace
// whose keychain item is absent unless an option says otherwise, the live
// item with the full sibling set, the fake security(1), a fake claude that
// records its argv, and a profile endpoint for the live swap. It then
// registers the seed-* commands that check what a run did.
func seedFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 1 {
		ts.Fatalf("usage: seedfixture <name> [option...]")
	}
	name := args[0]
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		ts.Fatalf("fixture name must be one path segment")
	}
	opts := map[string]bool{}
	for _, option := range args[1:] {
		if _, ok := seedFixtureOptions[option]; !ok {
			ts.Fatalf("unknown seed option %q", option)
		}
		opts[option] = true
	}
	root := ts.MkAbs(filepath.Join("seed-cases", name))
	configDir, home, items, bin := filepath.Join(root, "config"), filepath.Join(root, "home"), filepath.Join(root, "items"), filepath.Join(root, "bin")
	org := Org
	if opts["unknown-org"] {
		org = UnknownOrg
	}
	nsDir := filepath.Join(configDir, "claude", Acct, org)
	locksDir := filepath.Join(configDir, "claude", ".locks")
	itemDir := filepath.Join(items, KeychainAccount)
	for _, dir := range []string{nsDir, locksDir, itemDir, bin, filepath.Join(home, ".claude")} {
		ts.Check(os.MkdirAll(dir, 0o700))
	}
	write := func(path string, body []byte) {
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, body, 0o600))
	}
	compact := func(value any) jsontext.Value {
		body, err := compactJSON(value)
		ts.Check(err)
		return body
	}
	oauth := func(account, access string) jsontext.Value {
		return compact(map[string]any{"accessToken": "SENTINEL-" + access, "refreshToken": "SENTINEL-refresh-" + access, "expiresAt": FreshAt(), "scopes": []string{"user:inference", "user:profile"}, "tokenAccount": map[string]any{"uuid": account, "organizationUuid": Org, "emailAddress": "user@example.invalid"}})
	}
	// Members an owned file carries only when Claude Code's plaintext
	// fallback, or a hand edit, wrote them.
	fileMembers := []itemMember{
		{Name: "mcpOAuth", Value: compact(map[string]any{"file-server|00ff00ff": map[string]any{"accessToken": "FILE-SIBLING-mcp"}})},
		{Name: "FILE-SIBLING", Value: compact(map[string]any{"marker": "FILE-SIBLING-marker"})},
	}

	ownedOAuth := oauth(Acct, "owned")
	ownedFile := keychainDocument(ownedOAuth, nil)
	if opts["file-state"] {
		ownedFile = keychainDocument(ownedOAuth, fileMembers)
	}
	file := filepath.Join(nsDir, ".credentials.json")
	if !opts["no-file"] {
		write(file, ownedFile)
	}
	adopted := filepath.Join(nsDir, ".credentials.adopted.json")
	adoptedFile := keychainDocument(oauth(Acct, "adopted"), nil)
	if opts["adopted"] {
		write(adopted, adoptedFile)
	}
	pending := filepath.Join(nsDir, ".credentials.json.pending")
	pendingFile := keychainDocument(oauth(Acct, "pending"), nil)
	if opts["pending"] {
		write(pending, pendingFile)
	}

	var liveSiblings []itemMember
	var err error
	switch {
	case opts["padded"]:
		liveSiblings, err = paddedSiblings(0, KeychainBlobCeiling)
	case !opts["bare-live"]:
		liveSiblings, err = siblingSet("seeded")
	}
	ts.Check(err)
	livePath := filepath.Join(itemDir, ItemFileName(LiveService))
	if !opts["no-live"] {
		write(livePath, keychainDocument(oauth(seedLiveAccount, "live"), liveSiblings))
	}

	spelling := ExportSpelling(nsDir)
	service := LiveService + "-" + Sha8(spelling)
	itemPath := filepath.Join(itemDir, ItemFileName(service))
	listed := []string{LiveService}
	// The namespace's own MCP state: one login the live item also holds,
	// with another value, one of its own, and no plugin secret.
	nsSiblings := []itemMember{{Name: "mcpOAuth", Value: compact(map[string]any{
		"plugin:beta|4e5f6a7b": map[string]any{"serverName": "beta", "accessToken": "ns-own-beta"},
		"ns-own|0f0f0f0f":      map[string]any{"serverName": "own", "accessToken": "ns-own-login"},
	})}}
	if opts["present"] || opts["taken-over"] {
		owner := Acct
		if opts["taken-over"] {
			owner = seedOtherAccount
		}
		write(itemPath, keychainDocument(oauth(owner, "namespace-item"), nsSiblings))
		listed = append(listed, service)
	}
	itemBefore, _ := os.ReadFile(itemPath)
	liveBefore, _ := os.ReadFile(livePath)
	write(filepath.Join(items, ".allowed-services"), []byte(service+"\n"))
	dumpPath := filepath.Join(root, "dump")
	write(dumpPath, []byte(dumpListing(listed...)))
	logPath := filepath.Join(root, "security.log")
	write(logPath, nil)

	record := map[string]any{"account_uuid": Acct, "organization_uuid": org, "email": "user@example.invalid", "label": "account", "kind": map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)}, "forgotten": false, "created_at": "2026-09-08T00:00:00Z"}
	registry, err := json.Marshal(map[string]any{"version": 1, "accounts": []any{record}, "forgotten_services": []string{}})
	ts.Check(err)
	write(filepath.Join(configDir, "config.json"), registry)
	write(filepath.Join(home, ".claude.json"), []byte(`{"theme":"dark","mcpServers":{"alpha":{"type":"http","url":"https://alpha.example.invalid/mcp"}}}`))
	argvLog := filepath.Join(root, "claude-argv.log")
	write(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n: > \"$SEED_ARGV\"\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$SEED_ARGV\"; done\nexit \"${SEED_CHILD_CODE:-0}\"\n"))
	ts.Check(os.Chmod(filepath.Join(bin, "claude"), 0o700))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		say(w, `{"account":{"uuid":%q,"email":"user@example.invalid"},"organization":{"uuid":%q}}`, seedLiveAccount, Org)
	}))
	ts.Defer(server.Close)

	resume := filepath.Join(root, "resume")
	for key, value := range map[string]string{
		"AGENTCTL_CONFIG_DIR": configDir, "HOME": home, "USER": KeychainAccount, "LOGNAME": KeychainAccount,
		"AGENTCTL_KEYCHAIN_BACKEND": "", "AGCTL_FAKE_SECURITY_ITEMS": items, "AGCTL_FAKE_SECURITY_DUMP": dumpPath, "AGCTL_FAKE_SECURITY_LOG": logPath,
		"AGCTL_FAKE_SECURITY_WRITE_EXIT": "", "AGCTL_FAKE_SECURITY_FIND_EXIT": "", "AGCTL_FAKE_SECURITY_SLEEP": "", "AGCTL_FAKE_SECURITY_STDERR": "",
		"AGCTL_FAKE_WRITE_DELAY_MS": "", "AGCTL_FAKE_WRITE_DELAY_AFTER_MS": "", "AGCTL_FAKE_SECURITY_HOLD_PIPES": "", "AGCTL_FAKE_SECURITY_PIDFILE": "",
		"AGCTL_FAKE_SECURITY_FIND_EXIT_IF": filepath.Join(root, "find-fails"), "AGCTL_FAKE_SECURITY_FIND_EXIT_IF_CODE": "", "AGCTL_FAKE_SECURITY_DUMP_DELAY_MS": "", "AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT": "",
		"CLAUDE_CODE_OAUTH_TOKEN": "", "CLAUDE_CONFIG_DIR": "", "CLAUDE_SECURESTORAGE_CONFIG_DIR": "", "AGENTCTL_FAULT": "", "AGENTCTL_FAULT_RESUME": resume,
		"AGENTCTL_CLAUDE_PROFILE_URL": server.URL + "/profile",
		"SEED_ROOT":                   root, "SEED_NS": nsDir, "SEED_FILE": file, "SEED_ITEM": itemPath, "SEED_ARGV": argvLog, "SEED_CHILD_CODE": "0",
		"SEED_GUARD": filepath.Join(locksDir, Acct+"."+org+".lock"), "SEED_SESSION": filepath.Join(configDir, "claude-sessions", Acct, org),
	} {
		ts.Setenv(key, value)
	}
	ts.Setenv("PATH", bin+string(os.PathListSeparator)+ts.Getenv("PATH"))

	// expectedFile is the owned file's expected bytes: the fixture's, or the
	// ones a seed-action wrote.
	expectedFile := slices.Clone(ownedFile)
	var terminated *exec.Cmd
	ts.SetCmd("seed-action", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: seed-action <wait-pause|resume|create-item|change-account|add-file-members|change-sibling|fallback|storage-lock|find-fails|terminate|signal-exit>")
		}
		switch args[0] {
		case "wait-pause":
			awaitPause(ts, resume)
		case "resume":
			write(resume, []byte("go"))
		case "create-item":
			// Written directly, as a writer that takes no lock would.
			// Built once: the account object's expiry comes from the clock.
			created := keychainDocument(oauth(Acct, "created-meanwhile"), nsSiblings)
			write(itemPath, created)
			itemBefore = slices.Clone(created)
		case "change-account":
			expectedFile = keychainDocument(oauth(Acct, "relogged"), nil)
			write(file, expectedFile)
		case "add-file-members":
			expectedFile = keychainDocument(ownedOAuth, fileMembers)
			write(file, expectedFile)
		case "change-sibling":
			rewriteSiblings(ts, itemPath, "changed")
		case "fallback":
			// Claude Code's plaintext fallback: the whole document, the same
			// account object and its members, to the file, then the item it
			// held is deleted.
			expectedFile = keychainDocument(ownedOAuth, fileMembers)
			write(file, expectedFile)
			ts.Check(os.Remove(itemPath))
		case "storage-lock":
			ts.Check(os.Mkdir(filepath.Join(nsDir, ".storage-write.lock"), 0o700))
		case "find-fails":
			write(filepath.Join(root, "find-fails"), nil)
		case "terminate":
			background := ts.BackgroundCmds()
			if len(background) != 1 {
				ts.Fatalf("observed %d background commands; want 1", len(background))
			}
			lock := filepath.Join(nsDir, ".oauth_refresh.lock")
			if _, err := os.Lstat(lock); err != nil {
				ts.Fatalf("the paused removal does not hold its lock: %v", err)
			}
			terminated = background[0]
			ts.Check(terminated.Process.Signal(syscall.SIGTERM))
			// Cleanup releases this hold only after the controller cancels the
			// run, so resuming cannot race ahead of cancellation delivery.
			if !WaitUntil(10*time.Second, func() bool { _, err := os.Lstat(lock); return errors.Is(err, os.ErrNotExist) }) {
				ts.Fatalf("signal cleanup did not release the paused removal's lock")
			}
		case "signal-exit":
			if terminated == nil || terminated.ProcessState == nil || terminated.ProcessState.ExitCode() != 143 {
				ts.Fatalf("the interrupted use did not exit with status 143 after SIGTERM")
			}
		default:
			ts.Fatalf("unknown seed action %q", args[0])
		}
	})
	ts.SetCmd("seed-list", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: seed-list")
		}
		// A real keychain lists an item the moment it is created; the fake's
		// listing is a file, so it is rewritten to match.
		names := []string{}
		for _, path := range []string{livePath, itemPath} {
			if _, err := os.Stat(path); err == nil {
				names = append(names, map[string]string{livePath: LiveService, itemPath: service}[path])
			}
		}
		write(dumpPath, []byte(dumpListing(names...)))
	})
	ts.SetCmd("seed-report", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 2 || args[1] != "null" && len(args) < 6 {
			ts.Fatalf("usage: seed-report <stdout> null | <stdout> <outcome> <path|-> <readback|-> <transport|-> <owned_file|-> [note-substring...]")
		}
		assertSeedReport(ts, ts.ReadFile(args[0]), service, args[1:])
	})
	ts.SetCmd("seed-item", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: seed-item <created|unchanged|absent>")
		}
		data, err := os.ReadFile(itemPath)
		switch args[0] {
		case "absent":
			if !errors.Is(err, fs.ErrNotExist) {
				ts.Fatalf("the namespace item exists: %v", err)
			}
		case "unchanged":
			if err != nil || !bytes.Equal(data, itemBefore) {
				ts.Fatalf("the namespace item changed (%d bytes, was %d): %v", len(data), len(itemBefore), err)
			}
		case "created":
			ts.Check(err)
			assertSeededItem(ts, data, ownedOAuth)
		default:
			ts.Fatalf("unknown item state %q", args[0])
		}
	})
	ts.SetCmd("seed-file", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: seed-file <gone|same|changed>")
		}
		data, err := os.ReadFile(file)
		switch args[0] {
		case "gone":
			if !errors.Is(err, fs.ErrNotExist) {
				ts.Fatalf("the owned credential file is still there: %v", err)
			}
			entries, err := os.ReadDir(nsDir)
			ts.Check(err)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".credentials") && strings.HasSuffix(entry.Name(), ".json") && (!opts["adopted"] || entry.Name() != ".credentials.adopted.json") {
					ts.Fatalf("the namespace still holds %s", entry.Name())
				}
			}
		case "same":
			if err != nil || !bytes.Equal(data, ownedFile) {
				ts.Fatalf("the owned credential file is not the fixture's: %v", err)
			}
		case "changed":
			if err != nil || !bytes.Equal(data, expectedFile) || bytes.Equal(expectedFile, ownedFile) {
				ts.Fatalf("the owned credential file is not the one the action wrote: %v", err)
			}
		default:
			ts.Fatalf("unknown file state %q", args[0])
		}
		if opts["adopted"] {
			if data, err := os.ReadFile(adopted); err != nil || !bytes.Equal(data, adoptedFile) {
				ts.Fatalf("the adopted copy changed: %v", err)
			}
		}
		if opts["pending"] {
			if data, err := os.ReadFile(pending); err != nil || !bytes.Equal(data, pendingFile) {
				ts.Fatalf("the parked pending write changed: %v", err)
			}
		}
	})
	ts.SetCmd("seed-live", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 || args[0] != "unchanged" {
			ts.Fatalf("usage: seed-live unchanged")
		}
		if data, err := os.ReadFile(livePath); err != nil || !bytes.Equal(data, liveBefore) {
			ts.Fatalf("the live item changed: %v", err)
		}
	})
	ts.SetCmd("seed-contains", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 2 {
			ts.Fatalf("usage: seed-contains <file> <text>...")
		}
		data := ts.ReadFile(args[0])
		for _, text := range args[1:] {
			if !strings.Contains(data, text) {
				ts.Fatalf("%s does not contain %q", args[0], text)
			}
		}
	})
	ts.SetCmd("seed-writes", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: seed-writes <count>")
		}
		want, err := strconv.Atoi(args[0])
		ts.Check(err)
		assertArgvWrites(ts, ts.ReadFile(logPath), service, want)
	})
	ts.SetCmd("seed-calls", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 4 {
			ts.Fatalf("usage: seed-calls <preflights> <listings> <live-reads> <namespace-reads>")
		}
		got := make([]string, 0, 4)
		for _, match := range []func(string) bool{
			func(line string) bool { return strings.HasPrefix(line, "show-keychain-info") },
			func(line string) bool { return strings.HasPrefix(line, "dump-keychain") },
			func(line string) bool {
				return strings.HasPrefix(line, "find-generic-password") && strings.HasSuffix(line, " -s "+LiveService)
			},
			func(line string) bool {
				return strings.HasPrefix(line, "find-generic-password") && strings.HasSuffix(line, " -s "+service)
			},
		} {
			n := 0
			for line := range strings.SplitSeq(ts.ReadFile(logPath), "\n") {
				if match(line) {
					n++
				}
			}
			got = append(got, strconv.Itoa(n))
		}
		if diff := gocmp.Diff(args, got); diff != "" {
			ts.Fatalf("keychain calls preflight/listing/live/namespace (-want +got):\n%s", diff)
		}
	})
	ts.SetCmd("seed-quiet", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: seed-quiet")
		}
		if log := ts.ReadFile(logPath); log != "" {
			ts.Fatalf("the run made keychain calls:\n%s", log)
		}
	})
	ts.SetCmd("seed-audit", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: seed-audit <seed-entries> <last-outcome|-> <last-owned-file|->")
		}
		assertSeedAudit(ts, filepath.Join(configDir, "claude", "keychain-writes.jsonl"), args)
	})
	ts.SetCmd("seed-locks", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg {
			ts.Fatalf("usage: seed-locks [left-by-the-fixture...]")
		}
		var left []string
		for _, path := range []string{filepath.Join(nsDir, ".oauth_refresh.lock"), nsDir + ".lock", filepath.Join(nsDir, ".storage-write.lock")} {
			if _, err := os.Lstat(path); err == nil {
				left = append(left, filepath.Base(path))
			}
		}
		records, err := os.ReadDir(filepath.Join(configDir, "claude", "held-locks"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			ts.Fatalf("held-lock records: %v", err)
		}
		for _, record := range records {
			left = append(left, "held-locks/"+record.Name())
		}
		if diff := gocmp.Diff(args, left, emptyEqual); diff != "" {
			ts.Fatalf("lock artefacts left (-want +got):\n%s", diff)
		}
	})
	// seed-launched checks the fake claude's recorded argv, one argument per
	// line: the session's --mcp-config and its mcp.json, or nothing at all
	// under --no-mcp. The record is removed so the next run starts clean.
	ts.SetCmd("seed-launched", func(ts *testscript.TestScript, neg bool, args []string) {
		if len(args) > 1 || len(args) == 1 && args[0] != "--no-mcp" {
			ts.Fatalf("usage: [!] seed-launched [--no-mcp]")
		}
		argv, err := os.ReadFile(argvLog)
		launched := err == nil
		if neg {
			if launched {
				ts.Fatalf("the session was launched")
			}
			return
		}
		if !launched {
			ts.Fatalf("the session was not launched")
		}
		want := "--mcp-config\n" + filepath.Join(configDir, "claude-sessions", Acct, org, "mcp.json") + "\n"
		if len(args) == 1 {
			want = ""
		}
		if string(argv) != want {
			ts.Fatalf("the session's argv is %q, want %q", argv, want)
		}
		ts.Check(os.Remove(argvLog))
	})
	ts.SetCmd("seed-status", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: seed-status <stdout>")
		}
		var doc struct {
			Rows []struct {
				AccountUUID      string `json:"account_uuid"`
				OrganizationUUID string `json:"organization_uuid"`
				Kind             string `json:"kind"`
				Source           string `json:"source"`
				MCPOAuthEntries  *int   `json:"mcp_oauth_entries"`
			} `json:"rows"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[0])), &doc))
		for _, row := range doc.Rows {
			if row.AccountUUID == Acct && row.OrganizationUUID == org {
				// An owned row read from the keychain is how status shows a
				// migrated namespace when it was not asked to refresh.
				if row.Kind != "owned" || row.Source != "keychain" || row.MCPOAuthEntries == nil || *row.MCPOAuthEntries != 2 {
					ts.Fatalf("the namespace row is %+v, want an owned row read from the keychain with 2 MCP logins", row)
				}
				return
			}
		}
		ts.Fatalf("status has no row for the namespace")
	})
	ts.SetCmd("seed-clean", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: seed-clean")
		}
		var written []string
		if opts["file-state"] || !bytes.Equal(expectedFile, ownedFile) && bytes.Contains(expectedFile, []byte("FILE-SIBLING")) {
			written = append(written, file)
		}
		assertNoSiblingLeak(ts, root, itemDir, written...)
		log := ts.ReadFile(logPath)
		writes := 0
		for line := range strings.SplitSeq(log, "\n") {
			if strings.Contains(line, "SENTINEL") {
				ts.Fatalf("a keychain call carried credential material (%d bytes)", len(line))
			}
			if strings.HasPrefix(line, "add-generic-password") {
				writes++
			}
		}
		assertArgvWrites(ts, log, service, writes)
		for _, output := range []string{"stdout", "stderr"} {
			data := ts.ReadFile(output)
			if strings.Contains(data, SiblingCanary) || strings.Contains(data, "SENTINEL") || hexRun.MatchString(data) {
				ts.Fatalf("%s carried credential material", output)
			}
		}
	})
}

// emptyEqual makes a nil and an empty list of names compare equal.
var emptyEqual = gocmp.Comparer(func(a, b []string) bool { return slices.Equal(a, b) })

// seedReport is the part of `use <id> --json` a seed reports in.
type seedReport struct {
	Target        string  `json:"target"`
	Outcome       string  `json:"outcome"`
	Path          *string `json:"path"`
	SeededMembers []struct {
		Name    string         `json:"name"`
		Entries jsontext.Value `json:"entries"`
	} `json:"seeded_members"`
	SiblingsReadback *string `json:"siblings_readback"`
	Transport        *string `json:"transport"`
	OwnedFile        *string `json:"owned_file"`
	Note             *string `json:"note"`
}

// assertSeedReport checks the mcp_seed member of a `use <id> --json`
// document: its exact keys, its words, the members it seeded and, when
// given, that its note holds each substring.
func assertSeedReport(ts *testscript.TestScript, stdout, service string, args []string) {
	dec := jsontext.NewDecoder(strings.NewReader(stdout))
	value, err := dec.ReadValue()
	ts.Check(err)
	var doc map[string]jsontext.Value
	ts.Check(json.Unmarshal(value, &doc))
	raw, ok := doc["mcp_seed"]
	if !ok {
		ts.Fatalf("the document has no mcp_seed member")
	}
	if args[0] == "null" {
		if string(raw) != "null" {
			ts.Fatalf("mcp_seed = %s, want null", raw)
		}
		return
	}
	var keys map[string]jsontext.Value
	ts.Check(json.Unmarshal(raw, &keys))
	wantKeys := []string{"note", "outcome", "owned_file", "path", "seeded_members", "siblings_readback", "target", "transport"}
	gotKeys := make([]string, 0, len(keys))
	for key := range keys {
		gotKeys = append(gotKeys, key)
	}
	slices.Sort(gotKeys)
	if diff := gocmp.Diff(wantKeys, gotKeys); diff != "" {
		ts.Fatalf("mcp_seed keys (-want +got):\n%s", diff)
	}
	var report seedReport
	ts.Check(json.Unmarshal(raw, &report, json.RejectUnknownMembers(true)))
	if report.Target != service {
		ts.Fatalf("target = %s, want %s", report.Target, service)
	}
	field := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	got := []string{report.Outcome, field(report.Path), field(report.SiblingsReadback), field(report.Transport), field(report.OwnedFile)}
	if diff := gocmp.Diff(args[:5], got); diff != "" {
		ts.Fatalf("outcome/path/siblings_readback/transport/owned_file (-want +got):\n%s", diff)
	}
	members := []string{}
	for _, member := range report.SeededMembers {
		members = append(members, member.Name+"="+string(member.Entries))
	}
	wantMembers := []string{}
	if report.Transport != nil {
		wantMembers = seededCarry
	}
	if diff := gocmp.Diff(wantMembers, members); diff != "" {
		ts.Fatalf("seeded_members (-want +got):\n%s", diff)
	}
	for _, part := range args[5:] {
		if !strings.Contains(field(report.Note), part) {
			ts.Fatalf("the note %q does not contain %q", field(report.Note), part)
		}
	}
	if strings.Contains(field(report.Note), "SENTINEL") || strings.Contains(field(report.Note), SiblingCanary) {
		ts.Fatalf("the note carries credential material")
	}
}

// assertSeededItem checks a created namespace item: its account object is
// the owned file's, member for member in the encoder's form, and beside it
// are exactly the live item's members with their digests.
func assertSeededItem(ts *testscript.TestScript, document []byte, account jsontext.Value) {
	members, err := itemMembers(document)
	ts.Check(err)
	if len(members) == 0 || members[0].Name != "claudeAiOauth" {
		ts.Fatalf("the created item does not start with its account object: %v", memberDigests(members))
	}
	// The encoder reorders the account object's fields and spells the
	// optional ones it knows as null, so the object is compared field for
	// field, nulls aside; only digests are printed, never a token.
	semantic := func(value jsontext.Value) itemMember {
		var fields map[string]any
		ts.Check(json.Unmarshal(value, &fields))
		body, err := json.Marshal(withoutNulls(fields), json.Deterministic(true))
		ts.Check(err)
		return itemMember{Name: "claudeAiOauth", Value: body}
	}
	if diff := gocmp.Diff(memberDigests([]itemMember{semantic(account)}), memberDigests([]itemMember{semantic(members[0].Value)})); diff != "" {
		ts.Fatalf("the created item's account object is not the owned file's (-want +got):\n%s", diff)
	}
	seeded, err := siblingSet("seeded")
	ts.Check(err)
	want, err := canonicalMembers(seeded)
	ts.Check(err)
	if diff := gocmp.Diff(memberDigests(want), memberDigests(members[1:])); diff != "" {
		ts.Fatalf("the created item's members are not the live item's (-want +got):\n%s", diff)
	}
}

// withoutNulls returns fields with every null member removed, at any depth.
func withoutNulls(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for name, value := range fields {
		switch value := value.(type) {
		case nil:
		case map[string]any:
			out[name] = withoutNulls(value)
		default:
			out[name] = value
		}
	}
	return out
}

// assertSeedAudit checks the audit log: how many seed_write entries it
// holds, the last one's outcome and owned_file ("-" for absent), that no
// write entry was appended and that a seed entry never names an outgoing
// digest.
func assertSeedAudit(ts *testscript.TestScript, path string, args []string) {
	want, err := strconv.Atoi(args[0])
	ts.Check(err)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		ts.Fatalf("audit read: %v", err)
	}
	seeds, writes := 0, 0
	last := map[string]jsontext.Value{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		var row map[string]jsontext.Value
		ts.Check(json.Unmarshal([]byte(line), &row))
		switch string(row["event"]) {
		case `"seed_write"`:
			seeds++
			last = row
			if _, ok := row["from_digest8"]; ok {
				ts.Fatalf("a seed entry names an outgoing digest")
			}
		case `"write"`:
			writes++
		}
	}
	if seeds != want || writes != 0 {
		ts.Fatalf("audit seed/write entries = %d/%d, want %d/0", seeds, writes, want)
	}
	if bytes.Contains(data, []byte("SENTINEL")) || bytes.Contains(data, []byte(SiblingCanary)) {
		ts.Fatalf("the audit log carries credential material")
	}
	for i, key := range []string{"outcome", "owned_file"} {
		got := "-"
		if value, ok := last[key]; ok {
			var word string
			ts.Check(json.Unmarshal(value, &word))
			got = word
		}
		if got != args[1+i] {
			ts.Fatalf("the last seed entry's %s = %s, want %s", key, got, args[1+i])
		}
	}
}
