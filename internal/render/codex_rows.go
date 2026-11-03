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
	"time"

	"github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/usage"
)

// CodexStatusReportV2 projects shown accounts in discovery order.
func CodexStatusReportV2(accounts []codex.Account, now time.Time, hidden int, raw bool) StatusReportV2 {
	report := NewStatusReportV2(now, hidden)
	if raw {
		report.Raw = &RawBodies{}
	}
	for i := range accounts {
		account := &accounts[i]
		row := JSONRowV2{
			Provider: "codex", ID: account.ID,
			Identity: JSONIdentityV2{UserID: account.UserID, AccountID: account.AccountID, PlanType: account.Plan, Email: account.Email},
			Kind:     account.Kind.Name(), State: account.State.Name(), StateLabel: account.State.Label(),
			LockState: account.LockState, Note: account.Note, Windows: CodexWindowsV2(account.Usage),
			Credits: UnavailableCreditsV2(),
		}
		if account.Usage != nil {
			row.Credits = CodexCreditsV2(account.Usage.Credits)
			if raw && account.Usage.Raw != nil {
				report.Raw.Add(account.ID, account.Usage.Raw)
			}
		}
		if reset := account.NextReset(); !reset.IsZero() {
			row.NextReset = new(FormatInstant(reset))
		}
		if window := account.Window(usage.WindowSession); window != nil && !window.ResetsAt.IsZero() {
			row.SessionReset = new(FormatInstant(window.ResetsAt))
		}
		if window := account.Window(usage.WindowWeeklyAll); window != nil && !window.ResetsAt.IsZero() {
			row.WeeklyReset = new(FormatInstant(window.ResetsAt))
		}
		report.Rows = append(report.Rows, row)
	}
	return report
}

// CodexTableReport projects accounts directly into the Codex table's rows.
func CodexTableReport(accounts []codex.Account, now time.Time, zone *time.Location, all bool) CodexReport {
	report := CodexReport{Now: now, Zone: zone, ShowAll: all, Rows: make([]CodexTableRow, 0, len(accounts))}
	for _, account := range accounts {
		row := CodexTableRow{Account: account.Account(), Kind: account.Kind.Name(), Session: account.Window(usage.WindowSession), Weekly: account.Window(usage.WindowWeeklyAll), Credits: account.CreditsCell(), State: account.StateCell(), VisibleByDefault: account.Visible}
		if account.Plan != nil {
			row.Plan = *account.Plan
		}
		session, weekly := false, false
		if account.Usage != nil {
			for _, entry := range account.Usage.Windows {
				window := entry.Window
				switch window.Kind.Class {
				case usage.WindowSession:
					if !session {
						session = true
						continue
					}
				case usage.WindowWeeklyAll:
					if !weekly {
						weekly = true
						continue
					}
				}
				row.Extra = append(row.Extra, window)
			}
		}
		report.Rows = append(report.Rows, row)
	}
	return report
}
