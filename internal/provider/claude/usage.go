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

// The Claude usage endpoint: the request, the response, and what a failure
// means.
//
// The request is a GET of the usage path with a bearer token, the beta
// opt-in header, an Accept header and the shared User-Agent. The endpoint
// is undocumented, so every one of those was observed rather than assumed,
// and the base URL is overridable only under the testing build tag — a
// production-visible override of an endpoint that receives a bearer token
// would be an exfiltration vector.
//
// Two response shapes, one vocabulary. Current responses carry a limits[]
// array; older ones carry flat five_hour / seven_day / seven_day_<model>
// objects. [Normalize] prefers the array and falls back to the flat keys,
// so the renderer sees one shape. A kind this build has never seen becomes
// an unknown window and is still rendered, with the kind string visible:
// an unrecognised window that disappeared silently is how a user ends up
// over a limit they were never shown. A response that describes no window
// at all is not an error — it is what an API or console account looks
// like, and the caller renders it as its own state.
//
// Credits come from extra_usage, never from spend. A response describes
// the same credit balance twice, and the two disagree in shape and
// sometimes in value. Only extra_usage is read: it is the object the web
// UI drives its own credits panel from, so following it is what keeps
// agentctl and the site quoting the same number. spend is peeked at for
// exactly two contradiction checks and is otherwise passed through
// untouched, visible in --raw alone — parsing it into a second typed value
// would create a second answer to the same question and no rule for
// choosing between them.
//
// No token plaintext is exposed here. The bearer header arrives finished,
// through [provider.UsageAuth]; the value goes straight into the request
// and is never retained or logged.

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/usage"
)

// UsagePath is the path under the base URL.
const UsagePath = "/api/oauth/usage"

// BetaHeader is the beta opt-in header the endpoint requires.
const BetaHeader = "anthropic-beta"

// BetaValue is the beta opt-in value the endpoint requires.
const BetaValue = "oauth-2025-04-20"

// ConnectTimeout is how long a connection may take to establish.
const ConnectTimeout = 5 * time.Second

// MaxUsageBodyBytes is the largest response body this build will read.
//
// A usage document is a few kilobytes; the ceiling stops a wrong endpoint
// — a captive portal, a proxy error page — from being read into memory
// whole.
const MaxUsageBodyBytes = 1 << 22

// HeadlineScope is the scope whose weekly window has a column of its own
// in the table.
const HeadlineScope = "Fable"

// PercentAgreementTolerance is how far extra_usage.utilization and
// spend.percent may drift before the disagreement is worth a warning.
//
// One point, because the two are computed from the same balance at
// slightly different moments and rounded differently; anything larger
// means the column and the web UI would show materially different numbers.
const PercentAgreementTolerance = 1.0

// UsageClient is a client for one pass's worth of usage requests.
//
// It holds its own HTTP client so connections are pooled across the
// accounts in a pass, and so the timeouts are set once rather than per
// request.
type UsageClient struct {
	baseURL   string
	userAgent string
	client    *http.Client
}

var _ provider.UsageProvider[*usage.UsageSnapshot] = (*UsageClient)(nil)

// NewUsageClient builds a client against an explicit base URL.
//
// totalTimeout is the whole-request budget from --timeout;
// [ConnectTimeout] bounds the connection separately, so a host that
// accepts a connection and then goes quiet is cut off by the former and a
// black-holed address by the latter.
func NewUsageClient(baseURL, userAgent string, totalTimeout time.Duration) *UsageClient {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		transport = transport.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.DialContext = (&net.Dialer{Timeout: ConnectTimeout}).DialContext
	return &UsageClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		userAgent: userAgent,
		client:    &http.Client{Transport: transport, Timeout: totalTimeout},
	}
}

// NewUsageClientFromEnv builds the client this process should use: the
// built-in endpoint selection (see the build-tag pair for what that means
// per build) with the shared User-Agent.
func NewUsageClientFromEnv(totalTimeout time.Duration) *UsageClient {
	return NewUsageClient(usageBaseURL(), provider.UserAgent(provider.Claude), totalTimeout)
}

// UsageURL returns the full usage URL.
func (c *UsageClient) UsageURL() string {
	return c.baseURL + UsagePath
}

// Fetch fetches one account's current usage.
//
// A 401 arrives here as the unauthorized fetch error; the decision about
// refreshing is made by the caller, because refreshing safely means
// taking the namespace lock and re-reading the store first, and none of
// that is an HTTP concern.
func (c *UsageClient) Fetch(ctx context.Context, account provider.AccountRef) (*usage.UsageSnapshot, error) {
	if ctx.Err() != nil {
		return nil, provider.NewFetchCancelled()
	}
	slog.DebugContext(ctx, "fetching usage", slog.Any("account", account))

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.UsageURL(), nil)
	if err != nil {
		return nil, provider.NewFetchTransport(err.Error())
	}
	// The authorization value arrives finished and goes straight into the
	// header map; it is dropped with the request and never logged, because
	// the log line for a fetch records the account id and the status,
	// never a header.
	request.Header.Set("Authorization", account.Auth.AuthorizationHeader())
	request.Header.Set(BetaHeader, BetaValue)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	for _, header := range account.Auth.ExtraHeaders() {
		request.Header.Set(header.Name, header.Value)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return nil, mapTransportError(err)
	}
	defer func() {
		// The response has already been consumed or classified by the
		// time this runs; a close failure changes nothing the caller
		// could act on.
		_ = response.Body.Close()
	}()

	status := response.StatusCode
	switch {
	case status >= 200 && status <= 299:
	case status == 401 || status == 403:
		return nil, provider.NewFetchUnauthorized()
	case status == 429:
		if retryAfter, ok := ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()); ok {
			return nil, provider.NewFetchRateLimitedAfter(retryAfter)
		}
		return nil, provider.NewFetchRateLimited()
	default:
		return nil, provider.NewFetchHTTP(status)
	}

	// One byte past the ceiling is read so an oversized body is told apart
	// from one that is exactly at it.
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxUsageBodyBytes+1))
	if err != nil {
		if isTimeout(err) {
			return nil, provider.NewFetchTransport(err.Error())
		}
		return nil, provider.NewFetchParse(err.Error())
	}
	if len(body) > MaxUsageBodyBytes {
		return nil, provider.NewFetchParse(fmt.Sprintf("the response body exceeds %d bytes", MaxUsageBodyBytes))
	}

	var raw jsontext.Value
	if err := json.Unmarshal(body, &raw, jsontext.AllowDuplicateNames(true)); err != nil {
		return nil, provider.NewFetchParse(err.Error())
	}

	// The body is always kept: the cache stores it verbatim so that a
	// stale render and a --raw dump both work, and a build that learns to
	// read a new field understands entries an older one wrote. What --raw
	// controls is whether it is printed; it carries usage figures and no
	// token material.
	snapshot, err := ParseUsage(raw, time.Now(), true)
	if err != nil {
		return nil, provider.NewFetchParse(err.Error())
	}
	return snapshot, nil
}

// mapTransportError maps a failed request onto the pass's vocabulary.
//
// Every arm is transport: a status was never received, so there is
// nothing to branch on but "the request did not complete". The message is
// the error's own text, which names the failure class and the URL — never
// a response body, and so never an echoed token.
func mapTransportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return provider.NewFetchCancelled()
	}
	return provider.NewFetchTransport(err.Error())
}

// isTimeout reports whether a body read failed by running out of time
// rather than by the bytes being unreadable.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && netErr.Timeout()
}

// ParseRetryAfter reads a Retry-After header value.
//
// HTTP allows both a delay in seconds and an HTTP-date, and Anthropic has
// been observed sending neither, one, or the other. A date already in the
// past yields a zero duration — "you may retry now" — rather than false,
// because the server did answer the question.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0, false
	}

	if seconds, err := strconv.ParseUint(text, 10, 64); err == nil {
		// A delay the duration type cannot hold is clamped rather than
		// dropped: the server's answer was "a very long time", and the
		// clamp still says that.
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}

	at, err := http.ParseTime(text)
	if err != nil {
		return 0, false
	}
	if remaining := at.Sub(now); remaining > 0 {
		return remaining, true
	}
	return 0, true
}

// ParseUsage turns a usage response into a snapshot.
//
// It returns an error when the body is not a JSON object. A body that is
// an object but describes no window is not an error: that is what an API
// account looks like, and the caller renders it as its own state.
//
// This is the single place a credits warning becomes a log line, so
// "the warning was emitted" is a claim about the log the process actually
// writes.
func ParseUsage(body jsontext.Value, fetchedAt time.Time, keepRaw bool) (*usage.UsageSnapshot, error) {
	members, ok := objectMembers(body)
	if !ok {
		return nil, fmt.Errorf("expected a JSON object, got %s", jsonTypeName(body))
	}

	credits, warnings := CreditsFromBody(body)
	for _, warning := range warnings {
		slog.Warn("the credits column may not match the web UI for this account; `agentctl claude status --raw` shows what the server sent", slog.String("credits.warning", warning.String()))
	}

	snapshot := &usage.UsageSnapshot{FetchedAt: fetchedAt, Windows: normalizeMembers(members), Credits: credits}
	if keepRaw {
		snapshot.Raw = slices.Clone(body)
	}
	return snapshot, nil
}

// CreditsWarningKind names something the credits parser saw that the
// column cannot show.
type CreditsWarningKind uint8

const (
	// WarnSpendWithoutExtraUsage means the response carried no
	// extra_usage, but its spend object claims the account has credits.
	//
	// This is the under-delivery case and the only detector for it: the
	// column will read "n/a" while the web UI shows a figure.
	WarnSpendWithoutExtraUsage CreditsWarningKind = iota
	// WarnPercentDisagrees means extra_usage.utilization and spend.percent
	// disagree by more than [PercentAgreementTolerance].
	WarnPercentDisagrees
	// WarnExponentOutOfRange means decimal_places was outside the honoured
	// range; the default was used instead, so the figure is still shown,
	// and the warning is what says the decimal point may be misplaced.
	WarnExponentOutOfRange
)

// CreditsWarning is one thing the credits parser saw that the column
// cannot show.
//
// Returned rather than logged so that [CreditsFromBody] stays a pure
// function a test can assert the whole list against; [ParseUsage] is the
// single place that turns one of these into a log line.
type CreditsWarning struct {
	// Kind says which contradiction was seen.
	Kind CreditsWarningKind
	// ExtraUsage is what extra_usage.utilization said, unrounded, for
	// [WarnPercentDisagrees].
	ExtraUsage float64
	// Spend is what spend.percent said, for [WarnPercentDisagrees].
	Spend float64
	// DecimalPlaces is the out-of-range value the server sent, for
	// [WarnExponentOutOfRange].
	DecimalPlaces int64
}

// String states the warning in the user's terms.
func (w CreditsWarning) String() string {
	switch w.Kind {
	case WarnSpendWithoutExtraUsage:
		return "the response carried no `extra_usage` but its `spend` object reports credits"
	case WarnPercentDisagrees:
		return fmt.Sprintf("`extra_usage.utilization` (%v) and `spend.percent` (%v) disagree by more than %v point", w.ExtraUsage, w.Spend, PercentAgreementTolerance)
	case WarnExponentOutOfRange:
		return fmt.Sprintf("`decimal_places` was %d, outside 0..=%d; %d was assumed", w.DecimalPlaces, usage.MaxMoneyExponent, usage.DefaultMoneyExponent)
	default:
		return "the credits figures could not be read as sent"
	}
}

// CreditsFromBody reads the credits column's figures out of a usage body.
//
// extra_usage is the only source. Its absence is the unavailable state
// rather than an error, because a response shape that predates the object
// is a real thing to meet and an account whose windows parsed fine should
// still get a row.
func CreditsFromBody(body jsontext.Value) (usage.CreditsState, []CreditsWarning) {
	var warnings []CreditsWarning
	members, _ := objectMembers(body)
	spend, spendOK := objectField(members, "spend")

	warnOnWithheldSpend := func() {
		if spendOK && spendReportsCredits(spend) {
			warnings = append(warnings, CreditsWarning{Kind: WarnSpendWithoutExtraUsage})
		}
	}

	// A non-object extra_usage — null, most often — is "the server said
	// nothing", not a malformed document: the same key is null on every
	// account that has never touched credits.
	extra, ok := objectField(members, "extra_usage")
	if !ok {
		warnOnWithheldSpend()
		return usage.CreditsState{Class: usage.CreditsUnavailable}, warnings
	}

	// is_enabled is the one field the shape declares as a required
	// boolean; an extra_usage object without it is not the declared shape,
	// so it is reported as "no figure" rather than invented into an On
	// with nothing in it. The spend under-delivery detector still runs for
	// that case.
	enabled, ok := boolField(extra, "is_enabled")
	if !ok {
		warnOnWithheldSpend()
		return usage.CreditsState{Class: usage.CreditsUnavailable}, warnings
	}
	if !enabled {
		reason, _ := stringField(extra, "disabled_reason")
		return usage.CreditsState{Class: usage.CreditsOff, DisabledReason: reason}, warnings
	}

	exponent := usage.DefaultMoneyExponent
	if places, ok := int64Field(extra, "decimal_places"); ok {
		if places >= 0 && places <= int64(usage.MaxMoneyExponent) {
			exponent = uint8(places)
		} else {
			warnings = append(warnings, CreditsWarning{Kind: WarnExponentOutOfRange, DecimalPlaces: places})
		}
	}

	// No currency is not USD: the money type renders a bare figure for an
	// empty code, which says "this many, in whatever the server meant"
	// rather than inventing a symbol the server never sent.
	currency, _ := stringField(extra, "currency")

	utilization, utilizationOK := float64Field(extra, "utilization")
	if utilizationOK && spendOK {
		if spendPercent, ok := float64Field(spend, "percent"); ok &&
			!math.IsNaN(utilization) && !math.IsInf(utilization, 0) &&
			!math.IsNaN(spendPercent) && !math.IsInf(spendPercent, 0) &&
			math.Abs(utilization-spendPercent) > PercentAgreementTolerance {
			warnings = append(warnings, CreditsWarning{Kind: WarnPercentDisagrees, ExtraUsage: utilization, Spend: spendPercent})
		}
	}

	credits := usage.Credits{
		Used:  moneyField(extra, "used_credits", currency, exponent),
		Limit: moneyField(extra, "monthly_limit", currency, exponent),
	}
	if utilizationOK {
		if percent, ok := usage.PercentRound(utilization); ok {
			credits.Percent = &percent
		}
	}
	return usage.CreditsState{Class: usage.CreditsOn, Credits: credits}, warnings
}

// moneyField builds one money value from a minor-unit member, or nil when
// it is absent or unreadable.
func moneyField(members []jsonMember, name, currency string, exponent uint8) *usage.Money {
	value, ok := field(members, name)
	if !ok {
		return nil
	}
	amount, ok := minorUnits(value)
	if !ok {
		return nil
	}
	return &usage.Money{AmountMinor: amount, Currency: currency, Exponent: exponent}
}

// minorUnits reads a minor-unit amount, which the endpoint spells as
// either an integer or a float.
//
// The observed body sends monthly_limit as an integer and used_credits as
// a float in the same object, so both spellings have to work. A float is
// rounded to the nearest whole minor unit and refused outright when it is
// not finite or does not fit an int64: a saturated maximum appearing in a
// money column is a worse answer than an em dash.
func minorUnits(value jsontext.Value) (int64, bool) {
	if exact, ok := asInt64(value); ok {
		return exact, true
	}

	// The lower bound is exactly representable as a float64 because it is
	// a power of two; the upper bound is one past the maximum, exclusive,
	// because the maximum itself is not representable and rounds up to it.
	const lowest = -9_223_372_036_854_775_808.0
	const pastHighest = 9_223_372_036_854_775_808.0

	number, ok := asFloat64(value)
	if !ok {
		return 0, false
	}
	rounded := math.Round(number)
	if math.IsNaN(rounded) || math.IsInf(rounded, 0) || rounded < lowest || rounded >= pastHighest {
		return 0, false
	}
	// Exact: the bounds above admit only whole numbers an int64 holds.
	return int64(rounded), true
}

// spendReportsCredits reports whether a spend object is claiming this
// account has credits.
//
// Not "any non-null field": every response carries a disclaimer and a
// severity, so a walk that counted prose would warn on every body that
// merely lacks extra_usage, and a warning that fires constantly is one
// nobody reads. What matters for the under-delivery case is whether spend
// reports a live figure — an enabled switch, a non-zero percentage or
// amount, or a ceiling — so those are what this looks at.
//
// This is a peek, not a parse: nothing here is kept, and spend still
// reaches the user only through --raw.
func spendReportsCredits(spend []jsonMember) bool {
	if enabled, ok := boolField(spend, "enabled"); ok && enabled {
		return true
	}
	for _, name := range []string{"limit", "balance", "cap"} {
		if value, ok := field(spend, name); ok && value.Kind() != 'n' {
			return true
		}
	}
	if percent, ok := float64Field(spend, "percent"); ok && percent != 0.0 {
		return true
	}
	used, ok := objectField(spend, "used")
	if !ok {
		return false
	}
	amount, ok := float64Field(used, "amount_minor")
	return ok && amount != 0.0
}

// Normalize maps a usage response body onto usage windows. A body that is
// not an object has none.
//
// The limits[] array wins whenever it is present and non-empty. The flat
// legacy keys are consulted only as a fallback, so an account served both
// shapes is read through the newer one — which is the one that carries
// is_active and the scope display names.
func Normalize(body jsontext.Value) []usage.LimitWindow {
	members, ok := objectMembers(body)
	if !ok {
		return nil
	}
	return normalizeMembers(members)
}

// normalizeMembers is [Normalize] past the object check.
func normalizeMembers(members []jsonMember) []usage.LimitWindow {
	if limits, ok := field(members, "limits"); ok {
		if entries, ok := asArray(limits); ok && len(entries) > 0 {
			var windows []usage.LimitWindow
			for _, entry := range entries {
				if window, ok := windowFromLimit(entry); ok {
					windows = append(windows, window)
				}
			}
			if len(windows) > 0 {
				return windows
			}
		}
	}
	return legacyWindows(members)
}

// windowFromLimit maps one limits[] entry.
//
// An entry with no string kind is dropped: kind is the only thing that
// says what the number means, and a percentage with no name is not
// something that can honestly be put in a table.
func windowFromLimit(entry jsontext.Value) (usage.LimitWindow, bool) {
	members, ok := objectMembers(entry)
	if !ok {
		return usage.LimitWindow{}, false
	}
	kindText, ok := stringField(members, "kind")
	if !ok || kindText == "" {
		return usage.LimitWindow{}, false
	}

	var scopeLabel string
	if scope, ok := objectField(members, "scope"); ok {
		if model, ok := objectField(scope, "model"); ok {
			scopeLabel, _ = stringField(model, "display_name")
		}
	}

	var kind usage.WindowKind
	switch kindText {
	case "session":
		kind = usage.WindowKind{Class: usage.WindowSession}
	case "weekly_all":
		kind = usage.WindowKind{Class: usage.WindowWeeklyAll}
	case "weekly_scoped":
		// A scoped window with no readable display name still exists and
		// still constrains the account; "scoped" is a truthful placeholder.
		name := scopeLabel
		if name == "" {
			name = "scoped"
		}
		kind = usage.WindowKind{Class: usage.WindowWeeklyScoped, Name: name}
	default:
		kind = usage.WindowKind{Class: usage.WindowUnknown, Name: kindText}
	}

	window := usage.LimitWindow{Kind: kind, ScopeLabel: scopeLabel}
	if percent, ok := float64Field(members, "percent"); ok {
		setPercent(&window, percent)
	}
	window.Severity, _ = stringField(members, "severity")
	if text, ok := stringField(members, "resets_at"); ok {
		window.ResetsAt = parseTimestamp(text)
	}
	if active, ok := boolField(members, "is_active"); ok {
		window.IsActive = active
	}
	return window, true
}

// legacyWindows maps the flat pre-limits[] keys.
//
// Order is fixed rather than taken from the object: session, then the
// all-model week, then each scoped week in the order the body listed it.
// The table's first three columns depend on that order being stable
// across accounts.
func legacyWindows(members []jsonMember) []usage.LimitWindow {
	var windows []usage.LimitWindow

	if value, ok := field(members, "five_hour"); ok {
		if window, ok := legacyWindow(value, usage.WindowKind{Class: usage.WindowSession}, ""); ok {
			windows = append(windows, window)
		}
	}
	if value, ok := field(members, "seven_day"); ok {
		if window, ok := legacyWindow(value, usage.WindowKind{Class: usage.WindowWeeklyAll}, ""); ok {
			windows = append(windows, window)
		}
	}

	for _, member := range members {
		scope, found := strings.CutPrefix(member.name, "seven_day_")
		if !found || scope == "" {
			continue
		}
		kind := usage.WindowKind{Class: usage.WindowWeeklyScoped, Name: scope}
		if window, ok := legacyWindow(member.value, kind, scope); ok {
			windows = append(windows, window)
		}
	}

	return windows
}

// legacyWindow maps one flat legacy window object; a null or non-object
// value is "this account has no such window", not a window at zero.
func legacyWindow(value jsontext.Value, kind usage.WindowKind, scopeLabel string) (usage.LimitWindow, bool) {
	members, ok := objectMembers(value)
	if !ok {
		return usage.LimitWindow{}, false
	}
	// The flat shape predates is_active; claiming one of these windows is
	// the active constraint would be an invention.
	window := usage.LimitWindow{Kind: kind, ScopeLabel: scopeLabel}
	if percent, ok := float64Field(members, "utilization"); ok {
		setPercent(&window, percent)
	}
	if text, ok := stringField(members, "resets_at"); ok {
		window.ResetsAt = parseTimestamp(text)
	}
	return window, true
}

// setPercent fills both percentage fields from one server-sent value,
// leaving both nil when it is not a usable number.
func setPercent(window *usage.LimitWindow, percent float64) {
	if clamped, ok := usage.ClampPercent(percent); ok {
		window.Percent = &clamped
	}
	if floored, ok := usage.PercentFloor(percent); ok {
		window.PercentFloor = &floored
	}
}

// parseTimestamp parses an RFC 3339 timestamp, discarding anything
// unparseable.
//
// A window with an unreadable resets_at is still a window; the countdown
// cell reads an em dash and the percentage is unaffected.
func parseTimestamp(text string) time.Time {
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return at
}

// jsonTypeName names a JSON value's type for an error message.
func jsonTypeName(value jsontext.Value) string {
	switch value.Kind() {
	case 'n':
		return "null"
	case 'f', 't':
		return "a boolean"
	case '0':
		return "a number"
	case '"':
		return "a string"
	case '[':
		return "an array"
	case '{':
		return "an object"
	default:
		return "an unreadable value"
	}
}

// jsonMember is one member of a JSON object, in document order.
type jsonMember struct {
	name  string
	value jsontext.Value
}

// objectMembers reads an object's members in document order, or reports
// false for anything that is not an object. Order matters because the
// legacy scoped windows appear in the order the body listed them.
func objectMembers(value jsontext.Value) ([]jsonMember, bool) {
	if value.Kind() != '{' {
		return nil, false
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(value), jsontext.AllowDuplicateNames(true))
	if token, err := decoder.ReadToken(); err != nil || token.Kind() != '{' {
		return nil, false
	}
	var members []jsonMember
	for {
		if decoder.PeekKind() == '}' {
			_, err := decoder.ReadToken()
			return members, err == nil
		}
		token, err := decoder.ReadToken()
		if err != nil || token.Kind() != '"' {
			return nil, false
		}
		// The token is only valid until the next read, so its text is
		// taken before the value is.
		name := token.String()
		raw, err := decoder.ReadValue()
		if err != nil {
			return nil, false
		}
		members = append(members, jsonMember{name: name, value: slices.Clone(raw)})
	}
}

// field returns the named member's value; the last occurrence wins when a
// body repeats a name.
func field(members []jsonMember, name string) (jsontext.Value, bool) {
	for _, candidate := range slices.Backward(members) {
		if candidate.name == name {
			return candidate.value, true
		}
	}
	return nil, false
}

// objectField returns the named member parsed as an object.
func objectField(members []jsonMember, name string) ([]jsonMember, bool) {
	value, ok := field(members, name)
	if !ok {
		return nil, false
	}
	return objectMembers(value)
}

// stringField returns the named member when it is a JSON string.
func stringField(members []jsonMember, name string) (string, bool) {
	value, ok := field(members, name)
	if !ok || value.Kind() != '"' {
		return "", false
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", false
	}
	return text, true
}

// boolField returns the named member when it is a JSON boolean.
func boolField(members []jsonMember, name string) (bool, bool) {
	value, ok := field(members, name)
	if !ok {
		return false, false
	}
	switch value.Kind() {
	case 't':
		return true, true
	case 'f':
		return false, true
	default:
		return false, false
	}
}

// asInt64 reads a JSON number as an exact integer: no fraction, no
// exponent, within range. A float spelling takes the float path instead,
// where it is rounded, so the two spellings the endpoint uses stay
// distinguishable.
func asInt64(value jsontext.Value) (int64, bool) {
	if value.Kind() != '0' || bytes.ContainsAny(value, ".eE") {
		return 0, false
	}
	number, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

// int64Field returns the named member when it is an exact JSON integer.
func int64Field(members []jsonMember, name string) (int64, bool) {
	value, ok := field(members, name)
	if !ok {
		return 0, false
	}
	return asInt64(value)
}

// asFloat64 reads any JSON number.
func asFloat64(value jsontext.Value) (float64, bool) {
	if value.Kind() != '0' {
		return 0, false
	}
	number, err := strconv.ParseFloat(string(value), 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

// float64Field returns the named member when it is a JSON number.
func float64Field(members []jsonMember, name string) (float64, bool) {
	value, ok := field(members, name)
	if !ok {
		return 0, false
	}
	return asFloat64(value)
}

// asArray returns a value's elements when it is a JSON array.
func asArray(value jsontext.Value) ([]jsontext.Value, bool) {
	if value.Kind() != '[' {
		return nil, false
	}
	var elements []jsontext.Value
	if err := json.Unmarshal(value, &elements, jsontext.AllowDuplicateNames(true)); err != nil {
		return nil, false
	}
	return elements, true
}
