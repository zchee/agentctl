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
	"fmt"
	"io"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"

	"github.com/zchee/agentctl/internal/usage"
)

// The status table: a fixed column list per provider, one row per
// account with its continuation rows under it, then a footer counting
// the hidden rows.
//
// An account can have windows that no column names — a weekly scope
// other than the headline one, or a kind this build has never seen.
// Dropping them would hide a limit the user is subject to, and widening
// the table per account would make two accounts unalignable. So each
// gets a continuation row directly under its account: the first cell
// names the window, the percentage sits in the Weekly column and the
// reset in Weekly reset — 5h reset stays blank, because such a window
// is never the five-hour one. The label, not the column, is what says
// what the number means — which is why an unknown kind renders its kind
// string verbatim rather than being quietly filed as weekly.
//
// A missing figure is an em dash, never "0%" and never a blank. "0%" is
// a claim about the account; a blank is ambiguous between "nothing to
// say" and "something went wrong". The em dash says the figure is
// unavailable, which is the only true statement available.

// EmptyCell is what an unavailable figure looks like.
const EmptyCell = "—"

// ContinuationMarker is what a continuation row starts with.
const ContinuationMarker = "↳"

// claudeHeadings is the fixed Claude column list, in order.
var claudeHeadings = [10]string{
	"Account",
	"Org",
	"Plan",
	"5h",
	"Weekly",
	"Fable (weekly)",
	"Credits",
	"5h reset",
	"Weekly reset",
	"State",
}

// KindHeading is the heading of the column the identity view adds.
const KindHeading = "Kind"

// KindIndex is where that column goes: after Plan, with the other
// columns that say which account this is rather than what it is using.
const KindIndex = 3

// Where the two reset columns sit in a ten-cell record, before the
// identity view splices Kind in.
const (
	sessionResetIndex = 7
	weeklyResetIndex  = 8
)

// codexHeadings is the fixed Codex column list, in order. Nine rather
// than Claude's ten, and not a subset: a Codex account has no
// organization and no headline scoped window, and what it does have —
// which of the homes the credential came from — Claude only shows in
// the identity view. Two providers rendered through one column list
// would have meant an Org column that is always an em dash for half the
// rows, which is a column that says nothing.
var codexHeadings = [9]string{
	"Account",
	"Plan",
	"Kind",
	"5h",
	"Weekly",
	"Credits",
	"5h reset",
	"Weekly reset",
	"State",
}

// Where the two reset columns sit in a Codex record.
const (
	codexSessionResetIndex = 6
	codexWeeklyResetIndex  = 7
)

// Headings returns the Claude column list, with KindHeading spliced in
// at KindIndex when the identity view asked for it. A function rather
// than a second list so the fixed columns stay written down exactly
// once: a column added to the base list cannot be forgotten here.
func Headings(byIdentity bool) []string {
	headings := claudeHeadings[:]
	if !byIdentity {
		return headings
	}
	with := make([]string, 0, len(headings)+1)
	with = append(with, headings[:KindIndex]...)
	with = append(with, KindHeading)
	return append(with, headings[KindIndex:]...)
}

// CodexHeadings returns the Codex column list.
func CodexHeadings() []string {
	return codexHeadings[:]
}

// Footer returns the hidden-row footer.
func Footer(hidden int) string {
	noun := "entries"
	if hidden == 1 {
		noun = "entry"
	}
	return fmt.Sprintf("%d %s hidden (--all)", hidden, noun)
}

// Print writes rendered text followed by one newline, preserving right padding.
// The text must not already end in a newline. Any write error is returned.
func Print(w io.Writer, text string) error {
	_, err := fmt.Fprintln(w, text)
	return err
}

// Render renders a whole report: the table, then the hidden-row footer.
// The returned string carries no trailing newline; the printer adds it.
//
// A report with no shown rows still prints its headings and its footer,
// so a run whose only account is hidden explains itself rather than
// printing nothing.
func Render(report *Report) string {
	// Two passes, because a justified cell's padding depends on every
	// other row in its column (the countdown flush left, the absolute
	// time flush right), and that width is only known once every row has
	// been seen. The first pass builds each row's other cells directly
	// and sets the two reset cells aside as raw data; the second computes
	// each reset column's width and fills the justified text in before
	// any record reaches the table.
	var records [][]string
	var sessionColumn, weeklyColumn []resetSlot

	for _, row := range report.Shown() {
		snapshot := row.Usage
		sessionColumn = append(sessionColumn, resetSlotFor(windowOf(snapshot, sessionKind), report.Now, report.Zone))
		weeklyColumn = append(weeklyColumn, resetSlotFor(windowOf(snapshot, weeklyAllKind), report.Now, report.Zone))
		records = append(records, withKind(accountRecord(row), report, row.Kind))

		if snapshot == nil {
			continue
		}
		for _, window := range snapshot.ExtraWindows(HeadlineScope) {
			// Never the five-hour window, so that cell is left blank
			// rather than justified — see continuationRecord.
			sessionColumn = append(sessionColumn, resetSlot{})
			weeklyColumn = append(weeklyColumn, resetSlotFor(window, report.Now, report.Zone))
			records = append(records, withKind(continuationRecord(window), report, ""))
		}
	}

	sessionWidth := columnWidth(sessionColumn)
	weeklyWidth := columnWidth(weeklyColumn)
	sessionIndex := resetIndex(sessionResetIndex, report.ByIdentity)
	weeklyIndex := resetIndex(weeklyResetIndex, report.ByIdentity)
	for i := range records {
		records[i][sessionIndex] = sessionColumn[i].finalize(sessionWidth)
		records[i][weeklyIndex] = weeklyColumn[i].finalize(weeklyWidth)
	}

	out := renderTable(Headings(report.ByIdentity), records)
	if hidden := report.HiddenCount(); hidden > 0 {
		out += "\n" + Footer(hidden)
	}
	return out
}

// RenderCodex renders a Codex report: the Codex headings, one row per
// account with its continuation rows under it, then the hidden-row
// footer. The same two-pass layout as Render, so the two reset columns
// justify the same way in both tables.
func RenderCodex(report *CodexReport) string {
	var records [][]string
	var sessionColumn, weeklyColumn []resetSlot

	for i := range report.Rows {
		row := &report.Rows[i]
		if !report.ShowAll && !row.VisibleByDefault {
			continue
		}
		sessionColumn = append(sessionColumn, resetSlotFor(row.Session, report.Now, report.Zone))
		weeklyColumn = append(weeklyColumn, resetSlotFor(row.Weekly, report.Now, report.Zone))
		records = append(records, []string{
			row.Account,
			orEmpty(row.Plan),
			row.Kind,
			percentCell(row.Session),
			percentCell(row.Weekly),
			orEmpty(row.Credits),
			"",
			"",
			row.State,
		})
		for j := range row.Extra {
			window := &row.Extra[j]
			sessionColumn = append(sessionColumn, resetSlot{})
			weeklyColumn = append(weeklyColumn, resetSlotFor(window, report.Now, report.Zone))
			records = append(records, []string{
				"  " + ContinuationMarker + " " + window.Label(),
				"", "", "",
				percentCell(window),
				"", "", "", "",
			})
		}
	}

	sessionWidth := columnWidth(sessionColumn)
	weeklyWidth := columnWidth(weeklyColumn)
	for i := range records {
		records[i][codexSessionResetIndex] = sessionColumn[i].finalize(sessionWidth)
		records[i][codexWeeklyResetIndex] = weeklyColumn[i].finalize(weeklyWidth)
	}

	out := renderTable(CodexHeadings(), records)
	if hidden := report.HiddenCount(); hidden > 0 {
		out += "\n" + Footer(hidden)
	}
	return out
}

// Table renders headings and finished records in the shared table
// style, for a caller whose cells need no column-wide justification.
func Table(headings []string, records [][]string) string {
	return renderTable(headings, records)
}

// CodexAccountsLine renders one line of the Codex accounts listing,
// which is deliberately not a table: two-space separated id, kind and
// email, a dash for a missing email, and a forgotten marker.
func CodexAccountsLine(id, kind, email string, forgotten bool) string {
	if email == "" {
		email = "-"
	}
	line := id + "  " + kind + "  " + email
	if forgotten {
		line += "  (forgotten)"
	}
	return line
}

// The two kinds the fixed columns name.
var (
	sessionKind   = usage.WindowKind{Class: usage.WindowSession}
	weeklyAllKind = usage.WindowKind{Class: usage.WindowWeeklyAll}
)

// windowOf is the snapshot's window lookup, lifted over a row that has
// no numbers.
func windowOf(snapshot *usage.UsageSnapshot, kind usage.WindowKind) *usage.LimitWindow {
	if snapshot == nil {
		return nil
	}
	return snapshot.Window(kind)
}

// withKind returns a ten-cell record with kind spliced in at KindIndex
// when the identity view asked for it, and left out entirely otherwise.
// A continuation row's kind cell is blank for the same reason its Org
// and Plan cells are: the window belongs to the account named above it,
// and repeating the account's attributes on it would read as a second
// account.
func withKind(cells []string, report *Report, kind string) []string {
	if !report.ByIdentity {
		return cells
	}
	with := make([]string, 0, len(cells)+1)
	with = append(with, cells[:KindIndex]...)
	with = append(with, kind)
	return append(with, cells[KindIndex:]...)
}

// accountRecord builds one account's own row. The two reset cells are
// left blank here: Render's second pass fills them in once every row's
// slot has been collected and each column's width is known.
func accountRecord(row *StatusRow) []string {
	cells := []string{
		row.Account,
		orEmpty(row.Org),
		orEmpty(row.Plan),
		EmptyCell,
		EmptyCell,
		EmptyCell,
		EmptyCell,
		"",
		"",
		row.StateCell(),
	}
	if snapshot := row.Usage; snapshot != nil {
		cells[3] = percentCell(snapshot.Window(sessionKind))
		cells[4] = percentCell(snapshot.Window(weeklyAllKind))
		cells[5] = percentCell(snapshot.ScopedWindow(HeadlineScope))
		cells[6] = creditsCell(snapshot.Credits)
	}
	return cells
}

// continuationRecord builds the row of one window that has no column of
// its own. Its Weekly reset cell is left blank for the same reason as
// accountRecord's; its 5h reset cell is left blank for good, because
// such a window is never the five-hour one — not an em dash, which
// would claim a figure was unavailable.
func continuationRecord(window *usage.LimitWindow) []string {
	return []string{
		"  " + ContinuationMarker + " " + window.Label(),
		"", "", "",
		percentCell(window),
		"", "", "", "", "",
	}
}

// percentCell renders a floored percentage, or an em dash when the
// window is absent or carried no usable figure.
func percentCell(window *usage.LimitWindow) string {
	if window == nil || window.PercentFloor == nil {
		return EmptyCell
	}
	return fmt.Sprintf("%d%%", *window.PercentFloor)
}

// creditsCell renders the credits column. Four shapes, and the
// difference between the last two is the point: "off" means the account
// has credits and switched them off, "n/a" means the response said
// nothing about credits at all. Collapsing them would tell a user with
// credits enabled that they are disabled.
func creditsCell(credits usage.CreditsState) string {
	switch credits.Class {
	case usage.CreditsUnavailable:
		return "n/a"
	case usage.CreditsOff:
		return "off"
	default:
	}
	// Credits are switched on but the server sent no used figure.
	// Rendering "— / Unlimited" would put a ceiling next to a figure
	// that does not exist; the whole cell is unavailable.
	on := credits.Credits
	if on.Used == nil {
		return EmptyCell
	}
	limit := "Unlimited"
	if on.Limit != nil {
		limit = on.Limit.String()
	}
	if on.Percent == nil {
		return fmt.Sprintf("%s / %s", on.Used, limit)
	}
	return fmt.Sprintf("%s / %s (%d%%)", on.Used, limit, *on.Percent)
}

// resetSlot is one reset column's cell before its column's width is
// known: either a countdown/absolute pair to justify, or text that is
// already final and must not be touched by justification.
//
// An em dash covers both ways of a real window having nothing to say —
// the response described no such window, or described one with no reset
// — because the reader's question is about the figure, not about which
// of the two happened. A final slot also carries the blank a
// continuation row's 5h reset cell always is. The zero value is the
// final blank.
type resetSlot struct {
	pair                bool
	countdown, absolute string
	text                string
}

// naturalWidth is this slot's own contribution to its column's width. A
// pair's is its natural minimum — the countdown, one space, and the
// parenthesised absolute time — and a final slot's is simply its
// length: an em dash or a blank cell counts toward the column's width
// the same as any other row, which in practice never matters, since
// neither is ever the widest cell in a column that also holds a pair.
func (s resetSlot) naturalWidth() int {
	if s.pair {
		return lipgloss.Width(s.countdown) + 1 + lipgloss.Width(s.absolute) + 2
	}
	return lipgloss.Width(s.text)
}

// finalize is this slot's finished cell text, once the column's width
// is known.
func (s resetSlot) finalize(width int) string {
	if s.pair {
		return justifyReset(s.countdown, s.absolute, width)
	}
	return s.text
}

// resetSlotFor returns the slot for one window: a pair to justify when
// it carries a reset, an em dash otherwise.
func resetSlotFor(window *usage.LimitWindow, now time.Time, zone *time.Location) resetSlot {
	if window == nil || window.ResetsAt.IsZero() {
		return resetSlot{text: EmptyCell}
	}
	countdown, absolute := resetParts(now, window.ResetsAt, zone)
	return resetSlot{pair: true, countdown: countdown, absolute: absolute}
}

// columnWidth is the width a reset column's cells are justified to: the
// widest natural width among every row sharing the column, or zero for
// a column with no rows (an empty report still prints its headings).
func columnWidth(column []resetSlot) int {
	width := 0
	for _, slot := range column {
		width = max(width, slot.naturalWidth())
	}
	return width
}

// resetIndex shifts a ten-cell index by one when the identity view
// spliced Kind in before it — both reset columns sit after KindIndex,
// so both shift together.
func resetIndex(index int, byIdentity bool) int {
	if byIdentity && index >= KindIndex {
		return index + 1
	}
	return index
}

// orEmpty returns value, or an em dash when it is blank.
func orEmpty(value string) string {
	if value == "" {
		return EmptyCell
	}
	return value
}

// renderTable lays headings and records out in the shared style: a
// header rule and column separators only, no outer box, everything
// flush left with one space of padding on each side, right padding
// preserved, and no reflow to the terminal's width. The result is
// passed through a colour-profile writer pinned to the no-terminal
// profile, so the table is a plain string wherever it goes — there is
// no colour switch, because nothing is ever coloured.
func renderTable(headings []string, records [][]string) string {
	cell := lipgloss.NewStyle().Padding(0, 1)
	t := table.New().
		Border(lipgloss.Border{Top: "-", Left: "|", Middle: "+"}).
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(true).
		BorderHeader(true).
		StyleFunc(func(_, _ int) lipgloss.Style { return cell }).
		Headers(headings...).
		Rows(records...)
	return plain(t.String())
}

// plain strips everything a terminal would interpret, by writing
// through a colour-profile writer pinned to the no-terminal profile.
// The tables above never style anything, so this is the proof rather
// than a conversion; on the impossible write error the styled string is
// still returned, because a display path must not fail.
func plain(s string) string {
	var b strings.Builder
	w := &colorprofile.Writer{Forward: &b, Profile: colorprofile.NoTTY}
	if _, err := io.WriteString(w, s); err != nil {
		return s
	}
	return b.String()
}
