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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
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
	}{
		"namespace":                 {},
		"first-write":               {absent: true},
		"unmigrated-duplicate":      {absent: true, duplicate: true},
		"refresh":                   {expired: true},
		"json-inspect":              {expired: true, declined: true},
		"migrated-expired":          {expired: true, migrated: true},
		"migrated-fresh":            {migrated: true},
		"incoming-keychain-only":    {migrated: true, incomingAbsent: true},
		"failed-write":              {failed: true},
		"live":                      {live: true},
		"live-absent":               {live: true, absent: true},
		"live-env-token":            {live: true, envToken: true},
		"live-audit-refused":        {live: true, auditRefused: true, expired: true},
		"namespace-audit-refused":   {auditRefused: true},
		"live-wrong-profile":        {live: true, mismatch: true},
		"live-profile-unavailable":  {live: true, profileUnavailable: true},
		"live-newer":                {live: true, newer: true},
		"live-duplicate":            {live: true, duplicate: true},
		"live-compact-config":       {live: true, compact: true},
		"live-busy":                 {live: true, busy: true},
		"line-too-long":             {lineLong: true},
		"refresh-too-long":          {expired: true, rotateLong: true},
		"namespace-timeout":         {timeout: true},
		"first-write-timeout":       {absent: true, timeout: true},
		"live-timeout":              {live: true, timeout: true},
		"refresh-peer-change":       {expired: true, peerChange: true},
		"live-peer-change":          {live: true, peerChange: true},
		"live-outgoing-unavailable": {live: true, outgoingUnavailable: true},
		"live-outgoing-malformed":   {live: true, outgoingMalformed: true},
		"live-unclaimed":            {live: true, missingClaim: true},
		"live-session-hints":        {live: true, sessions: true},
		"live-catch-up-hints":       {live: true, duplicate: true, sessions: true},
		"live-config-busy":          {live: true, configBusy: true},
		"live-config-stale":         {live: true, configStale: true},
		"live-config-link":          {live: true, configLink: true},
		"refresh-save-race":         {expired: true, saveRace: true},
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
		return document(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "SENTINEL-access-" + name, "refreshToken": "SENTINEL-refresh-" + name, "expiresAt": expiry, "scopes": []string{"user:inference", "user:profile"}, "tokenAccount": map[string]any{"uuid": account, "organizationUuid": Org}}})
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
	outgoingBlob := blob("outgoing", owner, FreshAt())
	if test.duplicate {
		outgoingBlob = incomingBlob
	}
	if test.newer {
		outgoingBlob = blob("outgoing", incoming, FreshAt()+60000)
	}
	if test.missingClaim {
		outgoingBlob = blob("outgoing", "unclaimed-account", FreshAt())
	}
	write(filepath.Join(ownerDir, ".credentials.json"), outgoingBlob)
	if !test.incomingAbsent {
		write(filepath.Join(incomingDir, ".credentials.json"), incomingBlob)
	}
	write(filepath.Join(liveDir, ".credentials.json"), outgoingBlob)
	liveBefore := slices.Clone(outgoingBlob)
	owned := func(account, dir string) any {
		spelling := ExportSpelling(dir)
		return map[string]any{"account_uuid": account, "organization_uuid": Org, "kind": map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339)}
	}
	write(filepath.Join(configDir, "config.json"), document(map[string]any{"version": 1, "accounts": []any{owned(owner, ownerDir), owned(incoming, incomingDir)}, "forgotten_services": []string{}}))
	service := MigrationService(ownerDir)
	if test.live {
		service = LiveService
	}
	if !test.absent {
		write(filepath.Join(items, KeychainAccount, ItemFileName(service)), outgoingBlob)
	}
	if test.migrated {
		write(filepath.Join(items, KeychainAccount, ItemFileName(MigrationService(incomingDir))), incomingBlob)
	}
	write(filepath.Join(items, ".allowed-services"), []byte(service+"\n"))
	write(filepath.Join(root, "security.log"), nil)
	write(filepath.Join(root, "dump"), []byte(dumpListing()))
	auditPath := filepath.Join(configDir, "claude", "keychain-writes.jsonl")
	if test.auditRefused {
		write(auditPath, nil)
		ts.Check(os.Chmod(auditPath, 0o644))
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
	} else {
		write(configPath, configBody)
	}
	if test.configBusy || test.configStale {
		lock := configPath + ".lock"
		ts.Check(os.Mkdir(lock, 0o700))
		if test.configStale {
			old := time.Now().Add(-time.Hour)
			ts.Check(os.Chtimes(lock, old, old))
		}
	}
	if test.sessions {
		write(filepath.Join(liveDir, "sessions", "session.json"), document(map[string]any{"pid": os.Getpid(), "name": "private-session-name", "bridgeSessionId": "bridge"}))
	}
	if test.busy {
		ts.Check(os.Mkdir(filepath.Join(liveDir, ".oauth_refresh.lock"), 0o700))
	}
	for key, value := range map[string]string{
		"AGENTCTL_CONFIG_DIR": configDir, "HOME": home, "USER": KeychainAccount, "LOGNAME": KeychainAccount,
		"AGENTCTL_KEYCHAIN_BACKEND": "", "AGCTL_FAKE_SECURITY_ITEMS": items, "AGCTL_FAKE_SECURITY_DUMP": filepath.Join(root, "dump"), "AGCTL_FAKE_SECURITY_LOG": filepath.Join(root, "security.log"),
		"AGCTL_FAKE_SECURITY_WRITE_EXIT": "", "AGCTL_FAKE_SECURITY_FIND_EXIT": "", "AGCTL_FAKE_SECURITY_SLEEP": "", "AGCTL_FAKE_SECURITY_STDERR": "", "CLAUDE_CODE_OAUTH_TOKEN": "", "CLAUDE_CONFIG_DIR": "", "AGENTCTL_FAULT": "",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR": ExportSpelling(ownerDir), "INCOMING": incoming, "OWNER": owner, "SWAP_ROOT": root, "SWAP_STORE": ownerDir, "SWAP_AUDIT": auditPath,
	} {
		ts.Setenv(key, value)
	}
	if test.live {
		ts.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
		ts.Setenv("SWAP_STORE", liveDir)
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
		write(wrapper, []byte("#!/bin/sh\nif [ \"$1\" = '-i' ]; then printf '%s\\n' '-i' >> \"$AGCTL_FAKE_SECURITY_LOG\"; sleep 5; fi\nexec \"$AGCTL_FAKE_SWAP_REAL_SECURITY\" \"$@\"\n"))
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
				access += strings.Repeat("x", 2500)
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
		if account == incoming && test.mismatch {
			account = "wrong-account"
		}
		say(w, `{"account":{"uuid":%q,"email":"user@example.invalid"},"organization":{"uuid":%q}}`, account, Org)
	}))
	ts.Defer(server.Close)
	ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+"/token")
	ts.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", server.URL+"/profile")
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
			if line == "-i" {
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
		} else if !os.IsNotExist(err) {
			ts.Fatalf("audit could not be inspected: %v", err)
		}
		for _, dir := range []string{ownerDir, incomingDir, liveDir} {
			for _, lock := range []string{filepath.Join(dir, ".oauth_refresh.lock"), dir + ".lock", filepath.Join(dir, ".storage-write.lock")} {
				if test.busy && lock == filepath.Join(liveDir, ".oauth_refresh.lock") {
					continue
				}
				if _, err := os.Lstat(lock); !os.IsNotExist(err) {
					ts.Fatalf("swap left a peer-lock artifact: %s: %v", lock, err)
				}
			}
		}
		if !bytes.Equal([]byte(ts.ReadFile(filepath.Join(liveDir, ".credentials.json"))), liveBefore) {
			ts.Fatalf("live plaintext credential changed")
		}
		if _, err := os.Stat(filepath.Join(liveDir, ".credentials.adopted.json")); !os.IsNotExist(err) {
			ts.Fatalf("adoption entered live tree: %v", err)
		}
		if test.declined || test.auditRefused && test.live || test.mismatch || test.envToken {
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
			if _, err := os.Stat(configPath + ".lock"); err != nil {
				ts.Fatalf("foreign config lock was removed: %v", err)
			}
		}
		if test.configLink {
			if info, err := os.Lstat(configPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
				ts.Fatalf("config symlink was replaced")
			}
		}
		if test.compact && !bytes.Equal([]byte(ts.ReadFile(filepath.Join(home, ".claude.json"))), configBody) {
			ts.Fatalf("compact config was rewritten")
		}
	})
}
