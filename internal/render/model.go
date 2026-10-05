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
	"strings"
	"time"
)

// StatusRow is the boundary between an account pass and its presentation.
// Everything above it — the keychain, the namespace lock, the HTTP client
// — has finished by the time a row exists, and everything below it is
// presentation. That split is why the table can be tested without
// standing up a pass.
//
// A row carries a rendered state string rather than a state value, so
// that the renderer cannot accidentally make a policy decision — such as
// deciding that some state or other should be hidden. Visibility and the
// exit status are settled before a row is built.

// SameIdentityNote is what the State column appends to a row whose
// account and organization UUIDs are the live credential's.
//
// A note rather than a state, because it is not a problem: two
// independent token pairs for one account are a legitimate setup, and
// both sessions stay valid. What the reader needs is the reason the same
// address appears twice.
const SameIdentityNote = "same identity as live"

// LiveAndOwnedKind is the Kind cell of a row two rows were folded into:
// the live credential first, because that is the one already in use,
// then the store this binary owns.
const LiveAndOwnedKind = "live+owned"

// HeadlineScope is the display name of the scoped weekly window that has
// a column of its own. The renderer owns the pairing between this scope
// and the fixed column heading that names it; every other scoped window
// becomes a continuation row.
const HeadlineScope = "Fable"

// windowClass says which of the four window shapes a WindowKind is.
type windowClass uint8

const (
	windowSession windowClass = iota
	windowWeeklyAll
	windowWeeklyScoped
	windowUnknown
)

// WindowKind says which window a LimitWindow describes. The zero value
// is the rolling five-hour session window.
type WindowKind struct {
	class windowClass
	// name carries the scope display name for a scoped weekly window, or
	// the verbatim kind string for an unrecognised one.
	name string
}

// SessionWindow is the rolling five-hour window.
func SessionWindow() WindowKind {
	return WindowKind{class: windowSession}
}

// WeeklyAllWindow is the weekly window covering every model.
func WeeklyAllWindow() WindowKind {
	return WindowKind{class: windowWeeklyAll}
}

// WeeklyScopedWindow is a weekly window scoped to one model, named by
// its display name.
func WeeklyScopedWindow(scope string) WindowKind {
	return WindowKind{class: windowWeeklyScoped, name: scope}
}

// UnknownWindow is a window kind this build does not recognise, kept
// verbatim.
func UnknownWindow(kind string) WindowKind {
	return WindowKind{class: windowUnknown, name: kind}
}

// Label is what this window's continuation row calls it. Named windows
// say what they are scoped to; an unrecognised kind says so verbatim,
// because the kind string is the only evidence the user has that
// something new appeared.
func (k WindowKind) Label() string {
	switch k.class {
	case windowSession:
		return "session"
	case windowWeeklyAll:
		return "weekly"
	case windowWeeklyScoped:
		return k.name + " (weekly)"
	default:
		return k.name + " (unknown kind)"
	}
}

// LimitWindow is one usage window, whatever the provider called it, with
// only what the renderer reads: the kind, the floored percentage, and
// when the window rolls over.
type LimitWindow struct {
	// Kind says which window this is.
	Kind WindowKind
	// PercentFloor is the percentage consumed, floored to a whole number
	// in 0..100. A negative value means the server sent no usable figure,
	// which renders as an em dash: a missing measurement is not zero.
	PercentFloor int
	// ResetsAt is when the window rolls over; the zero time means the
	// response named no reset.
	ResetsAt time.Time
}

// UsageSnapshot is one account's usage at one moment, reduced to what
// the table and the watch rows read.
type UsageSnapshot struct {
	// Windows holds every window the response described, in the order it
	// described them.
	Windows []LimitWindow
	// Credits is what is known about extra-usage credits.
	Credits CreditsState
}

// Window returns the first window of the given kind, or nil when the
// response carried none.
func (u *UsageSnapshot) Window(kind WindowKind) *LimitWindow {
	for i := range u.Windows {
		if u.Windows[i].Kind == kind {
			return &u.Windows[i]
		}
	}
	return nil
}

// ScopedWindow returns the first weekly window scoped to scope, compared
// case-insensitively: the column heading is fixed while the server sends
// a display name it may capitalise differently from one release to the
// next, and a case flip should not blank the column.
func (u *UsageSnapshot) ScopedWindow(scope string) *LimitWindow {
	for i := range u.Windows {
		kind := u.Windows[i].Kind
		if kind.class == windowWeeklyScoped && strings.EqualFold(kind.name, scope) {
			return &u.Windows[i]
		}
	}
	return nil
}

// ExtraWindows returns every window that does not have a column of its
// own, in order: scoped weeklies other than the headline one, and
// anything of an unrecognised kind. These become the continuation rows
// under an account.
func (u *UsageSnapshot) ExtraWindows(headlineScope string) []*LimitWindow {
	var extra []*LimitWindow
	for i := range u.Windows {
		switch kind := u.Windows[i].Kind; kind.class {
		case windowWeeklyScoped:
			if !strings.EqualFold(kind.name, headlineScope) {
				extra = append(extra, &u.Windows[i])
			}
		case windowUnknown:
			extra = append(extra, &u.Windows[i])
		case windowSession, windowWeeklyAll:
		}
	}
	return extra
}

// NextReset returns the soonest reset across every window, or false when
// no window carries one.
func (u *UsageSnapshot) NextReset() (time.Time, bool) {
	var soonest time.Time
	for i := range u.Windows {
		at := u.Windows[i].ResetsAt
		if at.IsZero() {
			continue
		}
		if soonest.IsZero() || at.Before(soonest) {
			soonest = at
		}
	}
	return soonest, !soonest.IsZero()
}

// creditsClass says which of the three credit facts is known.
type creditsClass uint8

const (
	creditsUnavailable creditsClass = iota
	creditsOff
	creditsOn
)

// CreditsState is what is known about an account's extra-usage credits.
// It distinguishes "switched off" from "the response said nothing",
// because those are different facts about an account and only one of
// them is worth acting on. The zero value is the unavailable state.
type CreditsState struct {
	class   creditsClass
	credits Credits
}

// CreditsUnavailable records that the response carried no credits
// information at all. Rendered "n/a" — not the same as off: this build
// looked and the server said nothing.
func CreditsUnavailable() CreditsState {
	return CreditsState{class: creditsUnavailable}
}

// CreditsOff records that credits exist for the account but are switched
// off. Rendered "off".
func CreditsOff() CreditsState {
	return CreditsState{class: creditsOff}
}

// CreditsOn records that credits are on, with the figures in credits.
func CreditsOn(credits Credits) CreditsState {
	return CreditsState{class: creditsOn, credits: credits}
}

// Credits holds the figures for an account with extra usage enabled.
type Credits struct {
	// Used is how much has been spent, or nil when the server sent no
	// figure.
	Used *Money
	// Limit is the monthly ceiling, or nil for an uncapped account.
	Limit *Money
	// Percent is the server's own utilisation percentage, rounded and
	// clamped to 0..100. A negative value means the server sent none.
	Percent int
}

// maxMoneyExponent is the largest exponent Money honours. Beyond this
// the minor-unit divisor stops fitting anything sane, and the only
// currencies in play use 0, 2 or 3. Anything larger is clamped rather
// than rejected: a display type must not fail.
const maxMoneyExponent = 6

// Money is an amount in minor units. Minor units rather than a float
// because money compared or summed as a float drifts, and because the
// server already sends the amount and the exponent separately —
// reconstituting a float would throw away the exactness it took care to
// send.
type Money struct {
	// AmountMinor is the amount, in units of 10^-Exponent of the
	// currency. May be negative: a refunded or credited account is a real
	// state.
	AmountMinor int64
	// Currency is the ISO 4217 code.
	Currency string
	// Exponent is how many decimal places the minor unit represents.
	Exponent uint8
}

// String renders "$12.34" for USD and "EUR 12.34" for anything else. It
// never fails for any amount, including the most negative one, or any
// exponent: the magnitude is taken as an unsigned value, which is total,
// and the exponent is clamped before it reaches the divisor.
func (m Money) String() string {
	exponent := min(m.Exponent, maxMoneyExponent)
	scale := uint64(1)
	for range exponent {
		scale *= 10
	}
	magnitude := uint64(m.AmountMinor)
	if m.AmountMinor < 0 {
		magnitude = -magnitude
	}
	whole := magnitude / scale
	fraction := magnitude % scale

	var b strings.Builder
	if m.AmountMinor < 0 {
		b.WriteString("-")
	}
	switch m.Currency {
	case "USD":
		b.WriteString("$")
	case "":
	default:
		b.WriteString(m.Currency)
		b.WriteString(" ")
	}
	if exponent == 0 {
		fmt.Fprintf(&b, "%d", whole)
	} else {
		fmt.Fprintf(&b, "%d.%0*d", whole, int(exponent), fraction)
	}
	return b.String()
}

// StatusRow is one account, ready to render.
type StatusRow struct {
	// Account is how the account is named in the first column: its email
	// when known, otherwise the id the account flag accepts.
	Account string
	// Org is the organization's display name, or its UUID when unnamed.
	Org string
	// Plan is the subscription tier, as the credential recorded it.
	Plan string
	// State is the state column's text.
	State string
	// Note is a short explanation appended to the state, when there is
	// one.
	Note string
	// Usage holds the numbers, when this row has any.
	Usage *UsageSnapshot
	// VisibleByDefault says whether the row appears without the flag
	// that shows every row.
	VisibleByDefault bool
	// SameIdentityAsLive says whether this row's account and
	// organization pair is the live credential's, which is what puts
	// SameIdentityNote in the state cell.
	SameIdentityAsLive bool
	// Kind is carried on every row but printed only when the identity
	// column is requested, so the default table keeps its fixed columns.
	Kind string
}

// StateCell returns the state column's full text: the state, then its
// notes in parentheses. Two notes are joined with a semicolon rather
// than one replacing the other: a row can be both expired for a stated
// reason and the live account's twin, and dropping either would answer
// half the reader's question.
func (r *StatusRow) StateCell() string {
	notes := make([]string, 0, 2)
	if r.Note != "" {
		notes = append(notes, r.Note)
	}
	if r.SameIdentityAsLive {
		notes = append(notes, SameIdentityNote)
	}
	if len(notes) == 0 {
		return r.State
	}
	return r.State + " (" + strings.Join(notes, "; ") + ")"
}

// Report is a whole pass, ready to render.
type Report struct {
	// Rows holds every row the pass produced, hidden ones included.
	Rows []StatusRow
	// Now is the moment the report was built; every countdown is
	// relative to it, so a table cannot show two cells computed against
	// different clocks.
	Now time.Time
	// Zone is where the reset columns are printed, carried for the same
	// reason Now is: one report, one zone, so two cells cannot disagree
	// about what "Sunday" means. A test injects a fixed one.
	Zone *time.Location
	// ShowAll says whether every row was requested.
	ShowAll bool
	// ByIdentity says whether the identity view was requested, which is
	// the only thing that puts the Kind column on the table. The folding
	// itself has already happened by the time a report exists — which
	// rows survive is a decision about accounts, not about rendering.
	ByIdentity bool
}

// Shown returns the rows that will actually be printed.
func (r *Report) Shown() []*StatusRow {
	var shown []*StatusRow
	for i := range r.Rows {
		if r.ShowAll || r.Rows[i].VisibleByDefault {
			shown = append(shown, &r.Rows[i])
		}
	}
	return shown
}

// HiddenCount returns how many rows the show-everything flag would add.
func (r *Report) HiddenCount() int {
	if r.ShowAll {
		return 0
	}
	hidden := 0
	for i := range r.Rows {
		if !r.Rows[i].VisibleByDefault {
			hidden++
		}
	}
	return hidden
}

// CodexTableRow is one Codex account, ready to render. A separate type
// from StatusRow for the same reason that type exists: the renderer is
// handed finished cells and windows, never a provider state, so it
// cannot decide what to hide. The windows arrive already sorted into the
// two columns and the continuation rows by the pass that built this.
type CodexTableRow struct {
	// Account is the first column: the email when known, else the
	// display id.
	Account string
	// Plan is the plan, or empty when unknown.
	Plan string
	// Kind says where the credential lives.
	Kind string
	// Session is the five-hour window, when the response described one.
	Session *LimitWindow
	// Weekly is the weekly window, when the response described one.
	Weekly *LimitWindow
	// Extra holds every other window, one continuation row each.
	Extra []LimitWindow
	// Credits is the finished credits cell.
	Credits string
	// State is the finished state cell, notes included.
	State string
	// VisibleByDefault says whether the row appears without the flag
	// that shows every row.
	VisibleByDefault bool
}

// CodexReport is a whole Codex pass, ready to render.
type CodexReport struct {
	// Rows holds every row the pass produced, hidden ones included.
	Rows []CodexTableRow
	// Now is the moment every countdown is relative to.
	Now time.Time
	// Zone is where the reset columns are printed.
	Zone *time.Location
	// ShowAll says whether every row was requested.
	ShowAll bool
}

// HiddenCount returns how many rows the show-everything flag would add.
func (r *CodexReport) HiddenCount() int {
	if r.ShowAll {
		return 0
	}
	hidden := 0
	for i := range r.Rows {
		if !r.Rows[i].VisibleByDefault {
			hidden++
		}
	}
	return hidden
}
