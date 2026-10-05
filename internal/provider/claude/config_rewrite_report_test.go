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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/secret"
)

func TestConfigReportRendering(t *testing.T) {
	r := ConfigNotAttempted(secret.ConfigReasonProfileUnavailable)
	r.Account = &secret.IncomingIdentity{AccountUUID: "private-id"}
	got, err := json.Marshal(r.JSON(), json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"outcome":"not_attempted","reason":"profile_unavailable","backup":null,"hold_ms":null,"budget_ms":1200}`
	if diff := gocmp.Diff(want, string(got)); diff != "" {
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

func TestConfigReportJSONReferenceOrder(t *testing.T) {
	// Reference binary config members, with their nesting indentation removed.
	tests := map[string]struct {
		report ConfigReport
		want   string
	}{
		"success: applied config retains null reason": {
			report: ConfigReport{Outcome: secret.ConfigApplied, Backup: new(".claude.json.backup.1791235855951"), HoldMS: new(uint64(22))},
			want: `{
  "outcome": "applied",
  "reason": null,
  "backup": ".claude.json.backup.1791235855951",
  "hold_ms": 22,
  "budget_ms": 1200
}`,
		},
		"success: refused config retains null backup and hold": {
			report: ConfigReport{Outcome: secret.ConfigRefused, Reason: new(secret.ConfigReasonNotReproducible)},
			want: `{
  "outcome": "refused",
  "reason": "not_reproducible",
  "backup": null,
  "hold_ms": null,
  "budget_ms": 1200
}`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			test.report.Account = &secret.IncomingIdentity{AccountUUID: "private-account", OrganizationUUID: new("private-org")}
			test.report.FromSHA8 = new("private-from")
			test.report.ToSHA8 = new("private-to")
			got, err := json.Marshal(test.report.JSON(), json.Deterministic(true), jsontext.WithIndent("  "))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, string(got)); diff != "" {
				t.Fatalf("reference config JSON differs (-want +got):\n%s", diff)
			}
		})
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
