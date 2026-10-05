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

// The machine-readable status report, version 1.
//
// The document serialized here is a published interface. Its shape is
// fixed by schemas/status.v1.json, the normative statement of it: a field
// that changes name or type must fail the suite rather than a user's
// script.
//
// The types are deliberately not the ones the pass uses. The usage model
// is shaped for a program that is deciding things: a kind per window, a
// money value that keeps minor units and an exponent apart. A JSON
// consumer wants none of that shape and all of that information,
// flattened and stably named; serializing the internal types directly
// would publish their layout as the interface and make every internal
// refactor a breaking change, so the conversion is written out here.
//
// No token material, ever. Nothing in this file can reach a token: it is
// built from usage snapshots, the account identifiers and the row's
// state, never from a credential. The raw member carries usage bodies,
// which the API returns without any credential in them.

package render

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"time"

	"github.com/zchee/agentctl/internal/usage"
)

// StatusReportVersion is the report version this build emits, and the
// schema it validates against.
const StatusReportVersion = 1

// CreditsScope is the scope every credit figure is reported at.
//
// Extra-usage credits belong to the organization, not to the account, so
// two accounts in one organization report the same figure. A consumer
// that summed the rows would double-count, which is what this constant
// exists to warn it about.
const CreditsScope = "organization"

// SameIdentityLive is the value a row's same_identity_as member carries
// when the row's identity is the live credential's.
//
// A token rather than a boolean because the question it answers is "the
// same as what": a later build that can also say owned adds a value here
// instead of a second member.
const SameIdentityLive = "live"

// StatusReport is one whole status JSON document. Field order is the
// serialization order.
type StatusReport struct {
	// Version is the document version; always [StatusReportVersion] for
	// this build.
	Version int `json:"version"`
	// GeneratedAt is when the report was built, RFC 3339 in UTC.
	GeneratedAt string `json:"generated_at"`
	// Rows is the rows the run displayed, in the same order the table
	// shows them.
	Rows []JSONRow `json:"rows"`
	// Hidden is how many rows the show-everything flag would have added.
	Hidden int `json:"hidden"`
	// Raw is the untouched usage bodies, keyed by row id. Present only
	// when the raw flag was given.
	Raw *RawBodies `json:"raw,omitzero"`
}

// NewStatusReport builds an empty report stamped with now.
func NewStatusReport(now time.Time, hidden int) StatusReport {
	return StatusReport{
		Version:     StatusReportVersion,
		GeneratedAt: FormatInstant(now),
		Rows:        []JSONRow{},
		Hidden:      hidden,
	}
}

// MarshalStatusReport serializes the document the way stdout carries it:
// two-space indentation and no trailing newline, which the printer adds.
func MarshalStatusReport(report *StatusReport) ([]byte, error) {
	out, err := json.Marshal(report, jsontext.WithIndent("  "))
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out, []byte("\n")), nil
}

// JSONRow is one account of the report.
type JSONRow struct {
	// ID is the identifier the account flag accepts for this row.
	ID string `json:"id"`
	// AccountUUID is the Anthropic account UUID, empty when the row has
	// no identity.
	AccountUUID string `json:"account_uuid"`
	// OrganizationUUID is the organization UUID, or the unknown-org
	// placeholder.
	OrganizationUUID string `json:"organization_uuid"`
	// Email is the account's email address, when it is known.
	Email *string `json:"email"`
	// OrgName is the organization's display name, when it is known.
	OrgName *string `json:"org_name"`
	// Kind says where the credentials live and whether agentctl may
	// write them.
	Kind string `json:"kind"`
	// Source says where this row's credentials were read from.
	Source string `json:"source"`
	// State is the row state as a stable token.
	State string `json:"state"`
	// StateLabel is the same state as the sentence the table prints.
	StateLabel string `json:"state_label"`
	// LockState says what the namespace lock did on this pass.
	LockState string `json:"lock_state"`
	// Windows is every usage window the response described.
	Windows []JSONWindow `json:"windows"`
	// Credits is what is known about extra-usage credits.
	Credits JSONCredits `json:"credits"`
	// NextReset is the soonest reset across every window, RFC 3339.
	NextReset *string `json:"next_reset"`
	// SessionReset is when the five-hour session window rolls over, RFC
	// 3339. Additive next to NextReset rather than a replacement for it:
	// the table's two reset columns need one named window each, and a
	// consumer already reading NextReset must not have to change. In
	// UTC, like every other instant in the document.
	SessionReset *string `json:"session_reset"`
	// WeeklyReset is when the seven-day all-models window rolls over,
	// RFC 3339.
	WeeklyReset *string `json:"weekly_reset"`
	// Note is the short explanation the table appends to the state.
	Note *string `json:"note"`
	// SameIdentityAs says which other credential source on this machine
	// holds the same account and organization pair as this row:
	// [SameIdentityLive], or nil when no other does. Present on every
	// row rather than only on the matching ones, so a consumer testing
	// the member gets an answer instead of having to tell an absent
	// member from a null one.
	SameIdentityAs *string `json:"same_identity_as"`
	// OccupiedBy says who holds this row's keychain item, when a hot
	// swap put another identity there; nil on every other row. Never a
	// token — an email address when the occupying blob named one, an
	// account UUID otherwise.
	OccupiedBy *string `json:"occupied_by"`
}

// JSONWindow is one usage window of the report.
type JSONWindow struct {
	// Kind is session, weekly_all, weekly_scoped or unknown.
	Kind string `json:"kind"`
	// Label is the window's display label, which is where a scoped
	// window's scope and an unrecognised window's raw kind string appear.
	Label string `json:"label"`
	// Percent is the percentage the server sent, clamped to 0..=100.
	Percent *float64 `json:"percent"`
	// PercentFloor is the same figure floored, which is what the table
	// and the web UI show.
	PercentFloor *int `json:"percent_floor"`
	// ResetsAt is when the window rolls over, RFC 3339.
	ResetsAt *string `json:"resets_at"`
	// IsActive reports whether this is the window currently constraining
	// the account.
	IsActive bool `json:"is_active"`
}

// JSONCredits is an account's extra-usage credits, flattened.
//
// Every row carries this object, including a row that never fetched
// anything: a consumer testing the state member gets a real answer rather
// than having to distinguish an absent member from a null one.
type JSONCredits struct {
	// State is on, off, or unavailable — the last meaning the response
	// carried no extra_usage object at all, which is not the same as
	// switched off.
	State string `json:"state"`
	// UsedMinor is the amount spent, in minor units of Currency.
	UsedMinor *int64 `json:"used_minor"`
	// Currency is the ISO 4217 code the figures are in.
	Currency *string `json:"currency"`
	// Exponent is how many decimal places a minor unit represents.
	Exponent *int `json:"exponent"`
	// LimitMinor is the monthly ceiling in minor units, absent for an
	// uncapped account.
	LimitMinor *int64 `json:"limit_minor"`
	// Percent is the server's own utilisation percentage, rounded.
	Percent *int `json:"percent"`
	// DisabledReason says why credits are switched off, when the server
	// said.
	DisabledReason *string `json:"disabled_reason"`
	// Scope is always [CreditsScope]: these figures are the
	// organization's.
	Scope string `json:"scope"`
}

// UnavailableCredits is the credits object a row with no usage at all
// carries.
func UnavailableCredits() JSONCredits {
	return JSONCredits{State: "unavailable", Scope: CreditsScope}
}

// RawBodies is the raw member: the untouched usage bodies keyed by row
// id, in the order the rows were shown, which a plain map would not keep.
type RawBodies struct {
	entries []rawBody
}

// rawBody is one row's untouched usage body.
type rawBody struct {
	id   string
	body jsontext.Value
}

// Add appends one row's body under its id.
func (r *RawBodies) Add(id string, body jsontext.Value) {
	r.entries = append(r.entries, rawBody{id: id, body: body})
}

// MarshalJSONTo writes the bodies as one object in insertion order.
func (r *RawBodies) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, entry := range r.entries {
		if err := enc.WriteToken(jsontext.String(entry.id)); err != nil {
			return err
		}
		if err := enc.WriteValue(entry.body); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// WindowsOf builds the windows member for a row that may not have fetched
// anything. A row with no snapshot has no windows, which serializes as an
// empty array rather than null.
func WindowsOf(snapshot *usage.UsageSnapshot) []JSONWindow {
	if snapshot == nil {
		return nil
	}
	windows := make([]JSONWindow, 0, len(snapshot.Windows))
	for i := range snapshot.Windows {
		windows = append(windows, windowJSON(&snapshot.Windows[i]))
	}
	return windows
}

// windowJSON flattens one window.
func windowJSON(window *usage.LimitWindow) JSONWindow {
	out := JSONWindow{
		Kind:         windowKindToken(window.Kind),
		Label:        window.Label(),
		Percent:      window.Percent,
		PercentFloor: window.PercentFloor,
		IsActive:     window.IsActive,
	}
	if !window.ResetsAt.IsZero() {
		out.ResetsAt = new(FormatInstant(window.ResetsAt))
	}
	return out
}

// windowKindToken is the kind token for one window.
func windowKindToken(kind usage.WindowKind) string {
	switch kind.Class {
	case usage.WindowSession:
		return "session"
	case usage.WindowWeeklyAll:
		return "weekly_all"
	case usage.WindowWeeklyScoped:
		return "weekly_scoped"
	default:
		return "unknown"
	}
}

// CreditsOf builds the credits member for a row that may not have fetched
// anything.
func CreditsOf(snapshot *usage.UsageSnapshot) JSONCredits {
	if snapshot == nil {
		return UnavailableCredits()
	}
	switch state := snapshot.Credits; state.Class {
	case usage.CreditsOff:
		credits := UnavailableCredits()
		credits.State = "off"
		if state.DisabledReason != "" {
			credits.DisabledReason = new(state.DisabledReason)
		}
		return credits
	case usage.CreditsOn:
		return onCredits(state.Credits)
	default:
		return UnavailableCredits()
	}
}

// onCredits is the on case, where the currency and exponent have to be
// recovered.
//
// They come from whichever figure the server actually sent: an account
// with a limit and no spend yet carries them only on the limit, and one
// with an uncapped plan only on the used amount. Reporting minor units
// without saying what they are minor units of would make the figure
// unusable.
func onCredits(credits usage.Credits) JSONCredits {
	out := JSONCredits{State: "on", Scope: CreditsScope, Percent: credits.Percent}
	denomination := credits.Used
	if denomination == nil {
		denomination = credits.Limit
	}
	if denomination != nil {
		out.Currency = new(denomination.Currency)
		out.Exponent = new(int(denomination.Exponent))
	}
	if credits.Used != nil {
		out.UsedMinor = new(credits.Used.AmountMinor)
	}
	if credits.Limit != nil {
		out.LimitMinor = new(credits.Limit.AmountMinor)
	}
	return out
}

// NextResetOf builds the next_reset member: the soonest reset across
// every window, or nil for a row that fetched nothing.
func NextResetOf(snapshot *usage.UsageSnapshot) *string {
	if snapshot == nil {
		return nil
	}
	at, ok := snapshot.NextReset()
	if !ok {
		return nil
	}
	return new(FormatInstant(at))
}

// SessionResetOf builds the session_reset member: the five-hour window's
// own reset.
func SessionResetOf(snapshot *usage.UsageSnapshot) *string {
	return windowResetOf(snapshot, usage.WindowKind{Class: usage.WindowSession})
}

// WeeklyResetOf builds the weekly_reset member: the seven-day all-models
// window's own reset.
func WeeklyResetOf(snapshot *usage.UsageSnapshot) *string {
	return windowResetOf(snapshot, usage.WindowKind{Class: usage.WindowWeeklyAll})
}

// windowResetOf is one named window's reset, for a row that may not have
// fetched anything. Nil covers both a response that described no such
// window and one that described it without a reset, which is the same
// distinction the table's em dash declines to make.
func windowResetOf(snapshot *usage.UsageSnapshot, kind usage.WindowKind) *string {
	if snapshot == nil {
		return nil
	}
	window := snapshot.Window(kind)
	if window == nil || window.ResetsAt.IsZero() {
		return nil
	}
	return new(FormatInstant(window.ResetsAt))
}

// FormatInstant spells one instant the way every timestamp in the
// document is spelled: RFC 3339 in UTC, with the fraction kept only when
// it carries digits.
func FormatInstant(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}
