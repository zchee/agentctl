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

// Package render turns a finished account pass into something a person
// reads: the status tables, the reset cells, and the row vocabulary the
// watch display shares with the status command.
package render

import (
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/zchee/agentctl/internal/usage"
)

// A reset cell carries a countdown and the absolute local time it names.
// A countdown alone cannot be planned around: it says how long, never
// when. "2d6h" does not name an afternoon, and a weekly window that rolls
// over on Sunday at two in the afternoon is a fact worth stating as a
// time of day. The countdown leads, because it is the figure a reader
// scans the column for, and the absolute time follows in parentheses as
// the answer to "which one is that".
//
// The absolute half takes one of three shapes:
//
//	1h12m (4:15 PM)          this afternoon
//	2d6h (Sun 02:00 PM)      another day, less than a week away
//	7d5h (Sep 16 02:00 PM)   a week or more away
//
// A bare clock time is unambiguous only on the day it is read, so a reset
// on another local day carries its weekday. A weekday, in turn, is
// unambiguous only inside one week: "Sun" seven days out names the same
// word as "Sun" tomorrow, so from seven days the date replaces it. The
// cut is the calendar ambiguity, not a display preference.
//
// The hour is zero-padded once a weekday or a date joins it — "Sun 01:59
// PM", "Sep 16 02:00 PM" — so that every absolute time inside one shape
// is the same width, which is what lets justifyReset give every row in a
// column the same right edge without measuring each one specially. The
// bare today shape keeps the un-padded hour ("7:09 AM"): it never shares
// a column position with a weekday or date form's clock. The day of
// month stays un-padded in every shape ("Sep 6", not "Sep 06") — only
// the hour needs a constant width to line a column up.
//
// Every calendar comparison is made on the zoned value, not on the UTC
// instant: 23:30 UTC on a Saturday is 08:30 Sunday in +09:00, and a
// weekday taken from the instant would print the wrong day for half the
// world. The location arrives as an argument rather than being read from
// the host, so a test can pin one.

// weekOrMore is the distance at or beyond which a weekday names two days.
const weekOrMore = 7 * 24 * time.Hour

// timeOnlyLayout renders a reset on the same local calendar day as now:
// "4:15 PM".
const timeOnlyLayout = "3:04 PM"

// withWeekdayLayout renders a reset on another local day, less than a
// week out: "Sun 02:00 PM". The hour is zero-padded so every absolute
// time in this shape is the same width.
const withWeekdayLayout = "Mon 03:04 PM"

// withDateLayout renders a reset a week or more out, where the weekday no
// longer names it: "Sep 16 02:00 PM". The day of month stays un-padded;
// only the hour needs the constant width.
const withDateLayout = "Jan 2 03:04 PM"

// resetParts returns the countdown and the absolute local time for one
// reset, un-joined. A caller building a whole column needs both halves of
// every row before it can compute the column's width, so it cannot go
// through ResetCell, which already joins the two into one string;
// justifyReset is the other half, taking what this returns and the width
// the caller computed.
func resetParts(now, resetsAt time.Time, loc *time.Location) (countdown, absolute string) {
	return usage.RenderCountdown(now, resetsAt), absoluteLocal(now, resetsAt, loc)
}

// justifyReset lays countdown flush left and "(absolute)" flush right
// within width, the gap between them filled with spaces.
//
// width is the column's own width: the widest natural cell length among
// every row sharing the column — an em dash cell or a "now (…)" cell
// counts toward that maximum the same as any other row, which is why
// this takes plain strings rather than anything that knows about
// "missing" — computed by the caller, since only the caller sees every
// row in the column at once. A width narrower than this cell's own
// natural length still renders; the gap is never less than one space.
func justifyReset(countdown, absolute string, width int) string {
	paren := "(" + absolute + ")"
	pad := max(width-lipgloss.Width(countdown)-lipgloss.Width(paren), 1)
	return countdown + strings.Repeat(" ", pad) + paren
}

// ResetCell returns the whole cell for one window's reset, with no
// column-wide padding: the countdown, then the absolute local time in
// parentheses.
//
// Kept for a caller with no column to justify against — a single cell
// has no other row to line its closing paren up with, so the padding
// justifyReset exists for would have nothing to compute against. The
// table's own reset columns go through resetParts and justifyReset
// instead, once per column rather than once per cell, so every row in a
// column shares the same width.
//
// The countdown comes from the usage model — the same function the
// watch detail line uses, so the two presentations cannot drift apart —
// and a reset that has already passed renders as "now", which is what a
// window rolling over between the fetch and the render looks like.
func ResetCell(now, resetsAt time.Time, loc *time.Location) string {
	countdown, absolute := resetParts(now, resetsAt, loc)
	return countdown + " (" + absolute + ")"
}

// absoluteLocal renders the absolute half of the cell: a 12-hour local
// clock time, prefixed by as much of the date as it takes to name the
// day unambiguously.
//
// The minute always carries two digits, and the hour carries a leading
// zero once a weekday or a date joins it; on its own — the same local
// day as now — the hour carries none, so "2:05 PM" and "12:00 AM" read
// as clock times rather than as fields.
func absoluteLocal(now, at time.Time, loc *time.Location) string {
	atLocal := at.In(loc)
	return atLocal.Format(shape(now.In(loc), atLocal))
}

// shape picks which of the three layouts names atLocal from where
// nowLocal stands.
func shape(nowLocal, atLocal time.Time) string {
	ny, nm, nd := nowLocal.Date()
	ay, am, ad := atLocal.Date()
	if ny == ay && nm == am && nd == ad {
		return timeOnlyLayout
	}
	// Either direction, because a reset can be in the past: a window that
	// rolled over eight days ago is as badly served by a weekday as one
	// that rolls over eight days from now. Sub saturates at the duration
	// range's edges, and two instants whose difference does not fit are
	// certainly more than a week apart.
	if atLocal.Sub(nowLocal).Abs() >= weekOrMore {
		return withDateLayout
	}
	return withWeekdayLayout
}
