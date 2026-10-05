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
	"math"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// ts parses an RFC 3339 literal, or fails the test naming the literal.
func ts(t *testing.T, text string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("test literal %q should be a valid RFC 3339 timestamp: %v", text, err)
	}
	return at
}

// window builds a LimitWindow the way the parser does: both percentage
// fields from one input, each absent exactly when the input is not a
// number.
func window(t *testing.T, kind WindowKind, percent float64, resetsAt string) LimitWindow {
	t.Helper()
	w := LimitWindow{Kind: kind}
	if clamped, ok := ClampPercent(percent); ok {
		w.Percent = &clamped
	}
	if floored, ok := PercentFloor(percent); ok {
		w.PercentFloor = &floored
	}
	if resetsAt != "" {
		w.ResetsAt = ts(t, resetsAt)
	}
	return w
}

func TestPercentFloor(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input  float64
		want   int
		wantOK bool
	}{
		"success: below range clamps to zero":        {input: -1.0, want: 0, wantOK: true},
		"success: just above hundred clamps":         {input: 100.4, want: 100, wantOK: true},
		"success: far above hundred clamps":          {input: 250.0, want: 100, wantOK: true},
		"success: fraction floors, never rounds up":  {input: 35.9, want: 35, wantOK: true},
		"success: just below one floors to zero":     {input: 0.999, want: 0, wantOK: true},
		"success: just below hundred stays below it": {input: 99.999, want: 99, wantOK: true},
		"error: NaN is absence, not zero":            {input: math.NaN(), want: 0, wantOK: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := PercentFloor(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("PercentFloor(%v) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("PercentFloor(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestPercentRound(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input  float64
		want   int
		wantOK bool
	}{
		"success: below range clamps to zero": {input: -1.0, want: 0, wantOK: true},
		"success: exact zero stays zero":      {input: 0.0, want: 0, wantOK: true},
		"success: small fraction rounds down": {input: 0.4, want: 0, wantOK: true},
		"success: long fraction rounds down":  {input: 4.3911999999999995, want: 4, wantOK: true},
		"success: half rounds up":             {input: 99.5, want: 100, wantOK: true},
		"success: just above hundred clamps":  {input: 100.4, want: 100, wantOK: true},
		"success: far above hundred clamps":   {input: 250.0, want: 100, wantOK: true},
		"error: NaN is absence, not zero":     {input: math.NaN(), want: 0, wantOK: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := PercentRound(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("PercentRound(%v) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("PercentRound(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestPercentFloorAndRoundDisagreeWhereItMatters(t *testing.T) {
	t.Parallel()

	// The two exist side by side on purpose: a window percentage is
	// floored so the table never reads a point above the web UI, while the
	// credits utilisation figure is rounded.
	floored, ok := PercentFloor(35.9)
	if !ok || floored != 35 {
		t.Errorf("PercentFloor(35.9) = %d, %v; want 35, true", floored, ok)
	}
	rounded, ok := PercentRound(35.9)
	if !ok || rounded != 36 {
		t.Errorf("PercentRound(35.9) = %d, %v; want 36, true", rounded, ok)
	}
}

func TestClampPercent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input  float64
		want   float64
		wantOK bool
	}{
		"success: keeps the unrounded value":    {input: 4.3911999999999995, want: 4.3911999999999995, wantOK: true},
		"success: negative clamps to zero":      {input: -3.0, want: 0.0, wantOK: true},
		"success: positive infinity clamps":     {input: math.Inf(1), want: 100.0, wantOK: true},
		"success: negative infinity clamps":     {input: math.Inf(-1), want: 0.0, wantOK: true},
		"error: NaN is rejected, never clamped": {input: math.NaN(), want: 0, wantOK: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := ClampPercent(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ClampPercent(%v) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ClampPercent(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestMissingPercentIsNotZero(t *testing.T) {
	t.Parallel()

	// A window the server never measured must stay distinguishable from a
	// window at 0%: the renderer prints an em dash for one and "0%" for
	// the other.
	missing := window(t, WindowKind{Class: WindowSession}, math.NaN(), "")
	zero := window(t, WindowKind{Class: WindowSession}, 0.0, "")

	if missing.Percent != nil || missing.PercentFloor != nil {
		t.Errorf("a NaN percentage must leave both fields nil, got Percent=%v PercentFloor=%v", missing.Percent, missing.PercentFloor)
	}
	if zero.Percent == nil || zero.PercentFloor == nil {
		t.Fatalf("a measured 0%% must set both fields, got Percent=%v PercentFloor=%v", zero.Percent, zero.PercentFloor)
	}
	if *zero.Percent != 0 || *zero.PercentFloor != 0 {
		t.Errorf("a measured 0%% must read 0, got Percent=%v PercentFloor=%d", *zero.Percent, *zero.PercentFloor)
	}
	if diff := gocmp.Diff(missing, zero); diff == "" {
		t.Error("a missing percentage and a measured zero compared equal; they are different facts")
	}
}

func TestMoneyString(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		money Money
		want  string
	}{
		"success: USD renders with a symbol":        {money: Money{AmountMinor: 1234, Currency: "USD", Exponent: 2}, want: "$12.34"},
		"success: large USD amount":                 {money: Money{AmountMinor: 500_000, Currency: "USD", Exponent: 2}, want: "$5000.00"},
		"success: other currencies render the code": {money: Money{AmountMinor: 1234, Currency: "EUR", Exponent: 2}, want: "EUR 12.34"},
		"success: zero-decimal currency":            {money: Money{AmountMinor: 1234, Currency: "JPY", Exponent: 0}, want: "JPY 1234"},
		"success: observed credits figure":          {money: Money{AmountMinor: 21956, Currency: "USD", Exponent: 2}, want: "$219.56"},
		"success: zero exponent, no decimal point":  {money: Money{AmountMinor: 1234, Currency: "USD", Exponent: 0}, want: "$1234"},
		"success: negative at zero exponent":        {money: Money{AmountMinor: -1234, Currency: "USD", Exponent: 0}, want: "-$1234"},
		"success: negative at two decimals":         {money: Money{AmountMinor: -1234, Currency: "USD", Exponent: 2}, want: "-$12.34"},
		"success: three decimals":                   {money: Money{AmountMinor: 1234, Currency: "USD", Exponent: 3}, want: "$1.234"},
		"success: negative at three decimals":       {money: Money{AmountMinor: -1234, Currency: "USD", Exponent: 3}, want: "-$1.234"},
		"success: largest honoured exponent":        {money: Money{AmountMinor: 1234, Currency: "USD", Exponent: MaxMoneyExponent}, want: "$0.001234"},
		"success: negative at the largest exponent": {money: Money{AmountMinor: -1234, Currency: "USD", Exponent: MaxMoneyExponent}, want: "-$0.001234"},
		"success: fraction pads to the exponent":    {money: Money{AmountMinor: 5, Currency: "USD", Exponent: 3}, want: "$0.005"},
		"success: negative fraction pads too":       {money: Money{AmountMinor: -5, Currency: "USD", Exponent: 2}, want: "-$0.05"},
		"success: empty currency renders bare":      {money: Money{AmountMinor: 1234, Currency: "", Exponent: 2}, want: "12.34"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tt.money.String(); got != tt.want {
				t.Errorf("Money%+v.String() = %q, want %q", tt.money, got, tt.want)
			}
		})
	}
}

func TestMoneyStringNeverFailsOnTheEdges(t *testing.T) {
	t.Parallel()

	// Every exponent the endpoint could send plus two out-of-range ones,
	// and negatives at each. math.MinInt64 is the amount that would trip a
	// naive signed negation, because its magnitude does not fit back into
	// an int64.
	for _, exponent := range []uint8{0, 2, 3, 6, 7, math.MaxUint8} {
		for _, amount := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64} {
			money := Money{AmountMinor: amount, Currency: "USD", Exponent: exponent}
			rendered := money.String()
			if rendered == "" {
				t.Errorf("exponent %d, amount %d: rendered empty", exponent, amount)
			}
			wantSign := amount < 0
			gotSign := len(rendered) > 0 && rendered[0] == '-'
			if gotSign != wantSign {
				t.Errorf("exponent %d, amount %d: sign prefix = %v, want %v (%q)", exponent, amount, gotSign, wantSign, rendered)
			}
		}
	}
}

func TestRenderCountdown(t *testing.T) {
	t.Parallel()

	now := ts(t, "2026-09-08T00:00:00Z")
	tests := map[string]struct {
		target string
		want   string
	}{
		"success: days with trailing hours":    {target: "2026-09-11T04:30:00Z", want: "3d4h"},
		"success: hours with trailing minutes": {target: "2026-09-08T02:13:40Z", want: "2h13m"},
		"success: minutes alone":               {target: "2026-09-08T00:45:00Z", want: "45m"},
		"success: seconds alone":               {target: "2026-09-08T00:00:30Z", want: "30s"},
		"success: the same instant is now":     {target: "2026-09-08T00:00:00Z", want: "now"},
		"success: a past reset is now":         {target: "2026-09-07T00:00:00Z", want: "now"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := RenderCountdown(now, ts(t, tt.target)); got != tt.want {
				t.Errorf("RenderCountdown(now, %s) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestRenderCountdownSurvivesTheExtremes(t *testing.T) {
	t.Parallel()

	// time.Time.Sub saturates instead of wrapping, so two extreme
	// timestamps must render a countdown, never a wrapped nonsense value.
	earliest := time.Time{}
	latest := time.Unix(math.MaxInt64/int64(time.Second), 0)
	if got := RenderCountdown(latest, earliest); got != "now" {
		t.Errorf("RenderCountdown(latest, earliest) = %q, want %q", got, "now")
	}
	if got := RenderCountdown(earliest, latest); got == "" {
		t.Error("RenderCountdown(earliest, latest) rendered empty")
	}
}

func TestUsageSnapshotSelectsWindowsByKindAndScope(t *testing.T) {
	t.Parallel()

	snapshot := UsageSnapshot{
		FetchedAt: ts(t, "2026-09-08T00:00:00Z"),
		Windows: []LimitWindow{
			window(t, WindowKind{Class: WindowSession}, 21.0, "2026-09-08T03:30:00Z"),
			window(t, WindowKind{Class: WindowWeeklyAll}, 35.0, "2026-09-10T20:00:00Z"),
			window(t, WindowKind{Class: WindowWeeklyScoped, Name: "Fable"}, 56.0, "2026-09-10T20:00:00Z"),
			window(t, WindowKind{Class: WindowWeeklyScoped, Name: "opus"}, 12.0, ""),
			window(t, WindowKind{Class: WindowUnknown, Name: "monthly_foo"}, 7.0, ""),
		},
		Credits: CreditsState{Class: CreditsUnavailable},
	}

	session := snapshot.Window(WindowKind{Class: WindowSession})
	if session == nil || session.PercentFloor == nil || *session.PercentFloor != 21 {
		t.Errorf("Window(session) floor = %v, want 21", session)
	}
	scoped := snapshot.ScopedWindow("fable")
	if scoped == nil || scoped.PercentFloor == nil || *scoped.PercentFloor != 56 {
		t.Errorf("ScopedWindow(fable) floor = %v, want 56", scoped)
	}
	if got := snapshot.ScopedWindow("haiku"); got != nil {
		t.Errorf("ScopedWindow(haiku) = %+v, want nil", got)
	}

	var labels []string
	for _, extra := range snapshot.ExtraWindows("Fable") {
		labels = append(labels, extra.Label())
	}
	wantLabels := []string{"opus (weekly)", "monthly_foo (unknown kind)"}
	if diff := gocmp.Diff(wantLabels, labels); diff != "" {
		t.Errorf("ExtraWindows labels mismatch (-want +got):\n%s", diff)
	}

	next, ok := snapshot.NextReset()
	if !ok {
		t.Fatal("NextReset() reported no reset; the session window carries one")
	}
	if want := ts(t, "2026-09-08T03:30:00Z"); !next.Equal(want) {
		t.Errorf("NextReset() = %v, want %v", next, want)
	}
}

func TestNextResetAbsentWhenNoWindowResets(t *testing.T) {
	t.Parallel()

	snapshot := UsageSnapshot{
		FetchedAt: ts(t, "2026-09-08T00:00:00Z"),
		Windows:   []LimitWindow{window(t, WindowKind{Class: WindowSession}, 0.0, "")},
		Credits:   CreditsState{Class: CreditsUnavailable},
	}
	if next, ok := snapshot.NextReset(); ok {
		t.Errorf("NextReset() = %v, true; want absent", next)
	}
}

func TestWindowLabels(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		kind WindowKind
		want string
	}{
		"success: session window":                {kind: WindowKind{Class: WindowSession}, want: "session"},
		"success: all-model weekly window":       {kind: WindowKind{Class: WindowWeeklyAll}, want: "weekly"},
		"success: scoped weekly names its scope": {kind: WindowKind{Class: WindowWeeklyScoped, Name: "opus"}, want: "opus (weekly)"},
		"success: unknown kind is kept verbatim": {kind: WindowKind{Class: WindowUnknown, Name: "monthly_foo"}, want: "monthly_foo (unknown kind)"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := window(t, tt.kind, 7.0, "")
			if got := w.Label(); got != tt.want {
				t.Errorf("Label() = %q, want %q", got, tt.want)
			}
		})
	}
}
