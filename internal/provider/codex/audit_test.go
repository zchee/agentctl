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

package codex

import (
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func TestCodexAuditEventsAreOrderedPrivateLines(t *testing.T) {
	tests := map[string]struct{ event CodexEvent }{
		"success: ambiguous":           {CodexEvent{Outcome: AuditAmbiguous, Class: new("interrupted"), Digest8Before: new("0123abcd")}},
		"success: resend":              {CodexEvent{Outcome: AuditResend, Digest8Before: new("0123abcd")}},
		"success: adopted":             {CodexEvent{Outcome: AuditAdoptedExternal, Digest8Before: new("0123abcd"), Digest8After: new("fedc9876")}},
		"success: dead":                {CodexEvent{Outcome: AuditNeedsLogin, Class: new("refresh_token_reused"), Digest8Before: new("0123abcd")}},
		"success: reset":               {CodexEvent{Outcome: AuditFloorReset}},
		"success: applied after error": {CodexEvent{Outcome: AuditApplied, Digest8Before: new("0123abcd"), Digest8After: new("fedc9876")}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if _, exists, err := ReadCodexAudit(t.Context(), paths); err != nil || exists {
				t.Fatalf("absent log: exists=%t error=%v", exists, err)
			}
			if err := AppendCodexEvent(t.Context(), paths, "user", "acct", test.event); err != nil {
				t.Fatal(err)
			}
			text, exists, err := ReadCodexAudit(t.Context(), paths)
			if err != nil || !exists {
				t.Fatalf("read: exists=%t error=%v", exists, err)
			}
			if strings.Count(text, "\n") != 1 || strings.Contains(text, "@") {
				t.Fatalf("invalid audit line: %q", text)
			}
			var entry CodexAuditEntry
			if err := json.Unmarshal([]byte(text), &entry); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.event.Outcome, entry.Outcome); diff != "" {
				t.Fatal(diff)
			}
			if entry.PID != uint32(os.Getpid()) || entry.Provider != "codex" || entry.UserID != "user" || entry.AccountID != "acct" {
				t.Fatalf("wrong provenance: %+v", entry)
			}
			info, err := os.Stat(CodexAuditLogPath(paths))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode=%o", info.Mode().Perm())
			}
			if !strings.Contains(text, `"digest8_before":`) || !strings.Contains(text, `"digest8_after":`) || strings.Contains(text, `"keychain_account":`) {
				t.Fatal("optional field serialization drift")
			}
		})
	}
}

func TestCodexAuditFieldGuardRefusesBeforeIO(t *testing.T) {
	tests := map[string]struct {
		user  string
		event CodexEvent
	}{
		"error: email":            {"someone@example.invalid", CodexEvent{Outcome: AuditFloorReset}},
		"error: path":             {"../x", CodexEvent{Outcome: AuditFloorReset}},
		"error: long digest":      {"user", CodexEvent{Outcome: AuditResend, Digest8Before: new("0123abcd0123abcd0123abcd0123abcd")}},
		"error: uppercase digest": {"user", CodexEvent{Outcome: AuditResend, Digest8Before: new("0123ABCD")}},
		"error: raw grant":        {"user", CodexEvent{Outcome: AuditApplied, Digest8After: new("planted-private-token")}},
		"error: class":            {"user", CodexEvent{Outcome: AuditAmbiguous, Class: new("planted-private-token")}},
		"error: keychain account on another outcome": {"user", CodexEvent{Outcome: AuditApplied, KeychainAccount: new("cli|00112233abcdefff")}},
		"error: keychain missing":                    {"user", CodexEvent{Outcome: AuditLoginKeychainGained}},
		"error: keychain hostile":                    {"user", CodexEvent{Outcome: AuditLoginKeychainGained, KeychainAccount: new("cli|00112233abcdefff\"; id; \"")}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			err := AppendCodexEvent(t.Context(), paths, test.user, "acct", test.event)
			if err == nil {
				t.Fatal("accepted forbidden field")
			}
			if strings.Contains(err.Error(), "planted-private-token") || strings.Contains(err.Error(), "example.invalid") {
				t.Fatal("refusal exposed planted value")
			}
			if _, err := os.Stat(paths.CodexRoot()); !os.IsNotExist(err) {
				t.Fatal("refusal performed I/O")
			}
		})
	}
}

func TestCodexAuditRefusesUnsafeLog(t *testing.T) {
	tests := map[string]struct{ link bool }{"error: symlink": {true}, "error: wrong mode": {false}}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureCodexDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			other := paths.CodexRoot() + "/elsewhere.jsonl"
			if err := os.WriteFile(other, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if test.link {
				if err := os.Symlink(other, CodexAuditLogPath(paths)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(CodexAuditLogPath(paths), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := AppendCodexEvent(t.Context(), paths, "user", "acct", CodexEvent{Outcome: AuditFloorReset}); err == nil {
				t.Fatal("unsafe log accepted")
			}
			data, err := os.ReadFile(other)
			if err != nil || len(data) != 0 {
				t.Fatalf("link target changed: %v", err)
			}
		})
	}
}

func TestCodexAuditGainedItemsStreamWholeHistory(t *testing.T) {
	paths := config.NewPaths(t.TempDir())
	account := "cli|00112233abcdefff"
	for range 2 {
		if err := AppendCodexEvent(t.Context(), paths, "verified-user", "verified-acct", CodexEvent{Outcome: AuditLoginKeychainGained, KeychainAccount: new(account)}); err != nil {
			t.Fatal(err)
		}
	}
	text, _, err := ReadCodexAudit(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "verified-user") || strings.Contains(text, "verified-acct") {
		t.Fatal("unverified identity recorded")
	}
	if err := os.WriteFile(CodexAuditLogPath(paths), []byte(strings.Repeat("x", 4097)+"\n"+text), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := GainedCodexKeychainAccounts(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff([]string{account}, found); diff != "" {
		t.Fatal(diff)
	}
}

func TestShownCodexAuditLineValidatesEveryField(t *testing.T) {
	good := `{"ts":"2026-09-22T00:00:00Z","agctl_pid":1,"provider":"codex","user_id":"user","account_id":"acct","outcome":"applied"}`
	shown, ok := ShownCodexAuditLine(good)
	if !ok {
		t.Fatal("valid old-shaped line refused")
	}
	if !strings.Contains(shown, `"digest8_before":null`) {
		t.Fatal("ordered serializer drift")
	}
	tests := map[string]struct{ from, to string }{
		"error: provider": {`"provider":"codex"`, `"provider":"$(id)"`},
		"error: user":     {`"user_id":"user"`, `"user_id":"user\u001b[2J"`},
		"error: account":  {`"account_id":"acct"`, `"account_id":"acct|1"`},
		"error: outcome":  {`"outcome":"applied"`, `"outcome":"made up"`},
		"error: digest":   {`"outcome":"applied"`, `"outcome":"applied","digest8_before":"nothex!!"`},
		"error: class":    {`"outcome":"applied"`, `"outcome":"applied","class":"made up"`},
		"error: keychain": {`"outcome":"applied"`, `"outcome":"applied","keychain_account":"cli|00112233abcdefff"`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, ok := ShownCodexAuditLine(strings.Replace(good, test.from, test.to, 1)); ok {
				t.Fatal("planted field shown")
			}
		})
	}
}
