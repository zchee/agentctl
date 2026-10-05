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

package usage

import (
	"encoding/json/jsontext"
	"fmt"
	"math"
	"time"
)

// MaxMoneyExponent is the largest exponent [Money] will honour.
//
// Beyond this the minor-unit divisor stops fitting anything sane, and the
// only currencies in play use 0, 2 or 3. Anything larger is clamped rather
// than rejected: a display type must not fail.
const MaxMoneyExponent uint8 = 6

// DefaultMoneyExponent is the exponent assumed when the server sends none,
// or sends a nonsensical one.
//
// Two, because every currency the endpoint has been observed to quote is a
// two-decimal one, and because the alternative — refusing to show a figure
// the server did send — is worse than showing it with the wrong number of
// decimals.
const DefaultMoneyExponent uint8 = 2

// WindowClass says which family of usage window a [WindowKind] names.
type WindowClass uint8

const (
	// WindowSession is the rolling five-hour session window.
	WindowSession WindowClass = iota
	// WindowWeeklyAll is the weekly window covering every model.
	WindowWeeklyAll
	// WindowWeeklyScoped is a weekly window scoped to one model.
	WindowWeeklyScoped
	// WindowUnknown is a window kind this build does not recognise.
	WindowUnknown
)

// WindowKind identifies one usage window: a class, plus — for a scoped
// window — the scope's display name, or — for an unrecognised kind — the
// kind string kept verbatim. The verbatim string matters because it is the
// only evidence the user has that the server grew a window this build has
// never seen. The type is comparable, so kinds can be matched with ==.
type WindowKind struct {
	// Class is the window family.
	Class WindowClass
	// Name is the scope display name for [WindowWeeklyScoped], the
	// verbatim kind string for [WindowUnknown], and empty otherwise.
	Name string
}

// LimitWindow is one usage window, whatever the provider called it.
type LimitWindow struct {
	// Kind says which window this is.
	Kind WindowKind
	// Percent is the percentage consumed, clamped to 0..=100, or nil when
	// the server sent something that is not a number. Absence is not zero:
	// a nil Percent renders as an em dash, never as 0%.
	Percent *float64
	// PercentFloor is Percent floored to a whole number, for display, and
	// nil exactly when Percent is.
	PercentFloor *int
	// Severity is the server's own severity label, or empty when it sent
	// none.
	Severity string
	// ResetsAt is when the window rolls over; the zero time means the
	// server never said.
	ResetsAt time.Time
	// ScopeLabel is the scope's display name, for a scoped window.
	ScopeLabel string
	// IsActive reports whether this is the window currently constraining
	// the account.
	IsActive bool
}

// Label returns the name this window carries in the table's continuation
// rows.
//
// Named windows say what they are scoped to; an unrecognised kind says so
// verbatim, because the kind string is the only evidence the user has that
// something new appeared.
func (w *LimitWindow) Label() string {
	switch w.Kind.Class {
	case WindowSession:
		return "session"
	case WindowWeeklyAll:
		return "weekly"
	case WindowWeeklyScoped:
		return w.Kind.Name + " (weekly)"
	default:
		return w.Kind.Name + " (unknown kind)"
	}
}

// CreditsClass says what is known about an account's extra-usage credits.
type CreditsClass uint8

const (
	// CreditsUnavailable means the response carried no credits object.
	// Rendered "n/a". Not the same as [CreditsOff]: this build looked and
	// the server said nothing, which is what an account on an older API
	// shape looks like.
	CreditsUnavailable CreditsClass = iota
	// CreditsOff means credits exist for this account but are switched
	// off.
	CreditsOff
	// CreditsOn means credits are on, with the figures in
	// [CreditsState.Credits].
	CreditsOn
)

// CreditsState is what is known about an account's extra-usage credits.
type CreditsState struct {
	// Class distinguishes "the server said nothing", "switched off" and
	// "on with figures".
	Class CreditsClass
	// DisabledReason is the server's own reason, set only for
	// [CreditsOff] and empty when it gave none.
	DisabledReason string
	// Credits holds the figures, meaningful only for [CreditsOn].
	Credits Credits
}

// Credits is the credit figures for an account with extra usage enabled.
type Credits struct {
	// Used is how much has been spent, or nil when the server sent no
	// readable figure.
	Used *Money
	// Limit is the monthly ceiling, or nil for an uncapped account.
	Limit *Money
	// Percent is the server's own utilisation percentage, rounded and
	// clamped, or nil when it sent none.
	Percent *int
}

// Money is an amount of money in minor units.
//
// Minor units rather than a float because money compared or summed as a
// float64 drifts, and because the API already sends the amount and the
// exponent separately — reconstituting a float would throw away the
// exactness the server took care to send.
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

// String renders "$12.34" for USD and "EUR 12.34" for anything else; an
// empty currency renders the bare figure.
//
// It never fails, for any amount (including [math.MinInt64]) and any
// exponent: the magnitude is taken by two's-complement negation in the
// unsigned domain, which is total, and the exponent is clamped to
// [MaxMoneyExponent] before it reaches the divisor, so the divisor always
// fits a uint64.
func (m Money) String() string {
	exponent := min(m.Exponent, MaxMoneyExponent)
	scale := uint64(1)
	for range exponent {
		scale *= 10
	}
	magnitude := uint64(m.AmountMinor)
	if m.AmountMinor < 0 {
		magnitude = -magnitude
	}
	whole, fraction := magnitude/scale, magnitude%scale

	var sign string
	if m.AmountMinor < 0 {
		sign = "-"
	}
	var prefix string
	switch m.Currency {
	case "USD":
		prefix = sign + "$"
	case "":
		prefix = sign
	default:
		prefix = sign + m.Currency + " "
	}

	if exponent == 0 {
		return fmt.Sprintf("%s%d", prefix, whole)
	}
	return fmt.Sprintf("%s%d.%0*d", prefix, whole, int(exponent), fraction)
}

// UsageSnapshot is one account's usage at one moment.
type UsageSnapshot struct {
	// FetchedAt is when the response this was built from was received.
	FetchedAt time.Time
	// Windows is every window the response described, in the order it
	// described them.
	Windows []LimitWindow
	// Credits is what is known about credits.
	Credits CreditsState
	// Raw is the untouched response body, kept only for --raw. It carries
	// usage figures and no token material. Nil when the caller did not ask
	// for it.
	Raw jsontext.Value
}

// Window returns the first window of the given kind, or nil when the
// response carried none.
func (s *UsageSnapshot) Window(kind WindowKind) *LimitWindow {
	for i := range s.Windows {
		if s.Windows[i].Kind == kind {
			return &s.Windows[i]
		}
	}
	return nil
}

// ScopedWindow returns the first weekly window scoped to scope, compared
// case-insensitively over ASCII.
//
// Case-insensitive because the column heading is fixed while the server
// sends a display name it may capitalise differently from one release to
// the next; a case flip should not blank the column.
func (s *UsageSnapshot) ScopedWindow(scope string) *LimitWindow {
	for i := range s.Windows {
		window := &s.Windows[i]
		if window.Kind.Class == WindowWeeklyScoped && equalASCIIFold(window.Kind.Name, scope) {
			return window
		}
	}
	return nil
}

// ExtraWindows returns every window that does not have a column of its
// own, in order: scoped weeklies other than the headline one, and anything
// of an unrecognised kind. These become the continuation rows under an
// account.
func (s *UsageSnapshot) ExtraWindows(headlineScope string) []*LimitWindow {
	var extras []*LimitWindow
	for i := range s.Windows {
		window := &s.Windows[i]
		switch window.Kind.Class {
		case WindowWeeklyScoped:
			if !equalASCIIFold(window.Kind.Name, headlineScope) {
				extras = append(extras, window)
			}
		case WindowUnknown:
			extras = append(extras, window)
		}
	}
	return extras
}

// NextReset returns the soonest reset across every window, or false when no
// window carries one.
func (s *UsageSnapshot) NextReset() (time.Time, bool) {
	var soonest time.Time
	for i := range s.Windows {
		at := s.Windows[i].ResetsAt
		if at.IsZero() {
			continue
		}
		if soonest.IsZero() || at.Before(soonest) {
			soonest = at
		}
	}
	return soonest, !soonest.IsZero()
}

// ClampPercent clamps a server-sent percentage into 0..=100, rejecting
// non-numbers.
//
// A NaN reports false rather than a clamped value: NaN propagates through
// min and max, and printing "NaN%" in a usage table would be worse than
// admitting the figure is unavailable.
func ClampPercent(value float64) (float64, bool) {
	if math.IsNaN(value) {
		return 0, false
	}
	return min(max(value, 0), 100), true
}

// PercentFloor floors a percentage for display, after clamping it with
// [ClampPercent].
//
// Flooring, never rounding: the web UI floors its percentages, so a value
// of 35.9 must read 35 — rounding half-up is what would show a point above
// the site.
func PercentFloor(value float64) (int, bool) {
	clamped, ok := ClampPercent(value)
	if !ok {
		return 0, false
	}
	return int(math.Floor(clamped)), true
}

// PercentRound rounds a percentage to the nearest whole number, after
// clamping it with [ClampPercent].
//
// Rounding, not flooring, and the difference from [PercentFloor] is
// deliberate. A window percentage is floored because the web UI floors it.
// The credits utilisation figure is specified as rounded; whether the site
// floors it too has not been observed, so the two helpers sit side by side
// and a later edit has to choose one rather than inherit whichever it
// happens to import.
func PercentRound(value float64) (int, bool) {
	clamped, ok := ClampPercent(value)
	if !ok {
		return 0, false
	}
	return int(math.Round(clamped)), true
}

// RenderCountdown formats the time until target as a compact countdown.
//
// It renders "3d4h", "2h13m", "45m", "30s", and "now" for a reset that has
// already passed — a stale window rolling over between the fetch and the
// render is ordinary, not an error worth a negative duration. The
// subtraction saturates at the duration type's limits, so extreme
// timestamps render a countdown rather than wrapping into a confidently
// wrong one.
func RenderCountdown(now, target time.Time) string {
	remaining := target.Sub(now)
	if remaining <= 0 {
		return "now"
	}

	seconds := int64(remaining / time.Second)
	minutes := seconds / 60
	hours := minutes / 60
	days := hours / 24

	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours%24)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes%60)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// equalASCIIFold reports whether a and b are equal ignoring ASCII case
// alone. Non-ASCII bytes compare exactly, so a display name is matched the
// way the fixed column headings expect without folding letters the server
// never varies.
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
