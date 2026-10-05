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

package codex_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/tui"
	"github.com/zchee/agentctl/internal/usage"
)

type codexFrameRow struct{ codex.Account }

func (a codexFrameRow) Gauges() []render.Gauge {
	var result []render.Gauge
	for _, g := range a.Account.Gauges() {
		result = append(result, render.Gauge{Label: g.Label, Percent: g.Percent})
	}
	return result
}

func accountRows(t *testing.T) []codex.Account {
	t.Helper()
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":21.6,"limit_window_seconds":18000,"reset_at":1789578000},"secondary_window":{"used_percent":35.2,"limit_window_seconds":604800,"reset_at":1789999200}},"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","metered_feature":"codex_bengalfox","rate_limit":{"primary_window":{"used_percent":3.0,"limit_window_seconds":18000,"reset_at":1789578000}}}],"credits":{"has_credits":true,"unlimited":false,"balance":"12.34"}}`)
	u, err := codex.Normalize(body, now, true)
	if err != nil {
		t.Fatal(err)
	}
	return []codex.Account{{Index: 0, ID: "user-0000", UserID: "user-0000", AccountID: "acct-0000", Email: new("owner@example.com"), Plan: new("plus"), Kind: codex.RowOwned, State: codex.State{Kind: codex.StateOK}, LockState: "none", Usage: u, Visible: true}, {Index: 1, ID: "user-0001", UserID: "user-0001", AccountID: "acct-0001", Kind: codex.RowLive, State: codex.State{Kind: codex.StateNoUsageSource, Mode: "apikey"}, Visible: true}}
}

func TestAccountGoldenBytes(t *testing.T) {
	tests := map[string]struct {
		golden string
		render func(*testing.T) string
	}{"success: table": {"provider__codex__account__tests__codex_table", func(t *testing.T) string {
		rows := accountRows(t)
		rows = append(rows, codex.Account{Email: new("sibling@example.com"), State: codex.State{Kind: codex.StateStaleSibling}})
		report := render.CodexReport{Now: time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC), Zone: time.FixedZone("UTC+9", 9*3600)}
		for _, account := range rows {
			r := account.ToTableRow()
			report.Rows = append(report.Rows, render.CodexTableRow{Account: r.Account, Plan: r.Plan, Kind: r.Kind, Session: r.Session, Weekly: r.Weekly, Extra: r.Extra, Credits: r.Credits, State: r.State, VisibleByDefault: r.VisibleByDefault})
		}
		return render.RenderCodex(&report) + "\n"
	}}, "success: watch": {"provider__codex__account__tests__codex_watch_two_rows", func(t *testing.T) string {
		now := time.Date(2026, time.September, 16, 12, 0, 30, 0, time.UTC)
		m := tui.New[codexFrameRow](now, codex.Account{}.WatchTitle(), codex.Account{}.HiddenHint())
		m.Width, m.Height = 84, 16
		var rows tui.Rows[codexFrameRow]
		for _, row := range accountRows(t) {
			rows = append(rows, codexFrameRow{row})
		}
		m.Reduce(rows)
		m.Reduce(tui.PassFinished{At: now.Add(-30 * time.Second), Next: now.Add(4*time.Minute + 30*time.Second)})
		return testutil.FrameDump(m.View().Content, 84, 16)
	}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want := string(testutil.ReadGolden(t, tt.golden))
			if name == "success: watch" {
				var normalized []string
				for line := range strings.SplitSeq(strings.TrimSpace(want), "\n") {
					line = strings.TrimSuffix(strings.TrimPrefix(line, `"`), `"`)
					if strings.Contains(line, "agctl ") {
						line = lipgloss.NewStyle().MaxWidth(84).MaxHeight(1).Render(strings.ReplaceAll(line, "agctl ", "agentctl "))
						line += strings.Repeat(" ", max(0, 84-lipgloss.Width(line)))
					}
					normalized = append(normalized, `"`+line+`"`)
				}
				want = strings.Join(normalized, "\n") + "\n"
			}
			if diff := gocmp.Diff(want, tt.render(t)); diff != "" {
				t.Fatalf("golden bytes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAccountProjection(t *testing.T) {
	a := accountRows(t)[0]
	r := a.ToTableRow()
	if len(r.Extra) != 1 || r.Extra[0].Label() != "GPT-5.3-Codex-Spark:primary (unknown kind)" {
		t.Fatalf("extra=%+v", r.Extra)
	}
	if diff := gocmp.Diff([]codex.Gauge{{Label: "5h", Percent: 21}, {Label: "weekly", Percent: 35}}, a.Gauges()); diff != "" {
		t.Fatal(diff)
	}
	if a.BlockHeight() != 5 {
		t.Fatal("block height")
	}
	if a.Window(usage.WindowSession) == nil || a.Window(usage.WindowWeeklyAll) == nil {
		t.Fatal("fixed window missing")
	}
	tests := map[string]struct {
		credits codex.Credits
		want    string
	}{"success: balance": {codex.Credits{Available: true, Balance: new("12.34")}, "12.34"}, "success: unlimited": {codex.Credits{Available: true, Unlimited: true}, "Unlimited"}, "success: unknown balance": {codex.Credits{Available: true}, "—"}, "success: unavailable": {codex.Credits{}, "n/a"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := accountRows(t)[0]
			a.Usage.Credits = tt.credits
			if diff := gocmp.Diff(tt.want, a.CreditsCell()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
	a.Email = new("")
	a.Note = new("auto (file in effect)")
	if a.Account() != "user-0000" || a.StateCell() != "ok (auto (file in effect))" {
		t.Fatalf("account=%s state=%s", a.Account(), a.StateCell())
	}
}

func TestAccountStates(t *testing.T) {
	tests := map[string]struct {
		state   codex.State
		neutral bool
		badge   string
	}{"success: ok": {codex.State{Kind: codex.StateOK}, true, ""}, "success: no source": {codex.State{Kind: codex.StateNoUsageSource, Mode: "apikey"}, true, ""}, "success: forgotten": {codex.State{Kind: codex.StateForgotten}, true, ""}, "success: expired": {codex.State{Kind: codex.StateExpired, Reason: "run agentctl codex login"}, false, "expired"}, "success: torn": {codex.State{Kind: codex.StateTornRead}, false, "stale"}, "success: stale": {codex.State{Kind: codex.StateStale}, false, "stale"}, "success: busy": {codex.State{Kind: codex.StateBusy}, false, "busy"}, "success: daemon": {codex.State{Kind: codex.StateSessionDetected, Evidence: "pid alive"}, false, "codex-detected"}, "success: adopted dead": {codex.State{Kind: codex.StateAdoptedDead}, false, "needs login"}, "success: refresh unknown": {codex.State{Kind: codex.StateRefreshUnknown, Class: "rate_limited"}, false, "refresh unknown"}}
	seen := make(map[string]bool)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			token := tt.state.Name()
			if seen[token] || strings.Trim(token, "abcdefghijklmnopqrstuvwxyz_") != "" {
				t.Fatalf("invalid name %q", token)
			}
			seen[token] = true
			if tt.state.Label() == "" || tt.state.IsExitNeutral() != tt.neutral || codex.Badge(tt.state) != tt.badge {
				t.Fatalf("state=%+v", tt.state)
			}
		})
	}
	if (codex.State{Kind: codex.StateTornRead}).Label() != fmt.Sprintf("%s was being rewritten; retrying next pass", codex.ShownName()) {
		t.Fatal("torn label")
	}
}
