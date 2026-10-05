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
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// reportNow is the fixed clock every table here is rendered against, so
// a countdown is a constant rather than a moving target: 09:00 on
// Tuesday 2026-09-08 in the +09:00 zone the tables are rendered in.
const reportNow = "2026-09-08T00:00:00Z"

func window(kind WindowKind, percent int, resetsAt time.Time) LimitWindow {
	return LimitWindow{Kind: kind, PercentFloor: percent, ResetsAt: resetsAt}
}

// healthyWindows is the three windows a healthy subscription account
// reports.
func healthyWindows(tb testing.TB) []LimitWindow {
	tb.Helper()
	return []LimitWindow{
		window(SessionWindow(), 21, ts(tb, "2026-09-08T02:13:40Z")),
		window(WeeklyAllWindow(), 35, ts(tb, "2026-09-10T20:00:00Z")),
		window(WeeklyScopedWindow("Fable"), 56, ts(tb, "2026-09-10T20:00:00Z")),
	}
}

func healthyRow(tb testing.TB, account string) StatusRow {
	tb.Helper()
	return StatusRow{
		Account:          account,
		Org:              "Acme",
		Plan:             "max",
		State:            "ok",
		Usage:            &UsageSnapshot{Windows: healthyWindows(tb), Credits: CreditsUnavailable()},
		VisibleByDefault: true,
		Kind:             "owned",
	}
}

// emptyRow is a row with no numbers, in the given state.
func emptyRow(account, state string) StatusRow {
	return StatusRow{Account: account, State: state, VisibleByDefault: true, Kind: "owned"}
}

func report(tb testing.TB, rows []StatusRow, showAll bool) *Report {
	tb.Helper()
	return &Report{Rows: rows, Now: ts(tb, reportNow), Zone: plusNine, ShowAll: showAll}
}

// byIdentity is the same report, rendered the way the identity view
// renders it.
func byIdentity(tb testing.TB, rows []StatusRow) *Report {
	tb.Helper()
	return &Report{Rows: rows, Now: ts(tb, reportNow), Zone: plusNine, ByIdentity: true}
}

// cellsOf returns the cells of the rendered row whose first column
// contains needle. The reset columns are asserted by position rather
// than by substring search, because "the 5h reset cell is blank" is a
// claim about a column and a substring search cannot make it.
func cellsOf(tb testing.TB, rendered, needle string) []string {
	tb.Helper()
	for line := range strings.SplitSeq(rendered, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		var cells []string
		for cell := range strings.SplitSeq(line, "|") {
			cells = append(cells, strings.TrimSpace(cell))
		}
		return cells
	}
	tb.Fatalf("no row contains %q:\n%s", needle, rendered)
	return nil
}

// column returns where one heading sits, so assertions name columns
// rather than indices and a reordering fails loudly instead of
// silently.
func column(tb testing.TB, headings []string, heading string) int {
	tb.Helper()
	index := slices.Index(headings, heading)
	if index < 0 {
		tb.Fatalf("%q is not one of the headings: %v", heading, headings)
	}
	return index
}

func TestHeadingsColumnOrder(t *testing.T) {
	t.Parallel()

	want := []string{
		"Account", "Org", "Plan", "5h", "Weekly", "Fable (weekly)",
		"Credits", "5h reset", "Weekly reset", "State",
	}
	if diff := gocmp.Diff(want, Headings(false)); diff != "" {
		t.Errorf("default headings mismatch (-want +got):\n%s", diff)
	}
	if slices.Contains(Headings(false), KindHeading) {
		t.Errorf("%q must not be one of the default columns", KindHeading)
	}
}

func TestHeadingsByIdentityAddsKindAfterPlan(t *testing.T) {
	t.Parallel()

	// The extra column is opt-in: the default table keeps its fixed
	// columns, and an opt-in view is not a reason to widen what everyone
	// else sees.
	with := Headings(true)
	if got, want := len(with), len(Headings(false))+1; got != want {
		t.Fatalf("identity view has %d columns, want %d", got, want)
	}
	if with[KindIndex] != KindHeading {
		t.Errorf("column %d is %q, want %q", KindIndex, with[KindIndex], KindHeading)
	}
	if with[KindIndex-1] != "Plan" {
		t.Errorf("the kind column should sit with the identification columns, after %q", with[KindIndex-1])
	}
	if with[KindIndex+1] != "5h" {
		t.Errorf("the kind column should sit before the first figure, got %q after it", with[KindIndex+1])
	}
	// Removing it again gives back exactly the base columns, in order:
	// the view adds a column, it never reorders or renames one.
	without := slices.Delete(slices.Clone(with), KindIndex, KindIndex+1)
	if diff := gocmp.Diff(Headings(false), without); diff != "" {
		t.Errorf("splice is not clean (-want +got):\n%s", diff)
	}
}

func TestCodexHeadingsKeepTheirOwnColumnList(t *testing.T) {
	t.Parallel()

	codex := CodexHeadings()
	if got, want := len(codex), 9; got != want {
		t.Fatalf("codex column count = %d, want %d: %v", got, want, codex)
	}
	if slices.Contains(codex, "Org") {
		t.Errorf("a Codex account has no organization: %v", codex)
	}
	if slices.Contains(codex, "Fable (weekly)") {
		t.Errorf("a Codex account has no headline scoped window: %v", codex)
	}
}

func TestFooterAgreesWithItselfOnNumber(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		hidden int
		want   string
	}{
		"success: one entry":   {hidden: 1, want: "1 entry hidden (--all)"},
		"success: two entries": {hidden: 2, want: "2 entries hidden (--all)"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.want, Footer(tt.hidden)); diff != "" {
				t.Errorf("Footer(%d) mismatch (-want +got):\n%s", tt.hidden, diff)
			}
		})
	}
}

func TestHiddenRowsAreSummarisedInAFooter(t *testing.T) {
	t.Parallel()

	sibling := healthyRow(t, "5cdc535f")
	sibling.Usage = nil
	sibling.State = "stale sibling of live"
	sibling.VisibleByDefault = false

	switcher := emptyRow("claude-switcher:user", "unclaimed")
	switcher.VisibleByDefault = false

	rendered := Render(report(t, []StatusRow{healthyRow(t, "alice@example.com"), sibling, switcher}, false))
	if !strings.HasSuffix(rendered, "2 entries hidden (--all)") {
		t.Errorf("the footer should close the output, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "5cdc535f") {
		t.Errorf("a hidden row must not be printed:\n%s", rendered)
	}
}

func TestShowAllRevealsTheHiddenRowsAndDropsTheFooter(t *testing.T) {
	t.Parallel()

	sibling := emptyRow("5cdc535f", "stale sibling of live")
	sibling.VisibleByDefault = false

	rendered := Render(report(t, []StatusRow{healthyRow(t, "alice@example.com"), sibling}, true))
	if !strings.Contains(rendered, "5cdc535f") {
		t.Errorf("every row should be shown, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "hidden (--all)") {
		t.Errorf("no footer when nothing is hidden, got:\n%s", rendered)
	}
}

func TestARowWithNoNumbersShowsEmDashesRatherThanZeroes(t *testing.T) {
	t.Parallel()

	// "0%" would be a claim about the account. An account whose keychain
	// is locked has not been measured at all.
	row := emptyRow("alice@example.com", "keychain locked")
	row.Org = "Acme"

	rendered := Render(report(t, []StatusRow{row}, false))
	if strings.Contains(rendered, "0%") {
		t.Errorf("an unmeasured account must not claim zero usage:\n%s", rendered)
	}
	cells := cellsOf(t, rendered, "alice@example.com")
	for _, heading := range []string{"5h", "Weekly", "Fable (weekly)", "Credits", "5h reset", "Weekly reset"} {
		if got := cells[column(t, Headings(false), heading)]; got != EmptyCell {
			t.Errorf("the %s cell = %q, want the em dash", heading, got)
		}
	}
}

func TestBothResetColumnsCarryALocalTimeAndACountdown(t *testing.T) {
	t.Parallel()

	// The healthy windows reset at 02:13:40Z and 20:00:00Z, which in
	// +09:00 are 11:13 the same local morning and 05:00 on Friday. The
	// weekly cell is the point of the whole column: "2d20h" alone never
	// said which morning. With only one row in each column, the gap is
	// the guaranteed single space.
	rendered := Render(report(t, []StatusRow{healthyRow(t, "alice@example.com")}, false))
	cells := cellsOf(t, rendered, "alice@example.com")

	if got := cells[column(t, Headings(false), "5h reset")]; got != "2h13m (11:13 AM)" {
		t.Errorf("5h reset = %q, want %q in:\n%s", got, "2h13m (11:13 AM)", rendered)
	}
	if got := cells[column(t, Headings(false), "Weekly reset")]; got != "2d20h (Fri 05:00 AM)" {
		t.Errorf("Weekly reset = %q, want %q in:\n%s", got, "2d20h (Fri 05:00 AM)", rendered)
	}
}

func TestARowWithOnlyAWeeklyWindowLeavesTheFiveHourResetAnEmDash(t *testing.T) {
	t.Parallel()

	row := healthyRow(t, "alice@example.com")
	row.Usage = &UsageSnapshot{
		Windows: []LimitWindow{window(WeeklyAllWindow(), 35, ts(t, "2026-09-10T20:00:00Z"))},
		Credits: CreditsUnavailable(),
	}

	rendered := Render(report(t, []StatusRow{row}, false))
	cells := cellsOf(t, rendered, "alice@example.com")

	if got := cells[column(t, Headings(false), "5h reset")]; got != EmptyCell {
		t.Errorf("5h reset = %q, want the em dash in:\n%s", got, rendered)
	}
	if got := cells[column(t, Headings(false), "Weekly reset")]; got != "2d20h (Fri 05:00 AM)" {
		t.Errorf("Weekly reset = %q in:\n%s", got, rendered)
	}
}

func TestAWindowThatCarriesNoResetIsAnEmDashRatherThanABareCountdown(t *testing.T) {
	t.Parallel()

	// A window with a percentage and no reset is a real shape, and the
	// percentage columns must still fill.
	row := healthyRow(t, "alice@example.com")
	row.Usage = &UsageSnapshot{
		Windows: []LimitWindow{
			window(SessionWindow(), 21, time.Time{}),
			window(WeeklyAllWindow(), 35, time.Time{}),
		},
		Credits: CreditsUnavailable(),
	}

	rendered := Render(report(t, []StatusRow{row}, false))
	cells := cellsOf(t, rendered, "alice@example.com")

	if got := cells[column(t, Headings(false), "5h")]; got != "21%" {
		t.Errorf("the figure is there; only the reset is missing, got %q", got)
	}
	if got := cells[column(t, Headings(false), "5h reset")]; got != EmptyCell {
		t.Errorf("5h reset = %q, want the em dash in:\n%s", got, rendered)
	}
	if got := cells[column(t, Headings(false), "Weekly reset")]; got != EmptyCell {
		t.Errorf("Weekly reset = %q, want the em dash in:\n%s", got, rendered)
	}
}

func TestAnUnknownWindowGetsAContinuationRowNamingItsKind(t *testing.T) {
	t.Parallel()

	windows := healthyWindows(t)
	windows = append(windows,
		window(UnknownWindow("monthly_foo"), 7, ts(t, "2026-10-01T00:00:00Z")),
		window(WeeklyScopedWindow("opus"), 12, time.Time{}),
	)
	row := healthyRow(t, "alice@example.com")
	row.Usage = &UsageSnapshot{Windows: windows, Credits: CreditsUnavailable()}

	rendered := Render(report(t, []StatusRow{row}, false))
	if !strings.Contains(rendered, "↳ monthly_foo (unknown kind)") {
		t.Errorf("the unknown kind should be named verbatim:\n%s", rendered)
	}
	if !strings.Contains(rendered, "↳ opus (weekly)") {
		t.Errorf("the non-headline scope should get its own row:\n%s", rendered)
	}
}

func TestAContinuationRowsResetSitsInTheWeeklyColumn(t *testing.T) {
	t.Parallel()

	windows := healthyWindows(t)
	windows = append(windows, window(UnknownWindow("monthly_foo"), 7, ts(t, "2026-10-01T00:00:00Z")))
	row := healthyRow(t, "alice@example.com")
	row.Usage = &UsageSnapshot{Windows: windows, Credits: CreditsUnavailable()}

	rendered := Render(report(t, []StatusRow{row}, false))
	cells := cellsOf(t, rendered, "↳ monthly_foo")

	// Such a window is never the five-hour one, so that cell stays blank
	// — not an em dash, which would claim a figure was unavailable.
	if got := cells[column(t, Headings(false), "5h reset")]; got != "" {
		t.Errorf("5h reset = %q, want blank in:\n%s", got, rendered)
	}
	// Twenty-three days out: past a week, so the date names the day
	// rather than a weekday that would come round again first, and the
	// hour is zero-padded for that shape's constant width.
	if got := cells[column(t, Headings(false), "Weekly reset")]; got != "23d0h (Oct 1 09:00 AM)" {
		t.Errorf("Weekly reset = %q in:\n%s", got, rendered)
	}
}

func TestTheWeeklyResetColumnRightAlignsItsClosingParenAcrossRows(t *testing.T) {
	t.Parallel()

	// Two rows whose weekly countdowns differ in length ("6d5h" vs
	// "19h32m"), so the column's width comes from the longer one and the
	// shorter row's parenthesis must still land on the same right edge:
	// "6d5h" gets three extra spaces where "19h32m" gets one.
	short := healthyRow(t, "alice@example.com")
	short.Usage = &UsageSnapshot{
		Windows: []LimitWindow{window(WeeklyAllWindow(), 35, ts(t, "2026-09-14T05:00:00Z"))},
		Credits: CreditsUnavailable(),
	}
	long := healthyRow(t, "bob@example.com")
	long.Usage = &UsageSnapshot{
		Windows: []LimitWindow{window(WeeklyAllWindow(), 35, ts(t, "2026-09-08T19:32:00Z"))},
		Credits: CreditsUnavailable(),
	}

	rendered := Render(report(t, []StatusRow{short, long}, false))
	alice := cellsOf(t, rendered, "alice@example.com")
	bob := cellsOf(t, rendered, "bob@example.com")

	weekly := column(t, Headings(false), "Weekly reset")
	if got := alice[weekly]; got != "6d5h   (Mon 02:00 PM)" {
		t.Errorf("alice weekly reset = %q in:\n%s", got, rendered)
	}
	if got := bob[weekly]; got != "19h32m (Wed 04:32 AM)" {
		t.Errorf("bob weekly reset = %q in:\n%s", got, rendered)
	}
	if len(alice[weekly]) != len(bob[weekly]) {
		t.Errorf("both rows should reach the same column width:\n%s", rendered)
	}
}

func TestTheKindCellReadsLiveAndOwnedForAFoldedRowAndThePlainKindOtherwise(t *testing.T) {
	t.Parallel()

	folded := healthyRow(t, "alice@example.com")
	folded.Kind = LiveAndOwnedKind
	plain := healthyRow(t, "bob@example.com")
	plain.Kind = "owned"

	rendered := Render(byIdentity(t, []StatusRow{folded, plain}))
	headings := Headings(true)

	// Asserted by column position, not by substring, so a cell landing
	// in the wrong column fails here rather than passing on a match.
	kind := column(t, headings, KindHeading)
	if got := cellsOf(t, rendered, "alice@example.com")[kind]; got != "live+owned" {
		t.Errorf("folded kind cell = %q in:\n%s", got, rendered)
	}
	if got := cellsOf(t, rendered, "bob@example.com")[kind]; got != "owned" {
		t.Errorf("plain kind cell = %q in:\n%s", got, rendered)
	}
	// The figures did not shift: the column was inserted, not overlaid.
	if got := cellsOf(t, rendered, "alice@example.com")[column(t, headings, "5h")]; got != "21%" {
		t.Errorf("the 5h figure shifted to %q in:\n%s", got, rendered)
	}
}

func TestAContinuationRowLeavesTheKindCellBlank(t *testing.T) {
	t.Parallel()

	// The window belongs to the account named above it, so repeating
	// that account's kind on it would read as a second account.
	windows := healthyWindows(t)
	windows = append(windows, window(WeeklyScopedWindow("opus"), 12, time.Time{}))
	row := healthyRow(t, "alice@example.com")
	row.Kind = LiveAndOwnedKind
	row.Usage = &UsageSnapshot{Windows: windows, Credits: CreditsUnavailable()}

	rendered := Render(byIdentity(t, []StatusRow{row}))
	var continuation string
	for line := range strings.SplitSeq(rendered, "\n") {
		if strings.Contains(line, ContinuationMarker) {
			continuation = line
			break
		}
	}
	if continuation == "" {
		t.Fatalf("a continuation row should be rendered:\n%s", rendered)
	}
	if strings.Contains(continuation, "owned") {
		t.Errorf("the continuation row carries no kind: %q", continuation)
	}
	if got, want := strings.Count(continuation, "|")+1, len(Headings(true)); got != want {
		t.Errorf("the continuation row has %d cells, want one per column (%d): %q", got, want, continuation)
	}
}

func TestAnEmptyReportStillExplainsItself(t *testing.T) {
	t.Parallel()

	hidden := emptyRow("sibling", "stale sibling of live")
	hidden.VisibleByDefault = false

	rendered := Render(report(t, []StatusRow{hidden}, false))
	if !strings.Contains(rendered, "Account") {
		t.Errorf("the headings survive an empty body:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "1 entry hidden (--all)") {
		t.Errorf("the footer still counts, got:\n%s", rendered)
	}
}

func money(amountMinor int64, currency string, exponent uint8) *Money {
	return &Money{AmountMinor: amountMinor, Currency: currency, Exponent: exponent}
}

func TestTheCreditsCellCoversEveryStateTheColumnCanReach(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		credits CreditsState
		want    string
	}{
		"success: no credits information at all renders n/a": {
			credits: CreditsUnavailable(),
			want:    "n/a",
		},
		"success: credits switched off render off, not n/a": {
			credits: CreditsOff(),
			want:    "off",
		},
		"success: capped credits carry the used, the limit and the percent": {
			credits: CreditsOn(Credits{Used: money(1234, "USD", 2), Limit: money(5000, "USD", 2), Percent: 25}),
			want:    "$12.34 / $50.00 (25%)",
		},
		"success: an uncapped account says Unlimited and drops the percent": {
			credits: CreditsOn(Credits{Used: money(1234, "USD", 2), Percent: -1}),
			want:    "$12.34 / Unlimited",
		},
		"success: credits on with no used figure are unavailable, not an unlimited nothing": {
			credits: CreditsOn(Credits{Percent: -1}),
			want:    EmptyCell,
		},
		"success: the real capture's figures render to the cent": {
			credits: CreditsOn(Credits{Used: money(21956, "USD", 2), Limit: money(500000, "USD", 2), Percent: 4}),
			want:    "$219.56 / $5000.00 (4%)",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := healthyRow(t, "owner@example.com")
			row.Usage = &UsageSnapshot{Windows: healthyWindows(t), Credits: tt.credits}
			rendered := Render(report(t, []StatusRow{row}, false))
			got := cellsOf(t, rendered, "owner@example.com")[column(t, Headings(false), "Credits")]
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("credits cell mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMoneyString(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		money Money
		want  string
	}{
		"success: USD renders with the dollar sign": {
			money: Money{AmountMinor: 1234, Currency: "USD", Exponent: 2},
			want:  "$12.34",
		},
		"success: another currency renders with its code": {
			money: Money{AmountMinor: 1234, Currency: "EUR", Exponent: 2},
			want:  "EUR 12.34",
		},
		"success: an empty currency renders the bare figure": {
			money: Money{AmountMinor: 1234, Exponent: 2},
			want:  "12.34",
		},
		"success: a zero exponent renders no decimal point": {
			money: Money{AmountMinor: 1234, Currency: "USD"},
			want:  "$1234",
		},
		"success: a three-decimal currency keeps its leading zeros": {
			money: Money{AmountMinor: 1005, Currency: "KWD", Exponent: 3},
			want:  "KWD 1.005",
		},
		"success: a negative amount keeps the sign ahead of the symbol": {
			money: Money{AmountMinor: -1234, Currency: "USD", Exponent: 2},
			want:  "-$12.34",
		},
		"success: the most negative amount still renders": {
			money: Money{AmountMinor: math.MinInt64, Currency: "USD", Exponent: 2},
			want:  "-$92233720368547758.08",
		},
		"success: a nonsensical exponent is clamped rather than refused": {
			money: Money{AmountMinor: 1234, Currency: "USD", Exponent: 200},
			want:  "$0.001234",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.want, tt.money.String()); diff != "" {
				t.Errorf("Money.String() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStateCellJoinsItsNotesWithASemicolon(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		row  StatusRow
		want string
	}{
		"success: a bare state stays bare": {
			row:  StatusRow{State: "ok"},
			want: "ok",
		},
		"success: one note sits in parentheses": {
			row:  StatusRow{State: "rate-limited (retry in 30s)", Note: "showing cached values"},
			want: "rate-limited (retry in 30s) (showing cached values)",
		},
		"success: the live twin earns its note": {
			row:  StatusRow{State: "ok", SameIdentityAsLive: true},
			want: "ok (same identity as live)",
		},
		"success: two notes are joined rather than one replacing the other": {
			// A row can be both expired for a stated reason and the live
			// account's twin, and dropping either would answer half the
			// reader's question.
			row:  StatusRow{State: "expired", Note: "refresh refused", SameIdentityAsLive: true},
			want: "expired (refresh refused; same identity as live)",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.want, tt.row.StateCell()); diff != "" {
				t.Errorf("StateCell() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func codexRow(tb testing.TB, account string) CodexTableRow {
	tb.Helper()
	session := window(SessionWindow(), 21, ts(tb, "2026-09-08T02:13:40Z"))
	weekly := window(WeeklyAllWindow(), 35, ts(tb, "2026-09-10T20:00:00Z"))
	return CodexTableRow{
		Account:          account,
		Plan:             "plus",
		Kind:             "live",
		Session:          &session,
		Weekly:           &weekly,
		Credits:          "",
		State:            "ok",
		VisibleByDefault: true,
	}
}

func TestRenderCodexKeepsItsOwnColumns(t *testing.T) {
	t.Parallel()

	row := codexRow(t, "dev@example.com")
	row.Extra = []LimitWindow{window(UnknownWindow("monthly_foo"), 7, ts(t, "2026-10-01T00:00:00Z"))}
	hidden := codexRow(t, "forgotten@example.com")
	hidden.VisibleByDefault = false

	rendered := RenderCodex(&CodexReport{
		Rows: []CodexTableRow{row, hidden},
		Now:  ts(t, reportNow),
		Zone: plusNine,
	})

	cells := cellsOf(t, rendered, "dev@example.com")
	codex := CodexHeadings()
	if got := cells[column(t, codex, "Kind")]; got != "live" {
		t.Errorf("kind cell = %q in:\n%s", got, rendered)
	}
	if got := cells[column(t, codex, "Credits")]; got != EmptyCell {
		t.Errorf("an empty credits cell renders the em dash, got %q", got)
	}
	if got := cells[column(t, codex, "5h reset")]; got != "2h13m (11:13 AM)" {
		t.Errorf("5h reset = %q in:\n%s", got, rendered)
	}
	if got := cells[column(t, codex, "Weekly reset")]; got != "2d20h   (Fri 05:00 AM)" {
		t.Errorf("Weekly reset = %q, justified against the continuation row's longer cell in:\n%s", got, rendered)
	}

	continuation := cellsOf(t, rendered, "↳ monthly_foo")
	if got := continuation[column(t, codex, "Weekly")]; got != "7%" {
		t.Errorf("continuation percentage = %q in:\n%s", got, rendered)
	}
	if got := continuation[column(t, codex, "5h reset")]; got != "" {
		t.Errorf("a continuation row's 5h reset stays blank, got %q", got)
	}
	if got := continuation[column(t, codex, "Weekly reset")]; got != "23d0h (Oct 1 09:00 AM)" {
		t.Errorf("continuation reset = %q in:\n%s", got, rendered)
	}

	if strings.Contains(rendered, "forgotten@example.com") {
		t.Errorf("a hidden row must not be printed:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "1 entry hidden (--all)") {
		t.Errorf("the footer should count the hidden row:\n%s", rendered)
	}
}

func TestCodexAccountsLine(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		id        string
		kind      string
		email     string
		forgotten bool
		want      string
	}{
		"success: id, kind and email, two spaces apart": {
			id: "user-1", kind: "owned", email: "dev@example.com",
			want: "user-1  owned  dev@example.com",
		},
		"success: a missing email is a dash": {
			id: "user-2", kind: "live", email: "",
			want: "user-2  live  -",
		},
		"success: a forgotten row says so": {
			id: "user-3", kind: "owned", email: "old@example.com", forgotten: true,
			want: "user-3  owned  old@example.com  (forgotten)",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := CodexAccountsLine(tt.id, tt.kind, tt.email, tt.forgotten)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("CodexAccountsLine mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
