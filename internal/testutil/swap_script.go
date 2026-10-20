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
	"crypto/sha256"
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"
)

func init() { registerScriptCmd("swapfixture", swapFixture) }

func swapFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 1 || len(args) > 2 {
		ts.Fatalf("usage: swapfixture <scenario> [name]")
	}
	kind := args[0]
	name := kind
	if len(args) == 2 {
		name = args[1]
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			ts.Fatalf("fixture name must be one path segment")
		}
	}
	scenarios := map[string]struct {
		live, absent, duplicate, expired, migrated, incomingAbsent, envToken, auditRefused, declined, failed, mismatch, profileUnavailable, newer, compact, busy bool
		lineLong, rotateLong, timeout, peerChange, outgoingUnavailable, outgoingMalformed, missingClaim, sessions, configBusy, configStale, configLink, saveRace bool
		unowned, unreadable, outgoingMismatch, dangling, outgoingExpired                                                                                         bool
		third                                                                                                                                                    bool
		siblings, fileSiblings, siblingsNear, siblingsGap                                                                                                        bool
		storageBusy, independent, ownerMissing, configAbsent, auditSymlink, ownAudit, alternateSpelling                                                          bool
	}{
		"namespace-storage-busy":       {storageBusy: true},
		"live-independent":             {live: true, independent: true},
		"live-owner-missing":           {live: true, ownerMissing: true, configAbsent: true},
		"live-duplicate-audit-refused": {live: true, duplicate: true, auditRefused: true},
		"live-newer-audit-refused":     {live: true, newer: true, auditRefused: true},
		"live-expired-own":             {live: true, duplicate: true, expired: true, outgoingExpired: true, ownAudit: true},
		"live-expired-audit-mode":      {live: true, duplicate: true, expired: true, outgoingExpired: true, auditRefused: true, ownAudit: true},
		"live-expired-audit-symlink":   {live: true, duplicate: true, expired: true, outgoingExpired: true, auditSymlink: true, ownAudit: true},
		"namespace":                    {},
		"namespace-duplicate":          {duplicate: true},
		"namespace-env-token":          {envToken: true},
		"namespace-unowned":            {unowned: true},
		"namespace-unreadable":         {unreadable: true},
		"live-outgoing-mismatch":       {live: true, outgoingMismatch: true},
		"live-dangling":                {live: true, dangling: true},
		"live-expired":                 {live: true, outgoingExpired: true},
		"first-write":                  {absent: true},
		"unmigrated-duplicate":         {absent: true, duplicate: true},
		"refresh":                      {expired: true},
		"json-inspect":                 {third: true, declined: true},
		"declined-expired":             {expired: true, declined: true},
		"migrated-expired":             {expired: true, migrated: true},
		"migrated-fresh":               {migrated: true},
		"incoming-keychain-only":       {migrated: true, incomingAbsent: true},
		"failed-write":                 {failed: true},
		"live":                         {live: true},
		"live-absent":                  {live: true, absent: true},
		"live-env-token":               {live: true, envToken: true},
		"live-audit-refused":           {live: true, auditRefused: true, expired: true},
		"namespace-audit-refused":      {auditRefused: true},
		"live-wrong-profile":           {live: true, mismatch: true},
		"live-profile-unavailable":     {live: true, profileUnavailable: true},
		"live-newer":                   {live: true, newer: true},
		"live-duplicate":               {live: true, duplicate: true},
		"live-compact-config":          {live: true, compact: true},
		"live-busy":                    {live: true, busy: true},
		"line-too-long":                {lineLong: true},
		"credential-too-large":         {expired: true, siblingsNear: true},
		"refresh-too-long":             {expired: true, siblingsGap: true, rotateLong: true, alternateSpelling: true},
		"carry-live":                   {live: true, siblings: true},
		"carry-namespace":              {siblings: true},
		"carry-third":                  {third: true, siblings: true},
		"carry-first-write-file":       {absent: true, fileSiblings: true},
		"carry-namespace-file":         {siblings: true, fileSiblings: true},
		"namespace-timeout":            {timeout: true},
		"first-write-timeout":          {absent: true, timeout: true},
		"live-timeout":                 {live: true, timeout: true},
		"refresh-peer-change":          {expired: true, peerChange: true},
		"live-peer-change":             {live: true, peerChange: true},
		"live-outgoing-unavailable":    {live: true, outgoingUnavailable: true},
		"live-outgoing-malformed":      {live: true, outgoingMalformed: true},
		"live-unclaimed":               {live: true, missingClaim: true},
		"live-session-hints":           {live: true, sessions: true},
		"live-catch-up-hints":          {live: true, duplicate: true, sessions: true},
		"live-config-busy":             {live: true, configBusy: true},
		"live-config-stale":            {live: true, configStale: true},
		"live-config-link":             {live: true, configLink: true},
		"refresh-save-race":            {expired: true, saveRace: true},
	}
	test, ok := scenarios[kind]
	if !ok {
		ts.Fatalf("unknown swap scenario %q", kind)
	}
	root := ts.MkAbs(filepath.Join("swap-cases", name))
	owner, incoming := Acct, "44444444-4444-4444-8444-444444444444"
	configDir, home, items := filepath.Join(root, "config"), filepath.Join(root, "home"), filepath.Join(root, "items")
	ownerDir := filepath.Join(configDir, "claude", owner, Org)
	incomingDir := filepath.Join(configDir, "claude", incoming, Org)
	liveDir := filepath.Join(home, ".claude")
	for _, dir := range []string{ownerDir, incomingDir, liveDir, filepath.Join(items, KeychainAccount)} {
		ts.Check(os.MkdirAll(dir, 0o700))
	}
	write := func(path string, body []byte) {
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, body, 0o600))
	}
	document := func(value any) []byte { body, err := json.Marshal(value); ts.Check(err); return body }
	blob := func(name, account string, expiry int64) []byte {
		return document(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "SENTINEL-access-" + name, "refreshToken": "SENTINEL-refresh-" + name, "expiresAt": expiry, "scopes": []string{"user:inference", "user:profile"}, "tokenAccount": map[string]any{"uuid": account, "organizationUuid": Org, "emailAddress": nil, "organizationName": nil, "workspaceId": nil, "workspaceName": nil}}})
	}
	expires := FreshAt()
	if test.expired {
		expires = ExpiredAt()
	}
	incomingName := "incoming"
	if test.lineLong {
		incomingName += strings.Repeat("x", 2500)
	}
	incomingBlob := blob(incomingName, incoming, expires)
	// An owned file that carries members beside the account object, which
	// must never reach the keychain item: a file is not a sibling source.
	incomingFile := incomingBlob
	if test.fileSiblings {
		members, err := itemMembers(incomingBlob)
		ts.Check(err)
		incomingFile = keychainDocument(members[0].Value, []itemMember{
			{Name: "mcpOAuth", Value: document(map[string]any{"file-server|00ff00ff": map[string]any{"accessToken": "FILE-SIBLING-mcp"}})},
			{Name: "pluginSecrets", Value: document(map[string]any{"file-plugin": map[string]any{"apiKey": "FILE-SIBLING-plugin"}})},
		})
	}
	outgoingBlob := blob("outgoing", owner, FreshAt())
	if test.outgoingExpired {
		outgoingBlob = blob("outgoing", owner, ExpiredAt())
	}
	if test.duplicate {
		outgoingBlob = incomingBlob
		if test.outgoingExpired {
			outgoingBlob = blob("incoming", incoming, ExpiredAt())
		}
	}
	if test.newer {
		outgoingBlob = blob("outgoing", incoming, FreshAt()+60000)
	}
	if test.missingClaim {
		outgoingBlob = blob("outgoing", "unclaimed-account", FreshAt())
	}
	ownerBlob := outgoingBlob
	if test.independent {
		ownerBlob = blob("independent", owner, FreshAt())
	}
	write(filepath.Join(ownerDir, ".credentials.json"), ownerBlob)
	if test.ownerMissing {
		ts.Check(os.Remove(filepath.Join(ownerDir, ".credentials.json")))
		ts.Check(os.Remove(ownerDir))
	}
	if !test.incomingAbsent {
		write(filepath.Join(incomingDir, ".credentials.json"), incomingFile)
	}
	livePlaintext := filepath.Join(liveDir, ".credentials.json")
	write(livePlaintext, outgoingBlob)
	if test.dangling {
		preserved := filepath.Join(root, "preserved-live")
		ts.Check(os.Rename(liveDir, preserved))
		ts.Check(os.Symlink(filepath.Join(root, "missing-live"), liveDir))
		livePlaintext = filepath.Join(preserved, ".credentials.json")
	}
	liveBefore := slices.Clone(outgoingBlob)
	incomingExport := incomingDir
	if test.alternateSpelling {
		incomingExport = filepath.Join(root, "incoming-export")
		ts.Check(os.Symlink(incomingDir, incomingExport))
	}
	owned := func(account, dir string) any {
		spelling := ExportSpelling(dir)
		return map[string]any{"account_uuid": account, "organization_uuid": Org, "kind": map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339)}
	}
	registryPath := filepath.Join(configDir, "config.json")
	records := []any{owned(owner, ownerDir), owned(incoming, incomingExport)}
	if test.third {
		third := "55555555-5555-4555-8555-555555555555"
		thirdDir := filepath.Join(configDir, "claude", third, Org)
		records = append(records, owned(third, thirdDir))
		outgoingBlob = blob("third", third, FreshAt())
		ts.Setenv("SWAP_THIRD_STORE", filepath.Join(thirdDir, ".credentials.json"))
	}
	registryBefore := document(map[string]any{"version": 1, "accounts": records, "forgotten_services": []string{}})
	write(registryPath, registryBefore)
	service := MigrationService(ownerDir)
	if test.live {
		service = LiveService
	}
	itemPath := filepath.Join(items, KeychainAccount, ItemFileName(service))
	itemDocument := outgoingBlob
	if test.siblings || test.siblingsNear || test.siblingsGap {
		members, err := itemMembers(outgoingBlob)
		ts.Check(err)
		var siblings []itemMember
		switch {
		case test.siblingsNear:
			// The siblings alone fill the ceiling, so any account object
			// beside them puts the document past it.
			siblings, err = paddedSiblings(0, KeychainBlobCeiling)
		case test.siblingsGap:
			// The incoming account object fits beside the siblings with
			// 2 KiB to spare; the rotated one, 8 KiB longer, does not.
			incomingMembers, merr := itemMembers(incomingBlob)
			ts.Check(merr)
			siblings, err = paddedSiblings(len(incomingMembers[0].Value), KeychainBlobCeiling-2048)
		default:
			siblings, err = siblingSet("seeded")
		}
		ts.Check(err)
		itemDocument = keychainDocument(members[0].Value, siblings)
	}
	if !test.absent {
		write(itemPath, itemDocument)
	}
	if test.migrated {
		write(filepath.Join(items, KeychainAccount, ItemFileName(MigrationService(incomingDir))), incomingBlob)
	}
	write(filepath.Join(items, ".allowed-services"), []byte(service+"\n"))
	write(filepath.Join(root, "security.log"), nil)
	write(filepath.Join(root, "dump"), []byte(dumpListing()))
	auditPath := filepath.Join(configDir, "claude", "keychain-writes.jsonl")
	var auditBefore []byte
	if test.ownAudit {
		auditBefore = append(document(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "monotonic_ms": 0, "agctl_pid": 1, "event": "write", "target": "live", "direction": "forward", "outcome": "applied", "from_digest8": nil, "to_digest8": Sha8("SENTINEL-access-incoming"), "incoming_identity": map[string]any{"account_uuid": incoming, "organization_uuid": Org}}), '\n')
		write(auditPath, auditBefore)
	}
	if test.auditRefused {
		write(auditPath, auditBefore)
		ts.Check(os.Chmod(auditPath, 0o644))
	}
	if test.auditSymlink {
		decoy := filepath.Join(root, "audit-decoy.jsonl")
		ts.Check(os.Rename(auditPath, decoy))
		ts.Check(os.Symlink(decoy, auditPath))
	}
	configBody := []byte("{\n  \"theme\": \"dark\"\n}")
	if test.compact {
		configBody = []byte(`{"theme":"dark"}`)
	}
	configPath := filepath.Join(home, ".claude.json")
	if test.configLink {
		real := filepath.Join(root, "config-target.json")
		write(real, configBody)
		ts.Check(os.Symlink(real, configPath))
	} else if !test.configAbsent {
		write(configPath, configBody)
	}
	var configLockMtime time.Time
	if test.configBusy || test.configStale {
		lock := configPath + ".lock"
		ts.Check(os.Mkdir(lock, 0o700))
		if test.configStale {
			old := time.Now().Add(-time.Minute)
			ts.Check(os.Chtimes(lock, old, old))
		}
		info, err := os.Stat(lock)
		ts.Check(err)
		configLockMtime = info.ModTime()
	}
	if test.sessions {
		write(filepath.Join(liveDir, "sessions", "session.json"), document(map[string]any{"pid": os.Getpid(), "name": "private-session-name", "bridgeSessionId": "bridge"}))
	}
	if test.busy {
		ts.Check(os.Mkdir(filepath.Join(liveDir, ".oauth_refresh.lock"), 0o700))
	}
	if test.storageBusy {
		ts.Check(os.Mkdir(filepath.Join(ownerDir, ".storage-write.lock"), 0o700))
	}
	for key, value := range map[string]string{
		"AGENTCTL_CONFIG_DIR": configDir, "HOME": home, "USER": KeychainAccount, "LOGNAME": KeychainAccount,
		"AGENTCTL_KEYCHAIN_BACKEND": "", "AGCTL_FAKE_SECURITY_ITEMS": items, "AGCTL_FAKE_SECURITY_DUMP": filepath.Join(root, "dump"), "AGCTL_FAKE_SECURITY_LOG": filepath.Join(root, "security.log"),
		"AGCTL_FAKE_SECURITY_WRITE_EXIT": "", "AGCTL_FAKE_SECURITY_FIND_EXIT": "", "AGCTL_FAKE_SECURITY_SLEEP": "", "AGCTL_FAKE_SECURITY_STDERR": "",
		"AGCTL_FAKE_WRITE_DELAY_MS": "", "AGCTL_FAKE_WRITE_DELAY_AFTER_MS": "", "AGCTL_FAKE_SECURITY_HOLD_PIPES": "", "AGCTL_FAKE_SECURITY_PIDFILE": "", "AGCTL_FAKE_SECURITY_FIND_EXIT_IF": "", "AGCTL_FAKE_SECURITY_FIND_EXIT_IF_CODE": "", "CLAUDE_CODE_OAUTH_TOKEN": "", "CLAUDE_CONFIG_DIR": "", "AGENTCTL_FAULT": "", "AGENTCTL_FAULT_RESUME": filepath.Join(root, "resume"),
		"CLAUDE_SECURESTORAGE_CONFIG_DIR": ExportSpelling(ownerDir), "INCOMING": incoming, "OWNER": owner, "SWAP_ROOT": root, "SWAP_STORE": ownerDir, "SWAP_INCOMING_STORE": incomingDir, "SWAP_AUDIT": auditPath,
	} {
		ts.Setenv(key, value)
	}
	if test.live {
		ts.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
		ts.Setenv("SWAP_STORE", liveDir)
	}
	if test.unowned {
		ts.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", ExportSpelling(filepath.Join(root, "unowned")))
	}
	if test.unreadable {
		ts.Setenv("AGCTL_FAKE_SECURITY_FIND_EXIT", "36")
		ts.Setenv("AGCTL_FAKE_SECURITY_STDERR", "The user name or passphrase you entered is not correct.")
	}
	if test.envToken {
		ts.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "SENTINEL-env")
	}
	if test.failed {
		ts.Setenv("AGCTL_FAKE_SECURITY_WRITE_EXIT", "1")
	}
	if original := ts.Getenv("AGCTL_FAKE_SWAP_REAL_SECURITY"); original != "" {
		ts.Setenv("AGENTCTL_SECURITY_BIN", original)
	}
	if test.timeout {
		security := ts.Getenv("AGENTCTL_SECURITY_BIN")
		wrapper := filepath.Join(root, "slow-write-security")
		write(wrapper, []byte("#!/bin/sh\n"+slowWriteClause+"exec \"$AGCTL_FAKE_SWAP_REAL_SECURITY\" \"$@\"\n"))
		ts.Check(os.Chmod(wrapper, 0o700))
		ts.Setenv("AGCTL_FAKE_SWAP_REAL_SECURITY", security)
		ts.Setenv("AGENTCTL_SECURITY_BIN", wrapper)
	} else if original := ts.Getenv("AGCTL_FAKE_SWAP_REAL_SECURITY"); original != "" {
		ts.Setenv("AGENTCTL_SECURITY_BIN", original)
	}
	var posts, profiles atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			if test.peerChange {
				write(filepath.Join(items, KeychainAccount, ItemFileName(service)), blob("peer", owner, FreshAt()))
			}
			if test.saveRace {
				write(filepath.Join(incomingDir, ".credentials.json"), blob("peer", incoming, FreshAt()))
			}
			access := "SENTINEL-rotated-access"
			if test.rotateLong {
				access += strings.Repeat("x", 8<<10)
			}
			say(w, `{"access_token":%q,"refresh_token":"SENTINEL-rotated-refresh","token_type":"Bearer","expires_in":28800,"scope":"user:inference user:profile"}`, access)
			return
		}
		profiles.Add(1)
		account := owner
		if strings.Contains(r.Header.Get("Authorization"), "incoming") || strings.Contains(r.Header.Get("Authorization"), "rotated") || test.newer || test.duplicate {
			account = incoming
		}
		if test.missingClaim && account == owner {
			account = "unclaimed-account"
		}
		if account == owner && test.outgoingUnavailable {
			w.WriteHeader(500)
			return
		}
		if account == owner && test.outgoingMalformed {
			say(w, `{"account":{"uuid":"SENTINEL-profile-body"}}`)
			return
		}
		if account == incoming && test.peerChange {
			write(filepath.Join(items, KeychainAccount, ItemFileName(service)), blob("peer", owner, FreshAt()))
		}
		if account == incoming && test.profileUnavailable {
			w.WriteHeader(500)
			return
		}
		if account == owner && test.outgoingMismatch {
			account = "wrong-outgoing-account"
		}
		if account == incoming && test.mismatch {
			account = "wrong-account"
		}
		say(w, `{"account":{"uuid":%q,"email":"user@example.invalid"},"organization":{"uuid":%q}}`, account, Org)
	}))
	ts.Defer(server.Close)
	ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+"/token")
	ts.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", server.URL+"/profile")
	var terminated, signalled *exec.Cmd
	ts.SetCmd("swap-action", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-action <wait-held|held|release-leak|drift|resume|terminate|signal-exit>")
		}
		switch args[0] {
		case "wait-held":
			if !WaitUntil(5*time.Second, func() bool {
				data, err := os.ReadFile(filepath.Join(root, "security.log"))
				return err == nil && bytes.Count(data, []byte("find-generic-password")) >= 2
			}) {
				ts.Fatalf("swap did not re-read the item under its hold")
			}
			// The held pause has no reached marker. Allow the read child to finish
			// before modifying or signalling the holder after its logged re-read.
			time.Sleep(300 * time.Millisecond)
			fallthrough
		case "held":
			dir := ts.Getenv("SWAP_STORE")
			for _, path := range []string{filepath.Join(dir, ".oauth_refresh.lock"), dir + ".lock", filepath.Join(dir, ".storage-write.lock")} {
				info, err := os.Stat(path)
				ts.Check(err)
				if !info.IsDir() {
					ts.Fatalf("peer-lock artifact is not a directory: %s", path)
				}
			}
			records, err := os.ReadDir(filepath.Join(configDir, "claude", "held-locks"))
			ts.Check(err)
			if len(records) != 1 {
				ts.Fatalf("observed %d held records; want 1", len(records))
			}
		case "release-leak":
			dir := ts.Getenv("SWAP_STORE")
			for _, path := range []string{filepath.Join(dir, ".oauth_refresh.lock"), dir + ".lock", filepath.Join(dir, ".storage-write.lock")} {
				ts.Check(os.Remove(path))
			}
			recordDir := filepath.Join(configDir, "claude", "held-locks")
			records, err := os.ReadDir(recordDir)
			ts.Check(err)
			for _, record := range records {
				ts.Check(os.Remove(filepath.Join(recordDir, record.Name())))
			}
		case "drift":
			old := time.Now().Add(-time.Hour)
			ts.Check(os.Chtimes(filepath.Join(ts.Getenv("SWAP_STORE"), ".oauth_refresh.lock"), old, old))
		case "resume":
			write(filepath.Join(root, "resume"), []byte("go"))
		case "terminate":
			background := ts.BackgroundCmds()
			if len(background) != 1 {
				ts.Fatalf("observed %d background children; want 1", len(background))
			}
			terminated = background[0]
			ts.Check(terminated.Process.Signal(syscall.SIGTERM))
		case "signal-exit":
			if terminated == nil || terminated.ProcessState == nil || terminated.ProcessState.ExitCode() != 143 {
				ts.Fatalf("held swap did not exit with status 143 after SIGTERM")
			}
		default:
			ts.Fatalf("unknown swap action %q", args[0])
		}
	})
	ts.SetCmd("swap-io", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: swap-io <reads> <preflights> <listings>")
		}
		log := ts.ReadFile(filepath.Join(root, "security.log"))
		for i, command := range []string{"find-generic-password", "show-keychain-info", "dump-keychain"} {
			want, err := strconv.Atoi(args[i])
			ts.Check(err)
			got := 0
			for line := range strings.SplitSeq(log, "\n") {
				if strings.HasPrefix(line, command) {
					got++
				}
			}
			if got != want {
				ts.Fatalf("%s count=%d; want=%d", command, got, want)
			}
		}
	})
	ts.SetCmd("swap-audit", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 2 || len(args) > 3 || len(args) == 3 && args[2] != "no-item" {
			ts.Fatalf("usage: swap-audit <write-outcome|none> <config-count> [no-item]")
		}
		wantConfig, err := strconv.Atoi(args[1])
		ts.Check(err)
		data, err := os.ReadFile(auditPath)
		if err != nil && !os.IsNotExist(err) {
			ts.Fatalf("audit read failed: %v", err)
		}
		writes, configs := 0, 0
		previousEvent, previousID := "", ""
		for line := range strings.SplitSeq(string(data), "\n") {
			if line == "" {
				continue
			}
			var row struct {
				Event       string         `json:"event"`
				Outcome     string         `json:"outcome"`
				TS          string         `json:"ts"`
				PID         int            `json:"agctl_pid"`
				After       *string        `json:"after"`
				Backup      *string        `json:"backup"`
				FromDigest8 jsontext.Value `json:"from_digest8"`
				ToDigest8   *string        `json:"to_digest8"`
			}
			ts.Check(json.Unmarshal([]byte(line), &row))
			switch row.Event {
			case "write":
				writes++
				if row.Outcome != args[0] {
					ts.Fatalf("write audit outcome=%s; want=%s", row.Outcome, args[0])
				}
				if len(args) == 3 && (!bytes.Equal(bytes.TrimSpace(row.FromDigest8), []byte("null")) || row.ToDigest8 == nil) {
					ts.Fatalf("first-write audit must record a null from_digest8 and a non-null to_digest8")
				}
			case "config_write":
				configs++
				if row.After != nil && (previousEvent != "write" || *row.After != previousID) {
					ts.Fatalf("config audit did not immediately follow the write it names")
				}
				if row.Backup != nil && !bytes.Equal([]byte(ts.ReadFile(filepath.Join(liveDir, "backups", *row.Backup))), configBody) {
					ts.Fatalf("config backup does not contain the original bytes")
				}
			case "lock_break":
				if test.storageBusy {
					ts.Fatalf("fresh storage lock was broken")
				}
			}
			previousEvent, previousID = row.Event, row.TS+"#"+strconv.Itoa(row.PID)
		}
		wantWrites := 1
		if args[0] == "none" {
			wantWrites = 0
		}
		if writes != wantWrites || configs != wantConfig {
			ts.Fatalf("audit write/config counts=%d/%d; want=%d/%d", writes, configs, wantWrites, wantConfig)
		}
	})
	ts.SetCmd("swap-config", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 || args[0] != "incoming" {
			ts.Fatalf("usage: swap-config incoming")
		}
		var configuration struct {
			Account struct {
				AccountUUID      string `json:"accountUuid"`
				OrganizationUUID string `json:"organizationUuid"`
			} `json:"oauthAccount"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(configPath)), &configuration))
		if configuration.Account.AccountUUID != incoming || configuration.Account.OrganizationUUID != Org {
			ts.Fatalf("config account identity was not updated to the incoming account")
		}
	})
	snapshotOutside := func() map[string]fs.FileMode {
		out := make(map[string]fs.FileMode)
		for _, tree := range []string{home, configDir} {
			ts.Check(filepath.WalkDir(tree, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if path == filepath.Join(configDir, "claude") {
					return filepath.SkipDir
				}
				if path == filepath.Join(configDir, "cache") || path == filepath.Join(configDir, "cache", "claude") {
					if !entry.IsDir() {
						ts.Fatalf("permitted cache path is not a directory: %s", path)
					}
					return nil
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				out[path] = info.Mode()
				return nil
			}))
		}
		return out
	}
	snapshotTree := func() map[string][]byte {
		out := make(map[string][]byte)
		for _, tree := range []string{home, configDir} {
			ts.Check(filepath.WalkDir(tree, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				out[path] = data
				return nil
			}))
		}
		return out
	}
	var treeBefore map[string][]byte
	if test.declined {
		treeBefore = snapshotTree()
	}
	outsideBefore := snapshotOutside()
	ts.SetCmd("swap-tree", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) > 1 || len(args) == 1 && (args[0] != "no-new-credential" || treeBefore == nil) {
			ts.Fatalf("usage: swap-tree [no-new-credential]")
		}
		if len(args) == 1 {
			for path, data := range snapshotTree() {
				if !bytes.Equal(treeBefore[path], data) && bytes.Contains(data, []byte("SENTINEL")) {
					ts.Fatalf("inspection created or rewrote credential material at %s", path)
				}
			}
			return
		}
		if diff := gocmp.Diff(outsideBefore, snapshotOutside()); diff != "" {
			ts.Fatalf("paths outside the namespace root changed (-before +after):\n%s", diff)
		}
	})
	passStarted := time.Now()
	ts.SetCmd("swap-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: swap-check <writes> <posts> <profiles>")
		}
		want := make([]int, 3)
		for i, arg := range args {
			n, err := strconv.Atoi(arg)
			ts.Check(err)
			want[i] = n
		}
		log := ts.ReadFile(filepath.Join(root, "security.log"))
		writes := 0
		for line := range strings.SplitSeq(log, "\n") {
			if strings.HasPrefix(line, "add-generic-password ") {
				writes++
			}
			if strings.Contains(line, "SENTINEL") || strings.HasPrefix(line, "delete-generic-password") {
				ts.Fatalf("unsafe security argv")
			}
		}
		if writes != want[0] || int(posts.Load()) != want[1] || int(profiles.Load()) != want[2] {
			ts.Fatalf("observed writes/posts/profiles=%d/%d/%d; want=%v", writes, posts.Load(), profiles.Load(), want)
		}
		if audit, err := os.ReadFile(auditPath); err == nil {
			if bytes.Contains(audit, []byte("SENTINEL")) {
				ts.Fatalf("audit contains a fixture credential")
			}
			if test.ownAudit && !bytes.Equal(audit, auditBefore) {
				ts.Fatalf("expired already-active return appended to its audit log")
			}
			if test.ownAudit {
				info, err := os.Lstat(auditPath)
				ts.Check(err)
				if test.auditSymlink && info.Mode()&os.ModeSymlink == 0 || test.auditRefused && info.Mode().Perm() != 0o644 {
					ts.Fatalf("expired pass changed the refused audit shape")
				}
			}
		} else if !os.IsNotExist(err) {
			ts.Fatalf("audit could not be inspected: %v", err)
		}
		for _, dir := range []string{ownerDir, incomingDir, liveDir} {
			for _, lock := range []string{filepath.Join(dir, ".oauth_refresh.lock"), dir + ".lock", filepath.Join(dir, ".storage-write.lock")} {
				if test.busy && lock == filepath.Join(liveDir, ".oauth_refresh.lock") {
					continue
				}
				if test.storageBusy && lock == filepath.Join(ownerDir, ".storage-write.lock") {
					if info, err := os.Stat(lock); err != nil || !info.IsDir() {
						ts.Fatalf("foreign storage lock was not preserved: %v", err)
					}
					continue
				}
				if _, err := os.Lstat(lock); !os.IsNotExist(err) {
					ts.Fatalf("swap left a peer-lock artifact: %s: %v", lock, err)
				}
			}
		}
		records, err := os.ReadDir(filepath.Join(configDir, "claude", "held-locks"))
		if err != nil && !os.IsNotExist(err) {
			ts.Fatalf("held-lock records could not be inspected: %v", err)
		}
		if len(records) != 0 {
			ts.Fatalf("swap left %d held-lock records", len(records))
		}
		if test.storageBusy && time.Since(passStarted) >= 12*time.Second {
			ts.Fatalf("third-lock contention waited through a stale sampling interval")
		}
		if !test.live && want[0] != 0 || test.independent || test.ownerMissing {
			var parked, displaced map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(ownerDir, ".credentials.adopted.json"))), &parked))
			ts.Check(json.Unmarshal(outgoingBlob, &displaced))
			if !gocmp.Equal(parked, displaced) {
				ts.Fatalf("adopted copy does not contain the complete displaced credential")
			}
		}
		if test.independent && !bytes.Equal([]byte(ts.ReadFile(filepath.Join(ownerDir, ".credentials.json"))), ownerBlob) {
			ts.Fatalf("live parking changed the independent owned grant")
		}
		if test.ownerMissing {
			info, err := os.Stat(ownerDir)
			ts.Check(err)
			entries, err := os.ReadDir(ownerDir)
			ts.Check(err)
			if info.Mode().Perm() != 0o700 || len(entries) != 1 || entries[0].Name() != ".credentials.adopted.json" {
				ts.Fatalf("new owner namespace has unexpected permissions or entries")
			}
			info, err = os.Stat(filepath.Join(ownerDir, ".credentials.adopted.json"))
			ts.Check(err)
			if info.Mode().Perm() != 0o600 {
				ts.Fatalf("parked credential permissions=%s; want 0600", info.Mode())
			}
		}
		if test.configAbsent {
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				ts.Fatalf("swap created an absent config: %v", err)
			}
		}
		if test.envToken || test.unowned {
			if log != "" {
				ts.Fatalf("scope or environment refusal spawned a keychain child")
			}
		}
		if test.unreadable {
			reads := 0
			for line := range strings.SplitSeq(log, "\n") {
				if strings.HasPrefix(line, "find-generic-password") {
					reads++
				}
			}
			if reads != 1 {
				ts.Fatalf("unreadable outgoing item caused %d reads; want 1", reads)
			}
		}
		if !bytes.Equal([]byte(ts.ReadFile(livePlaintext)), liveBefore) {
			ts.Fatalf("live plaintext credential changed")
		}
		if _, err := os.Stat(filepath.Join(liveDir, ".credentials.adopted.json")); !os.IsNotExist(err) {
			ts.Fatalf("adoption entered live tree: %v", err)
		}
		if test.declined || test.auditRefused && test.live || test.mismatch || test.envToken || test.unowned || test.unreadable || test.outgoingMismatch || test.outgoingExpired || test.dangling {
			if _, err := os.Stat(filepath.Join(ownerDir, ".credentials.adopted.json")); !os.IsNotExist(err) {
				ts.Fatalf("refusal or inspection persisted an adopted credential: %v", err)
			}
		}
		if test.expired && want[1] != 0 && !test.saveRace {
			data := ts.ReadFile(filepath.Join(incomingDir, ".credentials.json"))
			if !strings.Contains(data, "SENTINEL-rotated-access") {
				ts.Fatalf("refreshed incoming credential was not preserved")
			}
		}
		if test.saveRace && !strings.Contains(ts.ReadFile(filepath.Join(incomingDir, ".credentials.json")), "SENTINEL-access-peer") {
			ts.Fatalf("refresh overwrote the peer's incoming-store credential")
		}
		if test.configBusy || test.configStale {
			if !bytes.Equal([]byte(ts.ReadFile(configPath)), configBody) {
				ts.Fatalf("foreign config lock did not preserve config bytes")
			}
			info, err := os.Stat(configPath + ".lock")
			ts.Check(err)
			if !info.IsDir() || !info.ModTime().Equal(configLockMtime) {
				ts.Fatalf("foreign config lock was removed, replaced or re-stamped")
			}
		}
		if test.configLink {
			if info, err := os.Lstat(configPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
				ts.Fatalf("config symlink was replaced")
			}
		}
		if test.duplicate && !test.live && !bytes.Equal([]byte(ts.ReadFile(registryPath)), registryBefore) {
			ts.Fatalf("already-active namespace swap changed registry bytes")
		}
		if test.declined || test.envToken || test.unowned || test.unreadable {
			if !bytes.Equal([]byte(ts.ReadFile(filepath.Join(ownerDir, ".credentials.json"))), ownerBlob) || !bytes.Equal([]byte(ts.ReadFile(filepath.Join(incomingDir, ".credentials.json"))), incomingFile) {
				ts.Fatalf("refusal or inspection changed namespace credential bytes")
			}
		}
		if (!test.live || test.declined || test.envToken || test.unowned || test.unreadable || test.auditRefused) && !bytes.Equal([]byte(ts.ReadFile(configPath)), configBody) {
			ts.Fatalf("namespace operation or refused plan changed config bytes")
		}
		if test.compact && !bytes.Equal([]byte(ts.ReadFile(filepath.Join(home, ".claude.json"))), configBody) {
			ts.Fatalf("compact config was rewritten")
		}
	})
	accounts := map[string]string{"incoming": incoming, "owner": owner, "third": "55555555-5555-4555-8555-555555555555"}
	ts.SetCmd("swap-item", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 || accounts[args[1]] == "" {
			ts.Fatalf("usage: swap-item <seeded|changed|none> <incoming|owner|third>")
		}
		assertCarriedItem(ts, ts.ReadFile(itemPath), args[0], accounts[args[1]])
	})
	ts.SetCmd("swap-sibling", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-sibling <seeded|changed|grown|duplicated>")
		}
		rewriteSiblings(ts, itemPath, args[0])
	})
	ts.SetCmd("swap-report", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 4 || len(args) > 5 {
			ts.Fatalf("usage: swap-report <stdout-file> <seeded|none|-> <equal|differs|unverified|-> <argv|-> [note-member]")
		}
		assertCarryReport(ts, ts.ReadFile(args[0]), args[1:])
	})
	ts.SetCmd("swap-argv", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 1 || len(args) > 2 || len(args) == 2 && args[1] != "sized" {
			ts.Fatalf("usage: swap-argv <writes> [sized]")
		}
		want, err := strconv.Atoi(args[0])
		ts.Check(err)
		sizes := assertArgvWrites(ts, ts.ReadFile(filepath.Join(root, "security.log")), service, want)
		if len(args) == 2 {
			if got := len(ts.ReadFile(itemPath)); len(sizes) == 0 || sizes[len(sizes)-1] != 2*got {
				ts.Fatalf("the last write's hex length %v is not twice the stored document's %d bytes", sizes, got)
			}
		}
	})
	ts.SetCmd("swap-clean", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: swap-clean")
		}
		var seeded []string
		if test.fileSiblings {
			seeded = append(seeded, filepath.Join(incomingDir, ".credentials.json"))
		}
		assertNoSiblingLeak(ts, root, filepath.Join(items, KeychainAccount), seeded...)
	})
	ts.SetCmd("swap-audit-carry", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: swap-audit-carry <seeded|none> <equal|differs|unverified|-> <argv|->")
		}
		assertAuditCarry(ts, auditPath, args)
	})
	ts.SetCmd("swap-pause", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: swap-pause")
		}
		awaitPause(ts, filepath.Join(root, "resume"))
	})
	ts.SetCmd("swap-group-gone", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-group-gone <pidfile>")
		}
		assertGroupGone(ts, ts.MkAbs(args[0]))
	})
	ts.SetCmd("swap-signal", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: swap-signal <INT|TERM|HUP> <pidfile>")
		}
		signalled = signalDuringWrite(ts, args[0], ts.MkAbs(args[1]), filepath.Join(ts.Getenv("SWAP_STORE"), ".oauth_refresh.lock"))
	})
	ts.SetCmd("swap-absent", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-absent <text>")
		}
		ts.Check(filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return err
			}
			data, err := os.ReadFile(path)
			if err == nil && bytes.Contains(data, []byte(args[0])) {
				relative, _ := filepath.Rel(root, path)
				ts.Fatalf("%s holds the marker", relative)
			}
			return err
		}))
	})
	ts.SetCmd("swap-store-entries", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg {
			ts.Fatalf("usage: swap-store-entries [name...]")
		}
		entries, err := os.ReadDir(ts.Getenv("SWAP_STORE"))
		ts.Check(err)
		got := []string{}
		for _, entry := range entries {
			got = append(got, entry.Name())
		}
		if diff := gocmp.Diff(append([]string{}, args...), got); diff != "" {
			ts.Fatalf("the namespace store holds other entries (-want +got):\n%s", diff)
		}
	})
	ts.SetCmd("swap-signal-exit", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-signal-exit <status>")
		}
		want, err := strconv.Atoi(args[0])
		ts.Check(err)
		if signalled == nil || signalled.ProcessState == nil || signalled.ProcessState.ExitCode() != want {
			ts.Fatalf("the signalled swap did not exit with status %d", want)
		}
	})
}

// memberDigests describes members by name, length and digest only, so a
// mismatch can be reported without printing a value.
func memberDigests(members []itemMember) []string {
	out := make([]string, 0, len(members))
	for _, member := range members {
		sum := sha256.Sum256(member.Value)
		out = append(out, fmt.Sprintf("%s %dB %x", member.Name, len(member.Value), sum[:6]))
	}
	return out
}

// assertCarriedItem checks a keychain document after a carry: its account
// object belongs to account, and beside it are exactly the sibling set kind
// ("none" for no siblings), member for member in the production encoder's
// form and in the seeded order.
func assertCarriedItem(ts *testscript.TestScript, document, kind, account string) {
	members, err := itemMembers([]byte(document))
	ts.Check(err)
	if len(members) == 0 || members[0].Name != "claudeAiOauth" {
		ts.Fatalf("the keychain document does not start with its account object: %v", memberDigests(members))
	}
	var oauth struct {
		TokenAccount struct {
			UUID string `json:"uuid"`
		} `json:"tokenAccount"`
	}
	ts.Check(json.Unmarshal(members[0].Value, &oauth))
	if oauth.TokenAccount.UUID != account {
		ts.Fatalf("the keychain item holds account %q, want %q", oauth.TokenAccount.UUID, account)
	}
	var want []itemMember
	if kind != "none" {
		seeded, err := siblingSet(kind)
		ts.Check(err)
		want, err = canonicalMembers(seeded)
		ts.Check(err)
	}
	if diff := gocmp.Diff(memberDigests(want), memberDigests(members[1:])); diff != "" {
		ts.Fatalf("the keychain item's siblings are not the %s set (-want +got):\n%s", kind, diff)
	}
}

// rewriteSiblings replaces the siblings of the keychain item at path with
// the set kind and keeps its account object byte for byte, the way another
// process refreshing one MCP login would.
func rewriteSiblings(ts *testscript.TestScript, path, kind string) {
	members, err := itemMembers([]byte(ts.ReadFile(path)))
	ts.Check(err)
	siblings, err := siblingSet(kind)
	ts.Check(err)
	temporary := path + ".rewrite"
	ts.Check(os.WriteFile(temporary, keychainDocument(members[0].Value, siblings), 0o600))
	ts.Check(os.Rename(temporary, path))
}

// carryOutcome is the part of a swap's JSON outcome the carry reports on.
type carryOutcome struct {
	Kind           string `json:"kind"`
	CarriedMembers *[]struct {
		Name    string         `json:"name"`
		Entries jsontext.Value `json:"entries"`
	} `json:"carried_members"`
	SiblingsReadback *string `json:"siblings_readback"`
	Transport        *string `json:"transport"`
	Note             *string `json:"note"`
	Lock             struct {
		HoldMS *int64 `json:"hold_ms"`
	} `json:"lock"`
}

// holdBudgetMS is the production hold budget a write's hold must stay
// under, timeouts and the writer's wait delay included.
const holdBudgetMS = 3000

// seededCarry is what carried_members says for the seeded sibling set.
var seededCarry = []string{"mcpOAuth=2", "mcpOAuthClientConfig=1", "pluginSecrets=1"}

// assertCarryReport checks the outcome document in a swap's JSON output:
// carried_members ("seeded", "none" for an empty list, "-" for absent), the
// siblings_readback and transport values ("-" for absent), and, when given,
// that the note names a member.
func assertCarryReport(ts *testscript.TestScript, stdout string, args []string) {
	var outcome *carryOutcome
	dec := jsontext.NewDecoder(strings.NewReader(stdout))
	for {
		value, err := dec.ReadValue()
		if err == io.EOF {
			break
		}
		ts.Check(err)
		var doc carryOutcome
		ts.Check(json.Unmarshal(value, &doc))
		if doc.Kind == "outcome" || outcome == nil {
			outcome = &doc
		}
	}
	if outcome == nil {
		ts.Fatalf("the output holds no JSON outcome")
	}
	if hold := outcome.Lock.HoldMS; hold != nil && *hold >= holdBudgetMS {
		ts.Fatalf("the hold lasted %d ms, not under the %d ms budget", *hold, holdBudgetMS)
	}
	var carried []string
	if outcome.CarriedMembers != nil {
		carried = []string{}
		for _, member := range *outcome.CarriedMembers {
			carried = append(carried, member.Name+"="+string(member.Entries))
		}
	}
	var wantCarried []string
	switch args[0] {
	case "seeded":
		wantCarried = seededCarry
	case "none":
		wantCarried = []string{}
	}
	if diff := gocmp.Diff(wantCarried, carried); diff != "" {
		ts.Fatalf("carried_members (-want +got):\n%s", diff)
	}
	for i, field := range []*string{outcome.SiblingsReadback, outcome.Transport} {
		want, name := args[1+i], []string{"siblings_readback", "transport"}[i]
		got := "-"
		if field != nil {
			got = *field
		}
		if got != want {
			ts.Fatalf("%s = %s, want %s", name, got, want)
		}
	}
	if len(args) == 4 && (outcome.Note == nil || !strings.Contains(*outcome.Note, args[3])) {
		ts.Fatalf("the outcome's note does not name %s", args[3])
	}
}

// linuxArgvBudget is the largest document a write the battery lets reach
// the fake may carry. Linux caps one argv element at 128 KiB, and the hex
// payload is twice the document, so every case that reaches the fake stays
// under 60 KiB of document.
const linuxArgvBudget = 60 << 10

// assertArgvWrites checks every keychain write the fake logged: exactly the
// redacted argv form for this fixture's account and service, an even hex
// length within the Linux argv budget, want of them in all. It returns the
// hex lengths in order.
func assertArgvWrites(ts *testscript.TestScript, log, service string, want int) []int {
	form := regexp.MustCompile(`^add-generic-password -U -a "` + regexp.QuoteMeta(KeychainAccount) + `" -s "` + regexp.QuoteMeta(service) + `" -X <REDACTED:([0-9]+)>$`)
	var sizes []int
	for line := range strings.SplitSeq(log, "\n") {
		if !strings.HasPrefix(line, "add-generic-password") {
			continue
		}
		match := form.FindStringSubmatch(line)
		if match == nil {
			ts.Fatalf("a keychain write was not logged in the redacted argv form (%d bytes)", len(line))
		}
		n, err := strconv.Atoi(match[1])
		ts.Check(err)
		if n%2 != 0 || n/2 > linuxArgvBudget {
			ts.Fatalf("a keychain write carried %d hex digits: odd, or a document over the %d-byte Linux argv budget", n, linuxArgvBudget)
		}
		sizes = append(sizes, n)
	}
	if len(sizes) != want {
		ts.Fatalf("observed %d keychain writes, want %d", len(sizes), want)
	}
	if len(sizes) != 0 {
		ts.Logf("largest keychain document a write handed the fake: %d bytes", slices.Max(sizes)/2)
	}
	return sizes
}

// hexRun is a run of hexadecimal digits longer than a SHA-256 digest: a
// payload, never a fingerprint.
var hexRun = regexp.MustCompile(`[0-9A-Fa-f]{65,}`)

// assertNoSiblingLeak walks every file under root except the keychain item
// files directly in itemDir: none may hold the sibling canary, the seeded
// MCP token filler or a run of hex digits longer than a digest, and no
// credentials file may name a sibling member, except the files listed in
// seeded, which the fixture itself wrote that way.
func assertNoSiblingLeak(ts *testscript.TestScript, root, itemDir string, seeded ...string) {
	ts.Check(filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !entry.Type().IsRegular() || filepath.Dir(path) == itemDir {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if bytes.Contains(data, []byte(SiblingCanary)) || bytes.Contains(data, []byte("mcp-alpha-")) {
			ts.Fatalf("%s holds a sibling value", relative)
		}
		if hexRun.Match(data) {
			ts.Fatalf("%s holds a run of more than 64 hexadecimal digits", relative)
		}
		if strings.HasPrefix(entry.Name(), ".credentials") && !slices.Contains(seeded, path) && (bytes.Contains(data, []byte("mcpOAuth")) || bytes.Contains(data, []byte("pluginSecrets"))) {
			ts.Fatalf("%s names a sibling member", relative)
		}
		return nil
	}))
}

// assertAuditCarry checks the last write entry of the audit log: the
// member names it carried ("seeded" or "none"), siblings_readback and
// transport ("-" for absent).
func assertAuditCarry(ts *testscript.TestScript, auditPath string, args []string) {
	type writeCarry struct {
		Event            string   `json:"event"`
		Carried          []string `json:"carried"`
		SiblingsReadback *string  `json:"siblings_readback"`
		Transport        *string  `json:"transport"`
	}
	var last *writeCarry
	for line := range strings.SplitSeq(ts.ReadFile(auditPath), "\n") {
		if line == "" {
			continue
		}
		var row writeCarry
		ts.Check(json.Unmarshal([]byte(line), &row))
		if row.Event == "write" {
			last = &row
		}
	}
	if last == nil {
		ts.Fatalf("the audit log holds no write entry")
	}
	var want []string
	if args[0] == "seeded" {
		want = []string{"mcpOAuth", "mcpOAuthClientConfig", "pluginSecrets"}
	}
	if diff := gocmp.Diff(want, last.Carried, cmpopts.EquateEmpty()); diff != "" {
		ts.Fatalf("the audit's carried names (-want +got):\n%s", diff)
	}
	for i, field := range []*string{last.SiblingsReadback, last.Transport} {
		want, name := args[1+i], []string{"siblings_readback", "transport"}[i]
		got := "-"
		if field != nil {
			got = *field
		}
		if got != want {
			ts.Fatalf("the audit's %s = %s, want %s", name, got, want)
		}
	}
}

// awaitPause waits for the acknowledged pause point behind resume to report
// that the run is held there, then clears the marker.
func awaitPause(ts *testscript.TestScript, resume string) {
	reached := resume + ".reached"
	if !WaitUntil(10*time.Second, func() bool { _, err := os.Stat(reached); return err == nil }) {
		ts.Fatalf("the run never reached its pause point")
	}
	ts.Check(os.Remove(reached))
}

// assertGroupGone checks that no member of the fake's process group, whose
// leader wrote pidfile, is left once the command under test has returned.
func assertGroupGone(ts *testscript.TestScript, pidfile string) {
	pid, ok := readCount(pidfile)
	if !ok || pid <= 1 {
		ts.Fatalf("the fake wrote no usable pidfile")
	}
	if !WaitUntil(2*time.Second, func() bool { return errors.Is(unix.Kill(-pid, 0), unix.ESRCH) }) {
		ts.Fatalf("the fake's process group %d outlived the write", pid)
	}
}

// signalDuringWrite sends sig to the one background command while the
// fake's leader is beating in its delay under the hold, then checks the
// order the teardown promises: the leader's last heartbeat comes before the
// hold directory is removed, and no beat follows. It returns the command.
func signalDuringWrite(ts *testscript.TestScript, name, pidfile, holdDir string) *exec.Cmd {
	sig, ok := map[string]os.Signal{"INT": unix.SIGINT, "TERM": unix.SIGTERM, "HUP": unix.SIGHUP}[name]
	if !ok {
		ts.Fatalf("unknown signal %q", name)
	}
	background := ts.BackgroundCmds()
	if len(background) != 1 {
		ts.Fatalf("observed %d background commands; want 1", len(background))
	}
	heartbeat := pidfile + ".heartbeat"
	if !WaitUntil(10*time.Second, func() bool { beats, ok := readCount(heartbeat); return ok && beats >= 2 }) {
		ts.Fatalf("the fake never beat twice in its write delay")
	}
	if _, err := os.Stat(holdDir); err != nil {
		ts.Fatalf("the write is not running under the hold: %v", err)
	}
	ts.Check(background[0].Process.Signal(sig))
	var lastBeat []byte
	if !WaitUntil(10*time.Second, func() bool {
		if _, err := os.Lstat(holdDir); !errors.Is(err, os.ErrNotExist) {
			return false
		}
		data, err := os.ReadFile(heartbeat)
		lastBeat = data
		return err == nil
	}) {
		ts.Fatalf("the hold directory was never removed after %s", name)
	}
	time.Sleep(200 * time.Millisecond)
	if data, err := os.ReadFile(heartbeat); err != nil || !bytes.Equal(data, lastBeat) {
		ts.Fatalf("the fake's leader beat after the hold directory was removed")
	}
	return background[0]
}
