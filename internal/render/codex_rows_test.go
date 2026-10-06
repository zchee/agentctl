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

package render_test

import (
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/testutil"
)

func codexProjectionFixture(t *testing.T) ([]codex.Account, time.Time) {
	t.Helper()
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	snapshot, err := codex.Normalize([]byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":21.6,"limit_window_seconds":18000,"reset_at":1789578000},"secondary_window":{"used_percent":35.2,"limit_window_seconds":604800,"reset_at":1789999200}},"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","metered_feature":"codex_bengalfox","rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":18000,"reset_at":1789578000}}}],"credits":{"has_credits":true,"unlimited":false,"balance":"12.34"}}`), now, true)
	if err != nil {
		t.Fatal(err)
	}
	return []codex.Account{
		{Index: 0, ID: "user-0000", UserID: "user-0000", AccountID: "acct-0000", Email: new("owner@example.com"), Plan: new("plus"), Kind: codex.RowOwned, State: codex.State{Kind: codex.StateOK}, LockState: "none", Usage: snapshot, Visible: true},
		{Index: 1, ID: "user-0001", UserID: "user-0001", AccountID: "acct-0001", Kind: codex.RowLive, State: codex.State{Kind: codex.StateNoUsageSource, Mode: "apikey"}, LockState: "none", Visible: true},
	}, now
}

func TestCodexAccountProjection(t *testing.T) {
	tests := map[string]struct{ raw bool }{"success: redacted rows": {}, "success: scrubbed raw body": {raw: true}}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			accounts, now := codexProjectionFixture(t)
			report := render.CodexStatusReportV2(accounts, now, 3, test.raw)
			body, err := render.MarshalStatusReportV2(&report)
			if err != nil {
				t.Fatal(err)
			}
			testutil.ValidateSchema(t, "status.v2.json", body)
			if diff := gocmp.Diff([]string{"user-0000", "user-0001"}, []string{report.Rows[0].ID, report.Rows[1].ID}); diff != "" {
				t.Fatalf("row order (-want +got):\n%s", diff)
			}
			first := report.Rows[0]
			if first.Provider != "codex" || first.Identity.AccountID != "acct-0000" || first.Credits.Balance == nil || *first.Credits.Balance != "12.34" || len(first.Windows) != 3 || first.SessionReset == nil || first.WeeklyReset == nil || report.Hidden != 3 {
				t.Fatalf("lost provider fields: %+v", report)
			}
			if strings.Contains(string(body), `"raw"`) != test.raw {
				t.Fatalf("raw inclusion differs: %s", body)
			}
		})
	}
	accounts, now := codexProjectionFixture(t)
	hidden := codex.Account{ID: "user-0002", Email: new("sibling@example.com"), State: codex.State{Kind: codex.StateStaleSibling}, Visible: false}
	accounts = append(accounts, hidden)
	report := render.CodexTableReport(accounts, now, time.FixedZone("JST", 9*60*60), false)
	rendered := render.RenderCodex(&report)
	testutil.GoldenTrimmed(t, "provider__codex__account__tests__codex_table", []byte(rendered))
}
