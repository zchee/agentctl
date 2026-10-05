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
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/usage"
)

// What the watch display needs from a row, whoever produced it.
//
// TuiRow is the whole of it: the questions below and two display
// strings. Everything the watch frame draws — the gauges, the line
// under them, the block's title and height, whether the row is shown at
// all — is an answer to one of them, so the frame code can be written
// once and handed any provider's row without knowing which it has. The
// helper functions beside the interface carry the shared answer bodies,
// so two providers cannot drift apart on the shapes they both use.

// SelectedMarker is the marker in front of the selected account.
const SelectedMarker = "▸"

// blockChrome is the lines an account block spends on something other
// than a gauge: the two borders and the detail line.
const blockChrome = 3

// Gauge is one labelled percentage bar of a watch block.
type Gauge struct {
	// Label names the window or figure the bar measures.
	Label string
	// Percent is the floored percentage the bar shows.
	Percent int
}

// TuiRow is one row of a watch display.
type TuiRow interface {
	// WatchTitle is what the header calls this display, before the
	// account count. A command name, not decoration: a display that
	// inherited another provider's title would tell the user they are
	// watching the other provider's accounts.
	WatchTitle() string
	// HiddenHint is the command the footer names for the rows this
	// display hides, a command name for the same reason.
	HiddenHint() string
	// Gauges returns the gauges this row earns, in display order. A
	// window the response did not describe earns no gauge rather than
	// one at zero: a bar at zero is a claim that nothing has been used,
	// which is a different statement from "the server did not say".
	Gauges() []Gauge
	// DetailLine is the line under the gauges: badge, state, note, next
	// reset.
	DetailLine(now time.Time) string
	// AccountTitle is the block's title: who this is, and whether it is
	// selected.
	AccountTitle(selected bool) string
	// BlockHeight is how tall the block is, borders included.
	BlockHeight() int
	// VisibleByDefault reports whether the row appears without the flag
	// that shows every row.
	VisibleByDefault() bool
	// StateToken is the row's state as one stable, machine-readable
	// token: the neutral half of what DetailLine renders in prose. A
	// caller that wants to decide something about a row — a colour, a
	// count, an exit status — reads this rather than parsing the
	// sentence.
	StateToken() string
}

// UsageGauges returns the gauges a snapshot earns, in display order:
// the five-hour window, the weekly window, the headline scoped window,
// and credits when they are on and carry a utilisation figure. A nil
// snapshot earns none. A provider whose rows show fewer bars picks its
// windows from the snapshot directly instead of filtering these.
func UsageGauges(snapshot *usage.UsageSnapshot) []Gauge {
	if snapshot == nil {
		return nil
	}
	var gauges []Gauge
	if window := snapshot.Window(sessionKind); window != nil && window.PercentFloor != nil {
		gauges = append(gauges, Gauge{Label: "5h", Percent: *window.PercentFloor})
	}
	if window := snapshot.Window(weeklyAllKind); window != nil && window.PercentFloor != nil {
		gauges = append(gauges, Gauge{Label: "weekly", Percent: *window.PercentFloor})
	}
	if window := snapshot.ScopedWindow(HeadlineScope); window != nil && window.PercentFloor != nil {
		gauges = append(gauges, Gauge{Label: HeadlineScope, Percent: *window.PercentFloor})
	}
	if snapshot.Credits.Class == usage.CreditsOn && snapshot.Credits.Credits.Percent != nil {
		gauges = append(gauges, Gauge{Label: "credits", Percent: *snapshot.Credits.Credits.Percent})
	}
	return gauges
}

// DetailLine joins the pieces of the line under a block's gauges with
// middle dots: the badge in brackets when the state has one, the state
// label, the note in parentheses when there is one, and the soonest
// reset as a countdown when any window carries one. The countdown comes
// from the same function the table's reset cells use, so the two
// presentations cannot drift apart.
func DetailLine(now time.Time, badge, state, note string, nextReset time.Time) string {
	parts := make([]string, 0, 4)
	if badge != "" {
		parts = append(parts, "["+badge+"]")
	}
	parts = append(parts, state)
	if note != "" {
		parts = append(parts, "("+note+")")
	}
	if !nextReset.IsZero() {
		parts = append(parts, "next reset in "+usage.RenderCountdown(now, nextReset))
	}
	return strings.Join(parts, " · ")
}

// AccountTitle renders a block's title: the selection marker or its
// placeholder space, the account, and two identity cells — the
// organization and plan for one provider, the credential kind and plan
// for another — with the em dash standing in for an empty one. The
// trailing space keeps the last word off the block's corner.
func AccountTitle(selected bool, account, middle, plan string) string {
	marker := " "
	if selected {
		marker = SelectedMarker
	}
	return marker + " " + account + " · " + orEmpty(middle) + " · " + orEmpty(plan) + " "
}

// BlockHeight is how tall a block drawing the given number of gauges
// is: the two borders and the detail line, then one line per gauge.
func BlockHeight(gauges int) int {
	return blockChrome + gauges
}
