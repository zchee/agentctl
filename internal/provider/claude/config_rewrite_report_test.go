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

package claude

import (
	json "encoding/json/v2"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/secret"
)

func TestConfigReportRendering(t *testing.T) {
	r := ConfigNotAttempted(secret.ConfigReasonProfileUnavailable)
	r.Account = &secret.IncomingIdentity{AccountUUID: "private-id"}
	got, err := json.Marshal(r.JSON())
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]any
	if err := json.Unmarshal(got, &members); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"outcome": "not_attempted", "reason": "profile_unavailable", "backup": nil, "hold_ms": nil, "budget_ms": float64(1200)}
	if diff := gocmp.Diff(want, members); diff != "" {
		t.Fatalf("JSON (-want +got):\n%s", diff)
	}
	if strings.Contains(string(got), "private-id") {
		t.Fatal("JSON exposed account")
	}
	if r.NotUpdated() == nil || *r.NotUpdated() != secret.ConfigReasonProfileUnavailable {
		t.Fatal("missing refusal reason")
	}
	record := r.Record(new("write-id"))
	if record.After == nil || *record.After != "write-id" || record.Account.AccountUUID != "private-id" {
		t.Fatalf("record = %+v", record)
	}
	r.Outcome = secret.ConfigApplied
	if r.NotUpdated() != nil {
		t.Fatal("applied report retained refusal")
	}
	if CompletionClause() != "running sessions show the new account within a second; " {
		t.Fatal("completion clause changed")
	}
}

func TestConfigNotices(t *testing.T) {
	tests := map[string]struct {
		reason   secret.ConfigReason
		recovery ConfigRecovery
		want     *ConfigNotice
	}{
		"success: absent note":     {reason: secret.ConfigReasonAbsent, want: &ConfigNotice{Message: "no `~/.claude.json` to update; Claude Code writes `oauthAccount` at its next start"}},
		"success: current silent":  {reason: secret.ConfigReasonAlreadyCurrent},
		"success: declined silent": {reason: secret.ConfigReasonDeclined},
		"error: forward recovery":  {reason: secret.ConfigReasonLockBusy, recovery: ConfigRecovery{ID: "acct", SameAgain: true}, want: &ConfigNotice{Warning: true, Message: "`~/.claude.json` was not updated (a Claude Code session held its config lock); running sessions keep showing the previous account — run the same `agentctl claude use --live acct` again to update it"}},
		"error: undo recovery":     {reason: secret.ConfigReasonProfileUnavailable, recovery: ConfigRecovery{ID: "acct", AfterMessage: true}, want: &ConfigNotice{Warning: true, Message: "`~/.claude.json` was not updated (the server could not be asked for that account's profile); running sessions keep showing the previous account — send one message in Claude Code, which refreshes the live credential, then run `agentctl claude use --live acct` to update it"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, ConfigNoticeFor(tt.reason, "/home/test/.claude.json", "/home/test", tt.recovery)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
	if got := ConfigShownPath("/home/test/file\n\x00\x7f", "/home/test"); got != `~/file\n\u{0}\u{7f}` {
		t.Fatalf("shown = %q", got)
	}
	if got := ConfigShownPath("/home/testing/file", "/home/test"); got != "/home/testing/file" {
		t.Fatalf("wrong path prefix %q", got)
	}
}
