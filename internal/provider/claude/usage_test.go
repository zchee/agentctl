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

package claude

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/fixtures"
	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/usage"
)

// fixture reads one usage fixture body.
func fixture(t *testing.T, name string) jsontext.Value {
	t.Helper()
	data, err := fixtures.FS.ReadFile("claude/" + name)
	if err != nil {
		t.Fatalf("the fixture %q should be embedded: %v", name, err)
	}
	return jsontext.Value(data)
}

func usageTS(t *testing.T, text string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("test literal %q should be a valid RFC 3339 timestamp: %v", text, err)
	}
	return at
}

// parsedFixture parses one fixture into a snapshot.
func parsedFixture(t *testing.T, name string, keepRaw bool) *usage.UsageSnapshot {
	t.Helper()
	snapshot, err := ParseUsage(fixture(t, name), usageTS(t, "2026-09-08T00:00:00Z"), keepRaw)
	if err != nil {
		t.Fatalf("every fixture body is a JSON object: %v", err)
	}
	return snapshot
}

// bearerAuth authenticates with a sealed token, opened only around
// building the one header value, the way a production credential does.
type bearerAuth struct {
	token *secret.Secret
}

func (a bearerAuth) AuthorizationHeader() string {
	var header string
	if err := a.token.WithPlaintext(func(b []byte) error {
		header = "Bearer " + string(b)
		return nil
	}); err != nil {
		return ""
	}
	return header
}

func (bearerAuth) ExtraHeaders() []provider.Header { return nil }

// accountWithToken builds an account whose access token is the given
// string.
func accountWithToken(t *testing.T, token string) provider.AccountRef {
	t.Helper()
	sealed, err := secret.NewSecret([]byte(token))
	if err != nil {
		t.Fatalf("the test token seals: %v", err)
	}
	return provider.AccountRef{ID: "acct", Auth: bearerAuth{token: sealed}}
}

func testClient(server *httptest.Server) *UsageClient {
	return NewUsageClient(server.URL, "agentctl/test", 5*time.Second)
}

func TestTheCapturedFixtureYieldsExactlyThreeWindows(t *testing.T) {
	t.Parallel()

	snapshot := parsedFixture(t, "usage-2026-09-08.json", false)
	if len(snapshot.Windows) != 3 {
		t.Fatalf("the capture describes three windows, got %d", len(snapshot.Windows))
	}

	wantKinds := []usage.WindowKind{
		{Class: usage.WindowSession},
		{Class: usage.WindowWeeklyAll},
		{Class: usage.WindowWeeklyScoped, Name: "Fable"},
	}
	wantFloors := []int{21, 35, 56}
	wantActive := []bool{false, false, true}
	for i, window := range snapshot.Windows {
		if window.Kind != wantKinds[i] {
			t.Errorf("window %d kind = %+v, want %+v", i, window.Kind, wantKinds[i])
		}
		if window.PercentFloor == nil || *window.PercentFloor != wantFloors[i] {
			t.Errorf("window %d floor = %v, want %d", i, window.PercentFloor, wantFloors[i])
		}
		if window.IsActive != wantActive[i] {
			t.Errorf("window %d active = %v, want %v; only the scoped weekly is active", i, window.IsActive, wantActive[i])
		}
	}

	wantReset, err := time.Parse(time.RFC3339, "2026-09-08T03:29:59.817079+00:00")
	if err != nil {
		t.Fatalf("the capture's timestamp parses: %v", err)
	}
	if !snapshot.Windows[0].ResetsAt.Equal(wantReset) {
		t.Errorf("resets_at = %v, want %v; sub-second precision must survive", snapshot.Windows[0].ResetsAt, wantReset)
	}
	if got := snapshot.Windows[2].ScopeLabel; got != "Fable" {
		t.Errorf("scope label = %q, want %q", got, "Fable")
	}
}

func TestTheFlatKeysAreIgnoredWhenLimitsIsPresent(t *testing.T) {
	t.Parallel()

	// The capture carries nimbus_quill with utilization 0.0 alongside
	// limits[]. Reading both shapes would invent a fourth window.
	snapshot := parsedFixture(t, "usage-2026-09-08.json", false)
	for _, window := range snapshot.Windows {
		if window.Kind.Class == usage.WindowUnknown && window.Kind.Name == "nimbus_quill" {
			t.Error("an unrecognised flat key must not become a window while limits[] exists")
		}
	}
}

func TestTheLegacyKeysMapWhenLimitsIsEmpty(t *testing.T) {
	t.Parallel()

	snapshot := parsedFixture(t, "usage-legacy-only.json", false)

	var kinds []usage.WindowKind
	var floors []int
	for _, window := range snapshot.Windows {
		kinds = append(kinds, window.Kind)
		if window.PercentFloor != nil {
			floors = append(floors, *window.PercentFloor)
		}
		if window.IsActive {
			t.Error("the flat shape predates is_active, so nothing may claim to be active")
		}
	}
	wantKinds := []usage.WindowKind{
		{Class: usage.WindowSession},
		{Class: usage.WindowWeeklyAll},
		{Class: usage.WindowWeeklyScoped, Name: "opus"},
	}
	if diff := gocmp.Diff(wantKinds, kinds); diff != "" {
		t.Errorf("five_hour, seven_day and seven_day_opus, in that order (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff([]int{21, 35, 56}, floors); diff != "" {
		t.Errorf("floors mismatch (-want +got):\n%s", diff)
	}
	if got := snapshot.Windows[2].ScopeLabel; got != "opus" {
		t.Errorf("scope label = %q, want %q", got, "opus")
	}
}

func TestNullLegacyWindowsAreDroppedNotRenderedAsZero(t *testing.T) {
	t.Parallel()

	// seven_day_sonnet: null means "this account has no such window", not
	// "this account is at 0%".
	snapshot := parsedFixture(t, "usage-legacy-only.json", false)
	for _, window := range snapshot.Windows {
		if window.Kind == (usage.WindowKind{Class: usage.WindowWeeklyScoped, Name: "sonnet"}) {
			t.Error("a null legacy window must not appear")
		}
	}
}

func TestAnUnrecognisedKindBecomesAVisibleUnknownWindow(t *testing.T) {
	t.Parallel()

	snapshot := parsedFixture(t, "usage-unknown-kind.json", false)
	if len(snapshot.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(snapshot.Windows))
	}
	var unknown *usage.LimitWindow
	for i := range snapshot.Windows {
		if snapshot.Windows[i].Kind.Class == usage.WindowUnknown {
			unknown = &snapshot.Windows[i]
		}
	}
	if unknown == nil {
		t.Fatal("the monthly_foo entry must survive normalization")
	}
	if unknown.Kind.Name != "monthly_foo" {
		t.Errorf("kind = %q, want %q", unknown.Kind.Name, "monthly_foo")
	}
	if unknown.PercentFloor == nil || *unknown.PercentFloor != 7 {
		t.Errorf("floor = %v, want 7", unknown.PercentFloor)
	}
	if !unknown.IsActive {
		t.Error("the unknown window is the active one in this fixture")
	}
	if got := unknown.Label(); got != "monthly_foo (unknown kind)" {
		t.Errorf("label = %q", got)
	}
}

func TestABodyWithNoWindowAnywhereParsesToZeroWindows(t *testing.T) {
	t.Parallel()

	// Not an error: the caller turns this into the "no subscription
	// limits" state.
	snapshot := parsedFixture(t, "usage-empty-limits.json", false)
	if len(snapshot.Windows) != 0 {
		t.Errorf("windows = %d, want 0", len(snapshot.Windows))
	}
	if next, ok := snapshot.NextReset(); ok {
		t.Errorf("next reset = %v, want none", next)
	}
	// The body still carries extra_usage, switched off: an account with
	// no subscription window can still have credits, and the two are
	// separate facts about it.
	want := usage.CreditsState{Class: usage.CreditsOff}
	if diff := gocmp.Diff(want, snapshot.Credits); diff != "" {
		t.Errorf("credits mismatch (-want +got):\n%s", diff)
	}
}

func TestCreditsFromTheFixtureBodies(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name         string
		wantState    usage.CreditsState
		wantWarnings []CreditsWarning
	}{
		// The account the capture came from has never enabled credits, so
		// is_enabled is false and every figure beside it is null. "off"
		// and "n/a" are different answers and this body deserves the
		// first; its spend reports nothing and must not turn a resolved
		// off into a warning.
		"success: the live capture reports credits switched off": {
			name:      "usage-2026-09-08.json",
			wantState: usage.CreditsState{Class: usage.CreditsOff},
		},
		// The one real body with credits on: monthly_limit 500000,
		// used_credits 21956.0, utilization 4.3911999999999995,
		// decimal_places 2. The cell this becomes is
		// "$219.56 / $5000.00 (4%)"; utilization 4.39 and spend.percent 4
		// agree to a point, so nothing is warned about.
		"success: the credits-enabled capture maps to the figures the site shows": {
			name: "usage-extra-usage-enabled.json",
			wantState: usage.CreditsState{Class: usage.CreditsOn, Credits: usage.Credits{
				Used:    &usage.Money{AmountMinor: 21_956, Currency: "USD", Exponent: 2},
				Limit:   &usage.Money{AmountMinor: 500_000, Currency: "USD", Exponent: 2},
				Percent: new(4),
			}},
		},
		"success: an account with no ceiling keeps its used figure": {
			name: "usage-extra-usage-unlimited.json",
			wantState: usage.CreditsState{Class: usage.CreditsOn, Credits: usage.Credits{
				Used: &usage.Money{AmountMinor: 1234, Currency: "USD", Exponent: 2},
			}},
		},
		// Not unavailable: the account has credits. The renderer is what
		// decides an unmeasured spend shows as an em dash.
		"success: credits on with no used figure is on and unmeasured": {
			name: "usage-extra-usage-unmeasured.json",
			wantState: usage.CreditsState{Class: usage.CreditsOn, Credits: usage.Credits{
				Limit: &usage.Money{AmountMinor: 500_000, Currency: "USD", Exponent: 2},
			}},
		},
		// The under-delivery case: the column will read "n/a" while the
		// web UI shows a figure. The warning is the only thing that says
		// so.
		"success: spend reporting credits that extra_usage omits warns once": {
			name:         "usage-spend-without-extra-usage.json",
			wantState:    usage.CreditsState{Class: usage.CreditsUnavailable},
			wantWarnings: []CreditsWarning{{Kind: WarnSpendWithoutExtraUsage}},
		},
		"success: a body with neither credits object is unavailable and silent": {
			name:      "usage-no-credits.json",
			wantState: usage.CreditsState{Class: usage.CreditsUnavailable},
		},
		"success: the unknown-kind body is unavailable and silent too": {
			name:      "usage-unknown-kind.json",
			wantState: usage.CreditsState{Class: usage.CreditsUnavailable},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			state, warnings := CreditsFromBody(fixture(t, tt.name))
			if diff := gocmp.Diff(tt.wantState, state); diff != "" {
				t.Errorf("state mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantWarnings, warnings); diff != "" {
				t.Errorf("warnings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUsedCreditsArrivesAsAFloatAndMonthlyLimitAsAnInteger(t *testing.T) {
	t.Parallel()

	// Both spellings appear in the same real object, so a parser that
	// read only exact integers would drop the figure the column exists to
	// show.
	body := fixture(t, "usage-extra-usage-enabled.json")
	if !bytes.Contains(body, []byte(`"used_credits": 21956.0`)) {
		t.Error("the capture sends used_credits as 21956.0")
	}
	if !bytes.Contains(body, []byte(`"monthly_limit": 500000`)) {
		t.Error("and monthly_limit as 500000")
	}
}

func TestASpendObjectOfOnlyProseAndZeroesIsNotAWithheldFigure(t *testing.T) {
	t.Parallel()

	// Every response carries a disclaimer and a severity. Warning on
	// those would fire on every account that simply predates extra_usage,
	// and a warning that always fires is one nobody reads.
	body := jsontext.Value(`{
		"spend": {
			"used": {"amount_minor": 0, "currency": "USD", "exponent": 2},
			"limit": null,
			"percent": 0,
			"severity": "normal",
			"enabled": false,
			"disclaimer": "Usage credits cover you when you hit your plan limits.",
			"can_purchase_credits": false
		}
	}`)
	state, warnings := CreditsFromBody(body)
	if state.Class != usage.CreditsUnavailable {
		t.Errorf("state = %+v, want unavailable", state)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
}

func TestEachThingSpendCanSayAboutALiveBalanceIsDetected(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"success: an enabled switch": `{"enabled": true}`,
		"success: a ceiling":         `{"limit": {"amount_minor": 5000}}`,
		"success: a balance":         `{"balance": {"amount_minor": 100}}`,
		"success: a cap":             `{"cap": {"credits": {"amount_minor": 500000, "exponent": 2}}}`,
		"success: a percentage":      `{"percent": 25}`,
		"success: an amount":         `{"used": {"amount_minor": 12500}}`,
	}
	for name, spend := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			state, warnings := CreditsFromBody(jsontext.Value(`{"spend": ` + spend + `}`))
			if state.Class != usage.CreditsUnavailable {
				t.Errorf("state = %+v, want unavailable", state)
			}
			want := []CreditsWarning{{Kind: WarnSpendWithoutExtraUsage}}
			if diff := gocmp.Diff(want, warnings); diff != "" {
				t.Errorf("warnings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAPercentageThatDisagreesWithSpendByMoreThanAPointWarns(t *testing.T) {
	t.Parallel()

	body := jsontext.Value(`{
		"extra_usage": {"is_enabled": true, "utilization": 25.0, "used_credits": 1234},
		"spend": {"percent": 90}
	}`)
	state, warnings := CreditsFromBody(body)
	want := []CreditsWarning{{Kind: WarnPercentDisagrees, ExtraUsage: 25.0, Spend: 90.0}}
	if diff := gocmp.Diff(want, warnings); diff != "" {
		t.Errorf("warnings mismatch (-want +got):\n%s", diff)
	}
	// The warning does not change the answer: extra_usage is still the
	// source, so the cell shows 25% and --raw shows the disagreement.
	if state.Class != usage.CreditsOn || state.Credits.Percent == nil || *state.Credits.Percent != 25 {
		t.Errorf("state = %+v, want credits on at 25%%", state)
	}

	// Exactly a point apart is agreement; the tolerance is inclusive.
	close := jsontext.Value(`{
		"extra_usage": {"is_enabled": true, "utilization": 25.0},
		"spend": {"percent": 26}
	}`)
	if _, warnings := CreditsFromBody(close); len(warnings) != 0 {
		t.Errorf("a one-point gap warned: %+v", warnings)
	}
}

func TestDisabledCreditsCarryTheServersOwnReason(t *testing.T) {
	t.Parallel()

	state, warnings := CreditsFromBody(jsontext.Value(`{"extra_usage": {"is_enabled": false, "disabled_reason": "past_due"}}`))
	want := usage.CreditsState{Class: usage.CreditsOff, DisabledReason: "past_due"}
	if diff := gocmp.Diff(want, state); diff != "" {
		t.Errorf("state mismatch (-want +got):\n%s", diff)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
}

func TestADecimalPlacesTheCurrencyCannotHaveFallsBackToTwoAndWarns(t *testing.T) {
	t.Parallel()

	for _, places := range []string{"-1", "7", "99"} {
		body := jsontext.Value(`{"extra_usage": {"is_enabled": true, "used_credits": 1234, "currency": "USD", "decimal_places": ` + places + `}}`)
		state, warnings := CreditsFromBody(body)
		if len(warnings) != 1 || warnings[0].Kind != WarnExponentOutOfRange {
			t.Errorf("decimal_places %s: warnings = %+v, want one out-of-range warning", places, warnings)
		}
		if state.Class != usage.CreditsOn {
			t.Fatalf("decimal_places %s: credits are on", places)
		}
		if diff := gocmp.Diff(&usage.Money{AmountMinor: 1234, Currency: "USD", Exponent: 2}, state.Credits.Used); diff != "" {
			t.Errorf("decimal_places %s: the figure is still shown, at two places (-want +got):\n%s", places, diff)
		}
	}
}

func TestDecimalPlacesZeroAndThreeAreHonoured(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		places   string
		currency string
		want     string
		exponent uint8
	}{
		"success: a zero-decimal currency":  {places: "0", currency: "JPY", want: "JPY 1234", exponent: 0},
		"success: a three-decimal currency": {places: "3", currency: "BHD", want: "BHD 1.234", exponent: 3},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := jsontext.Value(`{"extra_usage": {"is_enabled": true, "used_credits": 1234, "currency": "` + tt.currency + `", "decimal_places": ` + tt.places + `}}`)
			state, warnings := CreditsFromBody(body)
			if len(warnings) != 0 {
				t.Errorf("warnings = %+v, want none", warnings)
			}
			if state.Class != usage.CreditsOn || state.Credits.Used == nil {
				t.Fatalf("state = %+v, want credits on with a used figure", state)
			}
			if state.Credits.Used.Exponent != tt.exponent {
				t.Errorf("exponent = %d, want %d", state.Credits.Used.Exponent, tt.exponent)
			}
			if got := state.Credits.Used.String(); got != tt.want {
				t.Errorf("rendering = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestANegativeBalanceIsARealStateAndIsShownAsOne(t *testing.T) {
	t.Parallel()

	// A refunded account has spent a negative amount. Clamping it to zero
	// would hide a credit the user actually holds.
	body := jsontext.Value(`{"extra_usage": {"is_enabled": true, "used_credits": -500, "currency": "USD", "decimal_places": 2}}`)
	state, warnings := CreditsFromBody(body)
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
	if state.Class != usage.CreditsOn {
		t.Fatal("credits are on")
	}
	if diff := gocmp.Diff(&usage.Money{AmountMinor: -500, Currency: "USD", Exponent: 2}, state.Credits.Used); diff != "" {
		t.Errorf("used mismatch (-want +got):\n%s", diff)
	}
	if got := state.Credits.Used.String(); got != "-$5.00" {
		t.Errorf("rendering = %q, want %q", got, "-$5.00")
	}
}

func TestUtilizationIsTakenAsAPercentageAndNeverMultiplied(t *testing.T) {
	t.Parallel()

	// extra_usage.utilization is already 0-100. A build that treated it
	// as a fraction would show 439% for the captured account.
	tests := map[string]struct {
		utilization string
		want        *int
	}{
		"success: below range clamps to zero": {utilization: "-1", want: new(0)},
		"success: exact zero":                 {utilization: "0", want: new(0)},
		"success: the captured long fraction": {utilization: "4.3911999999999995", want: new(4)},
		"success: half rounds up":             {utilization: "99.5", want: new(100)},
		"success: just above hundred clamps":  {utilization: "100.4", want: new(100)},
		"success: far above hundred clamps":   {utilization: "250", want: new(100)},
		"success: null is absence, not zero":  {utilization: "null", want: nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := jsontext.Value(`{"extra_usage": {"is_enabled": true, "utilization": ` + tt.utilization + `}}`)
			state, _ := CreditsFromBody(body)
			if state.Class != usage.CreditsOn {
				t.Fatal("credits are on")
			}
			if diff := gocmp.Diff(tt.want, state.Credits.Percent); diff != "" {
				t.Errorf("percent mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestACurrencyTheServerDidNotSendIsNotInventedAsDollars(t *testing.T) {
	t.Parallel()

	body := jsontext.Value(`{"extra_usage": {"is_enabled": true, "used_credits": 1234, "currency": null}}`)
	state, _ := CreditsFromBody(body)
	if state.Class != usage.CreditsOn || state.Credits.Used == nil {
		t.Fatal("credits are on with a used figure")
	}
	if state.Credits.Used.Currency != "" {
		t.Errorf("currency = %q, want empty", state.Credits.Used.Currency)
	}
	if got := state.Credits.Used.String(); got != "12.34" {
		t.Errorf("rendering = %q, want a bare figure, not a dollar sign", got)
	}
}

func TestAnUnreadableMoneyFieldIsAbsentRatherThanSaturated(t *testing.T) {
	t.Parallel()

	// A silent saturation would put a confident maximum into a money
	// column, which is worse than an em dash.
	for _, used := range []string{`"many"`, "1e300", "-1e300", "{}", "null"} {
		body := jsontext.Value(`{"extra_usage": {"is_enabled": true, "used_credits": ` + used + `}}`)
		state, _ := CreditsFromBody(body)
		if state.Class != usage.CreditsOn {
			t.Fatalf("used_credits %s: credits are on", used)
		}
		if state.Credits.Used != nil {
			t.Errorf("used_credits %s: used = %+v, want nil", used, state.Credits.Used)
		}
	}
}

func TestAnExtraUsageWithoutTheDeclaredIsEnabledFlagIsUnavailable(t *testing.T) {
	t.Parallel()

	// is_enabled is a required boolean in the declared shape. An object
	// that lacks it (or carries a non-boolean) is not that shape, and a
	// figure must not be invented from it.
	for _, body := range []string{
		`{"extra_usage": {}}`,
		`{"extra_usage": {"is_enabled": "yes", "used_credits": 100}}`,
		`{"extra_usage": {"is_enabled": null, "monthly_limit": 500000}}`,
	} {
		state, warnings := CreditsFromBody(jsontext.Value(body))
		if state.Class != usage.CreditsUnavailable {
			t.Errorf("body %s: state = %+v, want unavailable", body, state)
		}
		if len(warnings) != 0 {
			t.Errorf("body %s: no spend, so nothing to warn about: %+v", body, warnings)
		}
	}

	state, warnings := CreditsFromBody(jsontext.Value(`{"extra_usage": {}, "spend": {"enabled": true, "percent": 25}}`))
	if state.Class != usage.CreditsUnavailable {
		t.Errorf("state = %+v, want unavailable", state)
	}
	want := []CreditsWarning{{Kind: WarnSpendWithoutExtraUsage}}
	if diff := gocmp.Diff(want, warnings); diff != "" {
		t.Errorf("warnings mismatch (-want +got):\n%s", diff)
	}
}

func TestABodyThatIsNotAnObjectHasNoCreditsAndNoWarning(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`[1, 2, 3]`, `null`} {
		state, warnings := CreditsFromBody(jsontext.Value(body))
		if state.Class != usage.CreditsUnavailable || len(warnings) != 0 {
			t.Errorf("body %s: state = %+v warnings = %+v, want unavailable and silent", body, state, warnings)
		}
	}
}

// capturedLogs runs body with the default logger swapped for one that
// keeps every line, and returns what was logged. The credits warnings are
// the only thing that tells a user their column may disagree with the web
// UI, so "the warning was emitted" has to be a claim about the log the
// process actually writes. Not parallel: the default logger is process
// state.
func capturedLogs(t *testing.T, body func()) string {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	body()
	return buffer.String()
}

func TestTheWarningReachesTheLogNamingRaw(t *testing.T) {
	logs := capturedLogs(t, func() {
		if _, err := ParseUsage(fixture(t, "usage-spend-without-extra-usage.json"), usageTS(t, "2026-09-08T00:00:00Z"), false); err != nil {
			t.Errorf("the fixture is a usage document: %v", err)
		}
	})

	if !strings.Contains(logs, "WARN") {
		t.Errorf("got:\n%s", logs)
	}
	if !strings.Contains(logs, "--raw") {
		t.Errorf("the message says where the figure survives: got:\n%s", logs)
	}
	if !strings.Contains(logs, "`spend` object reports credits") {
		t.Errorf("got:\n%s", logs)
	}
}

func TestABodyWithNothingToWarnAboutLogsNothing(t *testing.T) {
	logs := capturedLogs(t, func() {
		if _, err := ParseUsage(fixture(t, "usage-extra-usage-enabled.json"), usageTS(t, "2026-09-08T00:00:00Z"), false); err != nil {
			t.Errorf("a real capture is a usage document: %v", err)
		}
	})
	if logs != "" {
		t.Errorf("a healthy body is silent: got:\n%s", logs)
	}
}

func TestBothRealFixturesRoundTripThroughRawUntouched(t *testing.T) {
	t.Parallel()

	// The only typed thing that may come out of spend is the warnings;
	// everything else about it survives in --raw alone, byte for byte.
	for _, name := range []string{"usage-2026-09-08.json", "usage-extra-usage-enabled.json"} {
		original := fixture(t, name)
		snapshot, err := ParseUsage(original, usageTS(t, "2026-09-08T00:00:00Z"), true)
		if err != nil {
			t.Fatalf("%s: a real capture is a JSON object: %v", name, err)
		}
		if snapshot.Raw == nil {
			t.Fatalf("%s: --raw keeps the body", name)
		}
		if !bytes.Equal(snapshot.Raw, original) {
			t.Errorf("%s: the raw body must be byte-for-byte the response", name)
		}
		if !bytes.Contains(snapshot.Raw, []byte(`"spend"`)) {
			t.Errorf("%s: spend survives untouched", name)
		}
	}
}

func TestRawIsAbsentUnlessItWasAskedFor(t *testing.T) {
	t.Parallel()

	if snapshot := parsedFixture(t, "usage-2026-09-08.json", false); snapshot.Raw != nil {
		t.Error("raw must be nil when it was not asked for")
	}
}

func TestANonObjectBodyIsAParseFailure(t *testing.T) {
	t.Parallel()

	_, err := ParseUsage(jsontext.Value(`[1, 2, 3]`), usageTS(t, "2026-09-08T00:00:00Z"), false)
	if err == nil {
		t.Fatal("an array is not a usage document")
	}
	if !strings.Contains(err.Error(), "an array") {
		t.Errorf("the message should name what arrived: %v", err)
	}
}

func TestALimitsEntryWithoutAKindIsDropped(t *testing.T) {
	t.Parallel()

	windows := Normalize(jsontext.Value(`{"limits": [{"percent": 50}, {"kind": "session", "percent": 10}]}`))
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	if windows[0].Kind.Class != usage.WindowSession {
		t.Errorf("kind = %+v, want session", windows[0].Kind)
	}
}

func TestAScopedWindowWithoutADisplayNameStillAppears(t *testing.T) {
	t.Parallel()

	windows := Normalize(jsontext.Value(`{"limits": [{"kind": "weekly_scoped", "percent": 12, "scope": null}]}`))
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	want := usage.WindowKind{Class: usage.WindowWeeklyScoped, Name: "scoped"}
	if windows[0].Kind != want {
		t.Errorf("kind = %+v, want %+v", windows[0].Kind, want)
	}
	if windows[0].ScopeLabel != "" {
		t.Errorf("scope label = %q, want empty", windows[0].ScopeLabel)
	}
}

func TestALimitsArrayOfOnlyUnusableEntriesFallsBackToTheLegacyKeys(t *testing.T) {
	t.Parallel()

	// Otherwise an account served a malformed array would report "no
	// subscription limits" while its flat keys said 21%.
	windows := Normalize(jsontext.Value(`{
		"limits": [{"percent": 50}],
		"five_hour": {"utilization": 21.0, "resets_at": null}
	}`))
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	if windows[0].Kind.Class != usage.WindowSession {
		t.Errorf("kind = %+v, want session", windows[0].Kind)
	}
	if windows[0].PercentFloor == nil || *windows[0].PercentFloor != 21 {
		t.Errorf("floor = %v, want 21", windows[0].PercentFloor)
	}
}

func TestAnUnparseableResetsAtLeavesThePercentageIntact(t *testing.T) {
	t.Parallel()

	windows := Normalize(jsontext.Value(`{"limits": [{"kind": "session", "percent": 21, "resets_at": "tomorrow"}]}`))
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	if windows[0].PercentFloor == nil || *windows[0].PercentFloor != 21 {
		t.Errorf("floor = %v, want 21", windows[0].PercentFloor)
	}
	if !windows[0].ResetsAt.IsZero() {
		t.Errorf("resets_at = %v, want absent", windows[0].ResetsAt)
	}
}

func TestPercentAndFloorStayDistinctFromAbsence(t *testing.T) {
	t.Parallel()

	// A window whose percent is a non-number keeps both fields nil; a
	// measured zero sets both. The renderer depends on the difference.
	windows := Normalize(jsontext.Value(`{"limits": [
		{"kind": "session", "percent": 0},
		{"kind": "weekly_all", "percent": "unmeasured"}
	]}`))
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}
	if diff := gocmp.Diff(new(float64(0)), windows[0].Percent); diff != "" {
		t.Errorf("a measured zero must be kept (-want +got):\n%s", diff)
	}
	if windows[1].Percent != nil || windows[1].PercentFloor != nil {
		t.Errorf("a non-number percent must stay absent, got %v / %v", windows[1].Percent, windows[1].PercentFloor)
	}
}

func TestParseRetryAfterReadsBothFormsHTTPAllows(t *testing.T) {
	t.Parallel()

	now := usageTS(t, "2026-09-08T00:00:00Z")
	tests := map[string]struct {
		value   string
		want    time.Duration
		wantSet bool
	}{
		"success: a delay in seconds":            {value: "30", want: 30 * time.Second, wantSet: true},
		"success: outer whitespace is trimmed":   {value: "  30  ", want: 30 * time.Second, wantSet: true},
		"success: zero seconds is an answer":     {value: "0", want: 0, wantSet: true},
		"success: an IMF-fixdate in the future":  {value: "Tue, 08 Sep 2026 00:02:00 GMT", want: 2 * time.Minute, wantSet: true},
		"success: a date already past means now": {value: "Mon, 07 Sep 2026 00:00:00 GMT", want: 0, wantSet: true},
		"error: an empty value is no hint":       {value: "", wantSet: false},
		"error: prose is no hint":                {value: "soon", wantSet: false},
		"error: a negative delay is no hint":     {value: "-5", wantSet: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseRetryAfter(tt.value, now)
			if ok != tt.wantSet {
				t.Fatalf("ParseRetryAfter(%q) ok = %v, want %v", tt.value, ok, tt.wantSet)
			}
			if ok && got != tt.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestTheRequestCarriesExactlyTheHeadersTheEndpointNeeds(t *testing.T) {
	t.Parallel()

	captured := fixture(t, "usage-2026-09-08.json")
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != UsagePath {
			t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, UsagePath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-observed" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get(BetaHeader); got != BetaValue {
			t.Errorf("%s = %q, want %q", BetaHeader, got, BetaValue)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "agentctl/test" {
			t.Errorf("user-agent = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(captured)
	}))
	defer server.Close()

	snapshot, err := testClient(server).Fetch(t.Context(), accountWithToken(t, "sk-ant-oat01-observed"))
	if err != nil {
		t.Fatalf("a 200 with the captured body should parse: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if len(snapshot.Windows) != 3 {
		t.Errorf("windows = %d, want 3", len(snapshot.Windows))
	}
}

// fetchErr runs a fetch against a handler and requires it to fail.
func fetchErr(t *testing.T, handler http.HandlerFunc, token string) error {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	_, err := testClient(server).Fetch(t.Context(), accountWithToken(t, token))
	if err == nil {
		t.Fatal("the fetch should fail")
	}
	return err
}

func TestA401IsReportedAsUnauthorizedSoThePassCanRefreshOnce(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := fetchErr(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			// The body an expired access token has been observed to get.
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has expired. Re-authenticate to continue."},"request_id":null}`))
		}, "sk-ant-oat01-expired")

		fetchError, ok := errors.AsType[*provider.FetchError](err)
		if !ok {
			t.Fatalf("status %d: err = %T, want *provider.FetchError", status, err)
		}
		if fetchError.Kind != provider.FetchUnauthorized {
			t.Errorf("status %d: kind = %q, want unauthorized", status, fetchError.Kind)
		}
		if fetchError.IsTransient() {
			t.Errorf("status %d: a rejected token is not worth retrying unchanged", status)
		}
	}
}

func TestA429CarriesItsRetryHintThrough(t *testing.T) {
	t.Parallel()

	err := fetchErr(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("{}"))
	}, "sk-ant-oat01-limited")

	fetchError, ok := errors.AsType[*provider.FetchError](err)
	if !ok {
		t.Fatalf("err = %T, want *provider.FetchError", err)
	}
	if fetchError.Kind != provider.FetchRateLimited || !fetchError.HasRetryAfter || fetchError.RetryAfter != 30*time.Second {
		t.Errorf("err = %+v, want rate limited with a 30s hint", fetchError)
	}
	if !fetchError.IsTransient() {
		t.Error("a rate limit is worth another pass")
	}
}

func TestA429WithoutAHintIsStillARateLimit(t *testing.T) {
	t.Parallel()

	err := fetchErr(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("{}"))
	}, "sk-ant-oat01-limited")

	fetchError, ok := errors.AsType[*provider.FetchError](err)
	if !ok {
		t.Fatalf("err = %T, want *provider.FetchError", err)
	}
	if fetchError.Kind != provider.FetchRateLimited || fetchError.HasRetryAfter {
		t.Errorf("err = %+v, want rate limited without a hint", fetchError)
	}
}

func TestOtherStatusesAreReportedWithTheirCodeAndNoBody(t *testing.T) {
	t.Parallel()

	err := fetchErr(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream said sk-ant-should-never-be-echoed"))
	}, "sk-ant-oat01-ok")

	fetchError, ok := errors.AsType[*provider.FetchError](err)
	if !ok {
		t.Fatalf("err = %T, want *provider.FetchError", err)
	}
	if fetchError.Kind != provider.FetchHTTP || fetchError.Status != http.StatusServiceUnavailable {
		t.Errorf("err = %+v, want HTTP 503", fetchError)
	}
	if !fetchError.IsTransient() {
		t.Error("a 5xx is worth another pass")
	}
	if strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("a body must never reach the message: %v", err)
	}
}

func TestABodyThatIsNotAUsageDocumentIsAParseFailure(t *testing.T) {
	t.Parallel()

	err := fetchErr(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>captive portal</html>"))
	}, "sk-ant-oat01-ok")

	fetchError, ok := errors.AsType[*provider.FetchError](err)
	if !ok {
		t.Fatalf("err = %T, want *provider.FetchError", err)
	}
	if fetchError.Kind != provider.FetchParse {
		t.Errorf("kind = %q, want parse", fetchError.Kind)
	}
	if fetchError.IsTransient() {
		t.Error("the next pass would read the same document and fail the same way")
	}
}

func TestACancelledPassMakesNoRequestAtAll(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := testClient(server).Fetch(ctx, accountWithToken(t, "sk-ant-oat01-ok"))
	fetchError, ok := errors.AsType[*provider.FetchError](err)
	if !ok {
		t.Fatalf("err = %T, want *provider.FetchError", err)
	}
	if fetchError.Kind != provider.FetchCancelled {
		t.Errorf("kind = %q, want cancelled", fetchError.Kind)
	}
	if calls.Load() != 0 {
		t.Errorf("calls = %d, want 0; a cancelled pass does not fetch", calls.Load())
	}
}

func TestTheUsageURLIsTheBasePlusThePathWithNoDoubleSlash(t *testing.T) {
	t.Parallel()

	withSlash := NewUsageClient("https://example.test/", "agentctl/test", time.Second)
	if got := withSlash.UsageURL(); got != "https://example.test/api/oauth/usage" {
		t.Errorf("UsageURL() = %q", got)
	}
	without := NewUsageClient("https://example.test", "agentctl/test", time.Second)
	if got := without.UsageURL(); got != "https://example.test/api/oauth/usage" {
		t.Errorf("UsageURL() = %q", got)
	}
}

func TestTheBearerNeverAppearsInErrorsOrLogs(t *testing.T) {
	// Not parallel: the default logger is swapped to capture what a fetch
	// logs at every level.
	const token = "sk-ant-oat01-must-stay-sealed"

	responses := map[string]http.HandlerFunc{
		"unauthorized": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		"rate limited": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"server error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"not json":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) },
	}
	for name, handler := range responses {
		server := httptest.NewServer(handler)
		logs := capturedLogs(t, func() {
			_, err := testClient(server).Fetch(t.Context(), accountWithToken(t, token))
			if err == nil {
				t.Errorf("%s: the fetch should fail", name)
				return
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("%s: the bearer leaked into the error: %v", name, err)
			}
		})
		server.Close()
		if strings.Contains(logs, token) {
			t.Errorf("%s: the bearer leaked into the log:\n%s", name, logs)
		}

		// A transport failure wraps the request's own error chain, which
		// is the likeliest place a header could be echoed from.
		dead := NewUsageClient("http://127.0.0.1:9", "agentctl/test", 250*time.Millisecond)
		logs = capturedLogs(t, func() {
			_, err := dead.Fetch(t.Context(), accountWithToken(t, token))
			if err == nil {
				t.Error("nothing listens on port 9; the fetch should fail")
				return
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("the bearer leaked into the transport error: %v", err)
			}
		})
		if strings.Contains(logs, token) {
			t.Errorf("the bearer leaked into the transport log:\n%s", logs)
		}
	}
}
