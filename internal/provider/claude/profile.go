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

// The profile GET: who a credential belongs to, as the server names it.
//
// One GET, never retried. It rotates nothing — unlike a refresh, it spends
// no grant — which is why a caller may issue it before asking the user for
// anything. The response is the account's personal data: the email address
// and the rest of the document are never logged, never printed and never
// quoted in an error, and every render of [Profile] redacts them.

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	json "encoding/json/v2"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
)

// Profile is whose credential a profile GET was made with, as the server
// names it.
//
// Three members are required — account.uuid, the email and
// organization.uuid — and everything else the server sent is kept,
// untouched, in Document. The email is the account's personal data: it is
// never logged, never printed and never written by anything that holds a
// Profile for identity alone, and every render redacts it together with
// the document.
type Profile struct {
	// AccountUUID is the account UUID.
	AccountUUID string
	// Email is the account's email address (account.email, or the
	// exchange-shaped account.email_address).
	Email string
	// OrganizationUUID is the organization UUID.
	OrganizationUUID string
	// Document is the whole document as the server sent it.
	Document jsontext.Value
}

// ProfileParseError reports which required member a profile document
// lacked. The member's value is never quoted.
type ProfileParseError struct {
	// Member is the dotted member path.
	Member string
}

// Error names the member that is missing, empty or not a string.
func (e *ProfileParseError) Error() string {
	return fmt.Sprintf("`%s` is missing, empty or not a string", e.Member)
}

// organizationPlans is the organization_type → subscriptionType map, in
// the order the reference readers define it. These four words are the only
// subscriptionType values those readers accept, so anything else maps to
// nothing rather than to a guess.
var organizationPlans = [4][2]string{
	{"claude_max", "max"},
	{"claude_pro", "pro"},
	{"claude_enterprise", "enterprise"},
	{"claude_team", "team"},
}

// ProfileOf fetches the profile with credentials' access token and holds
// the document to the required schema.
//
// The bearer header arrives finished through
// [Credentials.AuthorizationHeader]; no token plaintext is read here. A
// document missing a required member surfaces as a parse failure naming
// the member, never its value, and a non-2xx answer keeps its status so
// the caller can tell "the token is no longer honoured" from "the server
// could not be asked".
func (c *OAuthClient) ProfileOf(ctx context.Context, credentials *Credentials) (*Profile, error) {
	if ctx.Err() != nil {
		return nil, provider.NewFetchCancelled()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.profileURL, nil)
	if err != nil {
		return nil, provider.NewFetchTransport(err.Error())
	}
	// Cache-Control: no-cache because the answer is about the credential in
	// hand now; an intermediary's copy of an earlier answer would name
	// whoever held the token before.
	request.Header.Set("Authorization", credentials.AuthorizationHeader())
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("User-Agent", c.userAgent)

	response, err := c.profileClient.Do(request)
	if err != nil {
		return nil, mapTransportError(err)
	}
	defer func() {
		// The response has already been consumed or classified by the time
		// this runs; a close failure changes nothing the caller could act
		// on.
		_ = response.Body.Close()
	}()

	text, err := readOAuthBody(response.Body)
	if err != nil {
		return nil, err
	}
	// The body is personal data; the profile keeps its own copy, so the
	// read buffer does not linger.
	defer memguard.WipeBytes(text)

	if status := response.StatusCode; status < 200 || status > 299 {
		kind := errs.NewHTTP(status)
		if redacted := redactBody(string(text)); redacted != "" {
			return nil, fmt.Errorf("%w: %s", kind, redacted)
		}
		return nil, kind
	}

	var document jsontext.Value
	if err := json.Unmarshal(text, &document, jsontext.AllowDuplicateNames(true)); err != nil {
		// A fixed sentence rather than the decoder's own: its message can
		// quote part of the document, and the document carries the
		// account's personal data.
		return nil, provider.NewFetchParse("the profile response could not be parsed: it is not a JSON document")
	}
	profile, err := ParseProfile(document)
	if err != nil {
		return nil, provider.NewFetchParse("the profile response could not be parsed: " + err.Error())
	}
	return profile, nil
}

// ParseProfile holds a profile document to the required schema.
//
// account.email is the profile endpoint's spelling; account.email_address
// is the token exchange's, read when the first is absent. A document with
// a flat top-level uuid names nobody: the captured shape is authoritative,
// so a guess at another one would be an invention.
func ParseProfile(document jsontext.Value) (*Profile, error) {
	text := func(block, member string) string {
		members, ok := objectMembers(document)
		if !ok {
			return ""
		}
		inner, ok := objectField(members, block)
		if !ok {
			return ""
		}
		value, ok := stringField(inner, member)
		if !ok {
			return ""
		}
		return value
	}

	accountUUID := text("account", "uuid")
	if accountUUID == "" {
		return nil, &ProfileParseError{Member: "account.uuid"}
	}
	email := text("account", "email")
	if email == "" {
		email = text("account", "email_address")
	}
	if email == "" {
		return nil, &ProfileParseError{Member: "account.email"}
	}
	organizationUUID := text("organization", "uuid")
	if organizationUUID == "" {
		return nil, &ProfileParseError{Member: "organization.uuid"}
	}
	return &Profile{
		AccountUUID:      accountUUID,
		Email:            email,
		OrganizationUUID: organizationUUID,
		Document:         bytes.Clone(document),
	}, nil
}

// organizationMember returns one string member of the document's
// organization block.
func (p *Profile) organizationMember(member string) (string, bool) {
	members, ok := objectMembers(p.Document)
	if !ok {
		return "", false
	}
	organization, ok := objectField(members, "organization")
	if !ok {
		return "", false
	}
	return stringField(organization, member)
}

// OrganizationName returns the organization's display name, when the
// document carries one.
func (p *Profile) OrganizationName() (string, bool) {
	return p.organizationMember("name")
}

// Identity returns whose profile this is: the two ids and nothing else.
//
// The email and the organization name stay nil, so comparing a profile
// with an exchange or a record copies no personal data into the value.
func (p *Profile) Identity() *Identity {
	organizationUUID := p.OrganizationUUID
	return &Identity{
		AccountUUID:      p.AccountUUID,
		OrganizationUUID: &organizationUUID,
	}
}

// PlanOf returns the plan a profile names, in the reference readers' own
// words.
//
// organization.organization_type, matched exactly — case-sensitive, never
// trimmed — against the four known values. Anything else, absent or not a
// string yields nothing, and nothing is read in its place: not seat_tier,
// not billing_type, and no other endpoint's plan flags.
func PlanOf(profile *Profile) (string, bool) {
	kind, ok := profile.organizationMember("organization_type")
	if !ok {
		return "", false
	}
	for _, pair := range organizationPlans {
		if pair[0] == kind {
			return pair[1], true
		}
	}
	return "", false
}

// RateLimitTierOf returns the rate-limit tier a profile names, when it is
// in the shape the reference readers store.
//
// organization.rate_limit_tier of the same GET, verbatim, but only when
// planWord admits it: the bound is what keeps the stored word inside the
// keychain line budget, so a long server string cannot turn a later write
// into a refusal. A refused value is logged by its length only — it is
// server text this build did not validate, and no log line may be where
// it gets printed.
func RateLimitTierOf(profile *Profile) (string, bool) {
	tier, ok := profile.organizationMember("rate_limit_tier")
	if !ok {
		return "", false
	}
	if planWord(tier) {
		return tier, true
	}
	slog.Debug("the rate-limit tier is not in the stored shape; it was not recorded", slog.Int("len", len(tier)))
	return "", false
}

// NeedsPlan reports whether a credential still lacks half of its plan,
// which is the gate for asking the profile at a refresh.
//
// Either field absent asks. Both present never ask again, so a stored
// pair costs no GET at any later refresh.
func NeedsPlan(credentials *Credentials) bool {
	return credentials.SubscriptionType == nil || credentials.RateLimitTier == nil
}

// FillPlan folds a profile's plan into a credential, per field new-or-old.
//
// A readable value replaces what is stored. An unmapped type, a refused
// tier or an absent organization keeps it, and nothing is invented: no
// default tier, and none derived from the plan.
func FillPlan(credentials *Credentials, profile *Profile) {
	if plan, ok := PlanOf(profile); ok && planWord(plan) {
		credentials.SubscriptionType = &plan
	}
	if tier, ok := RateLimitTierOf(profile); ok {
		credentials.RateLimitTier = &tier
	}
}

// planWord reports whether value matches ^[a-z][a-z0-9_]{0,63}$, the
// reference readers' own sanitiser for these words.
//
// A byte check rather than a regexp: one fixed pattern is not worth a
// dependency. A lowercase ASCII letter, then at most 63 lowercase
// letters, digits or underscores; 1 to 64 bytes in all.
func planWord(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	if value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		b := value[i]
		if b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' {
			continue
		}
		return false
	}
	return true
}

// String renders the two ids and nothing else, so a failing assertion
// cannot print the email or the rest of the document.
func (p *Profile) String() string {
	return fmt.Sprintf("Profile{account_uuid: %s, email: <redacted>, organization_uuid: %s, document: <redacted>}",
		p.AccountUUID, p.OrganizationUUID)
}

// GoString renders the same redacted form as String.
func (p *Profile) GoString() string { return p.String() }

// Format renders the same redacted form as String for every verb, width,
// precision and flag.
func (p *Profile) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, p.String())
}

// LogValue renders the same redacted form as String.
func (p *Profile) LogValue() slog.Value { return slog.StringValue(p.String()) }

// MarshalJSON returns a redaction marker: nothing serializes a profile,
// and nothing may start to by accident.
func (p *Profile) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo writes a redaction marker through the streaming hook.
func (p *Profile) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String("[REDACTED]"))
}
