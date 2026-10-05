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

package render

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/usage"
)

// reportRow is a healthy row with one window of each named kind, so every
// member of the document is exercised at least once.
func reportRow(t *testing.T) JSONRow {
	t.Helper()
	at := time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC)
	snapshot := &usage.UsageSnapshot{
		FetchedAt: at,
		Windows: []usage.LimitWindow{
			{Kind: usage.WindowKind{Class: usage.WindowSession}, Percent: new(21.5), PercentFloor: new(21), ResetsAt: at.Add(2 * time.Hour), IsActive: true},
			{Kind: usage.WindowKind{Class: usage.WindowWeeklyAll}, Percent: new(35.0), PercentFloor: new(35), ResetsAt: at.Add(48 * time.Hour)},
			{Kind: usage.WindowKind{Class: usage.WindowWeeklyScoped, Name: "Fable"}, Percent: new(56.9), PercentFloor: new(56)},
		},
		Credits: usage.CreditsState{
			Class: usage.CreditsOn,
			Credits: usage.Credits{
				Used:    &usage.Money{AmountMinor: 123, Currency: "USD", Exponent: 2},
				Limit:   &usage.Money{AmountMinor: 10_000, Currency: "USD", Exponent: 2},
				Percent: new(1),
			},
		},
	}
	return JSONRow{
		ID:               "11111111-2222-3333-4444-555555555555",
		AccountUUID:      "11111111-2222-3333-4444-555555555555",
		OrganizationUUID: "66666666-7777-8888-9999-000000000000",
		Email:            new("owner@example.com"),
		Kind:             "owned",
		Source:           "file",
		State:            "ok",
		StateLabel:       "ok",
		LockState:        "none",
		Windows:          WindowsOf(snapshot),
		Credits:          CreditsOf(snapshot),
		NextReset:        NextResetOf(snapshot),
		SessionReset:     SessionResetOf(snapshot),
		WeeklyReset:      WeeklyResetOf(snapshot),
	}
}

func TestStatusReportValidatesAgainstThePublishedSchema(t *testing.T) {
	tests := map[string]struct {
		build func(t *testing.T) StatusReport
	}{
		"success: a healthy row with windows and credits": {
			build: func(t *testing.T) StatusReport {
				t.Helper()
				report := NewStatusReport(time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC), 0)
				report.Rows = append(report.Rows, reportRow(t))
				return report
			},
		},
		"success: an empty report with hidden rows counted": {
			build: func(t *testing.T) StatusReport {
				t.Helper()
				return NewStatusReport(time.Date(2026, 9, 8, 1, 20, 5, 123_000_000, time.UTC), 2)
			},
		},
		"success: a degraded row with no snapshot keeps every member": {
			build: func(t *testing.T) StatusReport {
				t.Helper()
				report := NewStatusReport(time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC), 0)
				report.Rows = append(report.Rows, JSONRow{
					ID:               "live",
					OrganizationUUID: "_unknown-org",
					Kind:             "live",
					Source:           "none",
					State:            "keychain_locked",
					StateLabel:       "keychain locked",
					LockState:        "none",
					Windows:          WindowsOf(nil),
					Credits:          CreditsOf(nil),
					Note:             new("keychain service `Claude Code-credentials`"),
					SameIdentityAs:   new(SameIdentityLive),
				})
				return report
			},
		},
		"success: a raw member keyed by row id": {
			build: func(t *testing.T) StatusReport {
				t.Helper()
				report := NewStatusReport(time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC), 0)
				report.Rows = append(report.Rows, reportRow(t))
				raw := &RawBodies{}
				raw.Add(report.Rows[0].ID, jsontext.Value(`{"five_hour":{"utilization":21.5}}`))
				report.Raw = raw
				return report
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			report := tt.build(t)
			document, err := MarshalStatusReport(&report)
			if err != nil {
				t.Fatalf("marshal the report: %v", err)
			}
			testutil.ValidateSchema(t, "status.v1.json", document)
		})
	}
}

func TestStatusReportMemberShapes(t *testing.T) {
	report := NewStatusReport(time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC), 1)
	report.Rows = append(report.Rows, JSONRow{
		ID:               "live",
		OrganizationUUID: "_unknown-org",
		Kind:             "live",
		Source:           "none",
		State:            "needs_login",
		StateLabel:       "needs login",
		LockState:        "none",
		Credits:          CreditsOf(nil),
	})
	document, err := MarshalStatusReport(&report)
	if err != nil {
		t.Fatalf("marshal the report: %v", err)
	}
	text := string(document)

	for _, want := range []string{
		`"generated_at": "2026-09-08T01:20:05Z"`,
		`"windows": []`,
		`"email": null`,
		`"same_identity_as": null`,
		`"occupied_by": null`,
		`"state": "unavailable"`,
		`"scope": "organization"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the document is missing %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"raw"`) {
		t.Errorf("a report without bodies must omit the raw member:\n%s", text)
	}
	if strings.HasSuffix(text, "\n") {
		t.Error("the marshalled document carries no trailing newline; the printer adds it")
	}
}

func TestStatusReportFieldOrderIsTheSchemaOrder(t *testing.T) {
	report := NewStatusReport(time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC), 0)
	report.Rows = append(report.Rows, reportRow(t))
	document, err := MarshalStatusReport(&report)
	if err != nil {
		t.Fatalf("marshal the report: %v", err)
	}
	text := string(document)

	// The spellings are an interface: consumers diff documents, so the
	// member order must not drift between builds.
	order := []string{`"version"`, `"generated_at"`, `"rows"`, `"id"`, `"account_uuid"`, `"organization_uuid"`, `"email"`, `"org_name"`, `"kind"`, `"source"`, `"state"`, `"state_label"`, `"lock_state"`, `"windows"`, `"credits"`, `"next_reset"`, `"session_reset"`, `"weekly_reset"`, `"note"`, `"same_identity_as"`, `"occupied_by"`, `"hidden"`}
	last := -1
	for _, member := range order {
		at := strings.Index(text, member)
		if at < 0 {
			t.Fatalf("the document is missing %s:\n%s", member, text)
		}
		if at < last {
			t.Fatalf("%s is out of order:\n%s", member, text)
		}
		last = at
	}
}

func TestRawBodiesKeepInsertionOrder(t *testing.T) {
	raw := &RawBodies{}
	raw.Add("zeta", jsontext.Value(`{"z":1}`))
	raw.Add("alpha", jsontext.Value(`{"a":2}`))
	report := NewStatusReport(time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC), 0)
	report.Raw = raw

	document, err := MarshalStatusReport(&report)
	if err != nil {
		t.Fatalf("marshal the report: %v", err)
	}
	text := string(document)
	if strings.Index(text, `"zeta"`) > strings.Index(text, `"alpha"`) {
		t.Fatalf("the raw bodies were reordered:\n%s", text)
	}
}

func TestWindowConversions(t *testing.T) {
	at := time.Date(2026, 9, 8, 1, 20, 5, 0, time.UTC)
	tests := map[string]struct {
		window usage.LimitWindow
		want   JSONWindow
	}{
		"success: a session window carries its reset and activity": {
			window: usage.LimitWindow{Kind: usage.WindowKind{Class: usage.WindowSession}, Percent: new(21.5), PercentFloor: new(21), ResetsAt: at, IsActive: true},
			want:   JSONWindow{Kind: "session", Label: "session", Percent: new(21.5), PercentFloor: new(21), ResetsAt: new("2026-09-08T01:20:05Z"), IsActive: true},
		},
		"success: an unknown kind keeps the server's spelling in its label": {
			window: usage.LimitWindow{Kind: usage.WindowKind{Class: usage.WindowUnknown, Name: "lunar_cycle"}},
			want:   JSONWindow{Kind: "unknown", Label: "lunar_cycle (unknown kind)"},
		},
		"success: a scoped window names its scope": {
			window: usage.LimitWindow{Kind: usage.WindowKind{Class: usage.WindowWeeklyScoped, Name: "Fable"}},
			want:   JSONWindow{Kind: "weekly_scoped", Label: "Fable (weekly)"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := windowJSON(&tt.window)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("window mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreditsConversions(t *testing.T) {
	tests := map[string]struct {
		snapshot *usage.UsageSnapshot
		want     JSONCredits
	}{
		"success: no snapshot is unavailable": {
			snapshot: nil,
			want:     JSONCredits{State: "unavailable", Scope: CreditsScope},
		},
		"success: switched off keeps the server's reason": {
			snapshot: &usage.UsageSnapshot{Credits: usage.CreditsState{Class: usage.CreditsOff, DisabledReason: "monthly limit reached"}},
			want:     JSONCredits{State: "off", DisabledReason: new("monthly limit reached"), Scope: CreditsScope},
		},
		"success: an uncapped account denominates from the used figure": {
			snapshot: &usage.UsageSnapshot{Credits: usage.CreditsState{Class: usage.CreditsOn, Credits: usage.Credits{Used: &usage.Money{AmountMinor: 50, Currency: "USD", Exponent: 2}}}},
			want:     JSONCredits{State: "on", UsedMinor: new(int64(50)), Currency: new("USD"), Exponent: new(2), Scope: CreditsScope},
		},
		"success: a limit with no spend denominates from the limit": {
			snapshot: &usage.UsageSnapshot{Credits: usage.CreditsState{Class: usage.CreditsOn, Credits: usage.Credits{Limit: &usage.Money{AmountMinor: 10_000, Currency: "EUR", Exponent: 2}}}},
			want:     JSONCredits{State: "on", LimitMinor: new(int64(10_000)), Currency: new("EUR"), Exponent: new(2), Scope: CreditsScope},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := CreditsOf(tt.snapshot)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("credits mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
