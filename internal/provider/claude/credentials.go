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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/secret"
)

// BlobRoot is the key the credential object hangs under, in both the
// keychain item and the credential file.
const BlobRoot = "claudeAiOauth"

// RefreshMarginMillis is how long before expiry an access token counts as
// expired. Refreshing this far ahead keeps a token from dying between the
// expiry check and the request that uses it, which happens routinely
// because the two use different clocks.
const RefreshMarginMillis int64 = 300_000

// ClientID is Claude Code's public OAuth client id.
const ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// DefaultScopes is the scope set a live Claude Code credential carries, and
// the set agentctl requests at login.
var DefaultScopes = []string{
	"user:file_upload",
	"user:inference",
	"user:mcp_servers",
	"user:profile",
	"user:sessions:claude_code",
}

// ErrMissingRoot reports a credential blob with no claudeAiOauth object.
var ErrMissingRoot = errors.New("the credentials blob has no `claudeAiOauth` object")

// ErrNoRefreshToken reports a refresh attempted without a refresh token.
var ErrNoRefreshToken = errors.New("this account has no refresh token; run `agentctl claude login`")

// MissingFieldError reports a credential blob field this build requires
// that is absent or the wrong type.
type MissingFieldError struct {
	// Field is the blob member name.
	Field string
}

// Error names the missing or wrong-typed field.
func (e *MissingFieldError) Error() string {
	return fmt.Sprintf("the credentials blob field `%s` is missing or has the wrong type", e.Field)
}

// ExpiryOverflowError reports a timestamp computation that did not fit in a
// 64-bit millisecond timestamp.
type ExpiryOverflowError struct {
	// Field is the response member whose value overflowed.
	Field string
}

// Error names the overflowing field.
func (e *ExpiryOverflowError) Error() string {
	return fmt.Sprintf("the token expiry `%s` does not fit in a 64-bit millisecond timestamp", e.Field)
}

// TokenAccount is the identity block newer credential blobs carry.
//
// Every member is written on serialization, absent ones as null, in this
// fixed order, which is the shape the blob's author writes.
type TokenAccount struct {
	// UUID is the account UUID.
	UUID *string `json:"uuid"`
	// EmailAddress is the account's email address.
	EmailAddress *string `json:"emailAddress"`
	// OrganizationUUID is the organization UUID.
	OrganizationUUID *string `json:"organizationUuid"`
	// OrganizationName is the organization's display name.
	OrganizationName *string `json:"organizationName"`
	// WorkspaceID is the workspace id, when the token is workspace-scoped.
	WorkspaceID *string `json:"workspaceId"`
	// WorkspaceName is the workspace's display name.
	WorkspaceName *string `json:"workspaceName"`
}

// Identity is who a credential belongs to, once it is known.
type Identity struct {
	// AccountUUID is the Anthropic account UUID — half of the namespace key.
	AccountUUID string
	// OrganizationUUID is the organization UUID, when the blob named one.
	OrganizationUUID *string
	// Email is the account's email address.
	Email *string
	// OrgName is the organization's display name.
	OrgName *string
}

// Digests are fingerprints of a credential's token material: what log
// lines, audits and renders carry in place of the tokens themselves.
type Digests struct {
	// AccessSHA256 is the lowercase hex SHA-256 of the access token.
	AccessSHA256 string
	// RefreshSHA256 is the lowercase hex SHA-256 of the refresh token, or
	// empty when the credential has none.
	RefreshSHA256 string
}

// ExtraMember is one credential blob member this build does not recognise,
// preserved in arrival order with its raw value bytes.
type ExtraMember struct {
	// Name is the member name.
	Name string
	// Value is the member's raw JSON value.
	Value jsontext.Value
}

// Credentials authenticates usage requests through the vendor-neutral
// seam, which receives finished header values and never the secrets.
var _ provider.UsageAuth = (*Credentials)(nil)

// Credentials is one account's OAuth credentials.
//
// agentctl writes the credential blob in exactly the shape Claude Code
// reads, so a Claude Code session pointed at an agentctl namespace can read
// it. That makes the blob a shared format, and shared formats grow fields:
// anything this build does not recognise is kept in Extra and written back
// out unchanged, so a refresh never loses a field a newer Claude Code
// added — losing one would silently degrade the session that reads the
// file next.
//
// Every render of this type — String, GoString, Format, LogValue,
// MarshalJSON — shows digests or redaction markers, never a token. The
// credential document that must carry the tokens to its store is produced
// only by [Credentials.BlobJSON], and its output must be wiped after the
// I/O that consumes it.
type Credentials struct {
	// AccessToken is the bearer token for the usage API.
	AccessToken *secret.Secret
	// RefreshToken is the token that mints new access tokens. Nil in blobs
	// written by something that only had an access token to store.
	RefreshToken *secret.Secret
	// ExpiresAtMillis is when the access token expires, in milliseconds
	// since the epoch.
	ExpiresAtMillis int64
	// RefreshTokenExpiresAtMillis is when the refresh token expires. Nil in
	// older blobs.
	RefreshTokenExpiresAtMillis *int64
	// Scopes is the scope set the token carries.
	Scopes []string
	// SubscriptionType is the subscription tier, as the server named it.
	SubscriptionType *string
	// RateLimitTier is the rate-limit tier, as the server named it.
	RateLimitTier *string
	// ClientID is the OAuth client id the token was minted for. Nil in
	// older blobs.
	ClientID *string
	// TokenAccount is who the token belongs to. Nil in older blobs, which
	// is why an imported account can have an unknown identity.
	TokenAccount *TokenAccount
	// Extra is every member this build does not recognise, in arrival
	// order.
	Extra []ExtraMember
}

// ParseBlob parses a credential blob in Claude Code's shape.
//
// accessToken and expiresAt are required; everything else is optional,
// because older blobs genuinely lack refreshTokenExpiresAt, clientId and
// tokenAccount, and refusing them would hide real accounts from the user.
//
// The caller keeps responsibility for wiping blob: it carries the tokens
// in plaintext.
func ParseBlob(blob []byte) (*Credentials, error) {
	root, err := readWholeValue(blob)
	if err != nil {
		return nil, fmt.Errorf("the credentials blob is not valid JSON: %w", err)
	}
	defer memguard.WipeBytes(root)

	if root.Kind() != '{' {
		return nil, ErrMissingRoot
	}
	rootMembers, err := collapseObject(root)
	if err != nil {
		return nil, fmt.Errorf("the credentials blob is not valid JSON: %w", err)
	}
	defer wipeMembers(rootMembers)
	inner := memberValue(rootMembers, BlobRoot)
	if inner == nil || inner.Kind() != '{' {
		return nil, ErrMissingRoot
	}

	members, err := collapseObject(inner)
	if err != nil {
		return nil, fmt.Errorf("the credentials blob is not valid JSON: %w", err)
	}
	defer wipeMembers(members)

	credentials := &Credentials{}
	for _, m := range members {
		switch m.name {
		case "accessToken":
			if token, ok := takeSecret(m.value); ok {
				credentials.AccessToken = token
				continue
			}
		case "refreshToken":
			if token, ok := takeSecret(m.value); ok {
				credentials.RefreshToken = token
				continue
			}
		case "expiresAt":
			if n, ok := takeInt64(m.value); ok {
				credentials.ExpiresAtMillis = n
				continue
			}
		case "refreshTokenExpiresAt":
			if n, ok := takeInt64(m.value); ok {
				credentials.RefreshTokenExpiresAtMillis = &n
				continue
			}
		case "scopes":
			// A wrong-typed scope list is dropped rather than preserved:
			// the field defaults to empty and the member is not kept.
			if m.value.Kind() == '[' {
				credentials.Scopes = stringItems(m.value)
			}
			continue
		case "subscriptionType":
			if s, ok := takeString(m.value); ok {
				credentials.SubscriptionType = &s
				continue
			}
		case "rateLimitTier":
			if s, ok := takeString(m.value); ok {
				credentials.RateLimitTier = &s
				continue
			}
		case "clientId":
			if s, ok := takeString(m.value); ok {
				credentials.ClientID = &s
				continue
			}
		case "tokenAccount":
			// An identity block that does not unmarshal is dropped rather
			// than preserved, like a wrong-typed scope list.
			var account TokenAccount
			if err := json.Unmarshal(m.value, &account); err == nil {
				credentials.TokenAccount = &account
			}
			continue
		default:
			credentials.Extra = append(credentials.Extra, ExtraMember{Name: m.name, Value: bytes.Clone(m.value)})
			continue
		}
		// A recognised key of the wrong type keeps its place among the
		// unknown members, so writing the blob back does not move it.
		credentials.Extra = append(credentials.Extra, ExtraMember{Name: m.name, Value: bytes.Clone(m.value)})
	}

	if credentials.AccessToken == nil {
		return nil, &MissingFieldError{Field: "accessToken"}
	}
	if !hasMember(members, "expiresAt") || !isInt64(memberValue(members, "expiresAt")) {
		return nil, &MissingFieldError{Field: "expiresAt"}
	}
	return credentials, nil
}

// BlobJSON serializes the blob, ready to be written to its store.
//
// Known members are emitted in the order Claude Code writes them, then
// every unrecognised member in the order it arrived. Absent optional
// members are omitted rather than written as null, which is what Claude
// Code does and what makes the round-trip of an old blob byte-stable.
//
// The output carries the tokens in plaintext. It exists only to be handed
// to a credential store, and the caller must wipe it after that I/O.
func (c *Credentials) BlobJSON() ([]byte, error) {
	var buffer bytes.Buffer
	enc := jsontext.NewEncoder(&buffer)

	write := func(tokens ...jsontext.Token) error {
		for _, token := range tokens {
			if err := enc.WriteToken(token); err != nil {
				return err
			}
		}
		return nil
	}
	if err := write(jsontext.BeginObject, jsontext.String(BlobRoot), jsontext.BeginObject); err != nil {
		return nil, err
	}

	emitted := make(map[string]bool)
	writeSecretMember := func(name string, token *secret.Secret) error {
		if err := write(jsontext.String(name)); err != nil {
			return err
		}
		emitted[name] = true
		var quoted []byte
		err := token.WithPlaintext(func(b []byte) error {
			var quoteErr error
			if quoted, quoteErr = jsontext.AppendQuote(nil, b); quoteErr != nil {
				return quoteErr
			}
			return enc.WriteValue(quoted)
		})
		memguard.WipeBytes(quoted)
		return err
	}

	if err := writeSecretMember("accessToken", c.AccessToken); err != nil {
		return nil, err
	}
	if c.RefreshToken != nil {
		if err := writeSecretMember("refreshToken", c.RefreshToken); err != nil {
			return nil, err
		}
	}
	if err := write(jsontext.String("expiresAt"), jsontext.Int(c.ExpiresAtMillis)); err != nil {
		return nil, err
	}
	emitted["expiresAt"] = true
	if c.RefreshTokenExpiresAtMillis != nil {
		if err := write(jsontext.String("refreshTokenExpiresAt"), jsontext.Int(*c.RefreshTokenExpiresAtMillis)); err != nil {
			return nil, err
		}
		emitted["refreshTokenExpiresAt"] = true
	}
	if err := write(jsontext.String("scopes"), jsontext.BeginArray); err != nil {
		return nil, err
	}
	for _, scope := range c.Scopes {
		if err := write(jsontext.String(scope)); err != nil {
			return nil, err
		}
	}
	if err := write(jsontext.EndArray); err != nil {
		return nil, err
	}
	emitted["scopes"] = true
	for _, optional := range []struct {
		name  string
		value *string
	}{
		{"subscriptionType", c.SubscriptionType},
		{"rateLimitTier", c.RateLimitTier},
		{"clientId", c.ClientID},
	} {
		if optional.value == nil {
			continue
		}
		if err := write(jsontext.String(optional.name), jsontext.String(*optional.value)); err != nil {
			return nil, err
		}
		emitted[optional.name] = true
	}
	if c.TokenAccount != nil {
		if err := write(jsontext.String("tokenAccount")); err != nil {
			return nil, err
		}
		if err := json.MarshalEncode(enc, c.TokenAccount); err != nil {
			return nil, err
		}
		emitted["tokenAccount"] = true
	}
	for _, extra := range c.Extra {
		// An unrecognised member whose name a known member already used is
		// skipped: the known member is the one this build vouches for.
		if emitted[extra.Name] {
			continue
		}
		if err := write(jsontext.String(extra.Name)); err != nil {
			return nil, err
		}
		if err := enc.WriteValue(extra.Value); err != nil {
			return nil, err
		}
	}
	if err := write(jsontext.EndObject, jsontext.EndObject); err != nil {
		return nil, err
	}

	// The encoder appends a newline after the top-level value; the blob is
	// a single line in both its stores.
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// AuthorizationHeader returns the Authorization header value for a request
// made as this account.
//
// The returned value carries the token — that is what a bearer header is —
// and is the one deliberate plaintext return of this type. A credential
// whose secret can no longer be opened yields an empty value, which the
// server rejects as unauthorized.
func (c *Credentials) AuthorizationHeader() string {
	var header string
	if err := c.AccessToken.WithPlaintext(func(b []byte) error {
		header = "Bearer " + string(b)
		return nil
	}); err != nil {
		return ""
	}
	return header
}

// ExtraHeaders returns no header pairs: the anthropic-beta header is a
// property of the endpoint, not of the credential, so it stays where the
// request is built.
func (c *Credentials) ExtraHeaders() []provider.Header {
	return nil
}

// RefreshBody builds the token-refresh POST body.
//
// Built here rather than in the OAuth client so the refresh token's
// plaintext stays inside this type's exposure path. The body must include
// scope, or the server narrows the grant to its default single scope.
//
// The output carries the refresh token in plaintext; the caller must wipe
// it after the request that consumes it.
func (c *Credentials) RefreshBody(clientID string) ([]byte, error) {
	if c.RefreshToken == nil {
		return nil, ErrNoRefreshToken
	}
	var buffer bytes.Buffer
	enc := jsontext.NewEncoder(&buffer)
	for _, token := range []jsontext.Token{
		jsontext.BeginObject,
		jsontext.String("grant_type"), jsontext.String("refresh_token"),
		jsontext.String("refresh_token"),
	} {
		if err := enc.WriteToken(token); err != nil {
			return nil, err
		}
	}
	var quoted []byte
	err := c.RefreshToken.WithPlaintext(func(b []byte) error {
		var quoteErr error
		if quoted, quoteErr = jsontext.AppendQuote(nil, b); quoteErr != nil {
			return quoteErr
		}
		return enc.WriteValue(quoted)
	})
	memguard.WipeBytes(quoted)
	if err != nil {
		return nil, err
	}
	for _, token := range []jsontext.Token{
		jsontext.String("client_id"), jsontext.String(clientID),
		jsontext.String("scope"), jsontext.String(strings.Join(c.Scopes, " ")),
		jsontext.EndObject,
	} {
		if err := enc.WriteToken(token); err != nil {
			return nil, err
		}
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// AccessExpired reports whether the access token is expired, or will be
// within marginMillis of nowMillis.
//
// Overflow answers expired: a wrapped addition would otherwise produce a
// large negative instant and report a long-dead token as fresh.
func (c *Credentials) AccessExpired(nowMillis, marginMillis int64) bool {
	threshold, ok := checkedAdd(nowMillis, marginMillis)
	if !ok {
		return true
	}
	return threshold >= c.ExpiresAtMillis
}

// MergeRefresh folds a token-endpoint response into these credentials,
// taking ownership of the response's secrets.
//
// Two rules carry weight:
//
//   - A response without a refresh token keeps the old one. The server
//     does not rotate refresh tokens on every exchange, and overwriting
//     the stored one with nothing would end the chain.
//   - The response lifetimes are in seconds while everything stored is in
//     milliseconds, and the conversion is checked.
func (c *Credentials) MergeRefresh(r *TokenResponse, nowMillis int64) error {
	expiresAt, err := deadlineMillis(nowMillis, r.ExpiresIn, "expires_in")
	if err != nil {
		return err
	}
	refreshExpires := c.RefreshTokenExpiresAtMillis
	if r.RefreshTokenExpiresIn != nil {
		deadline, err := deadlineMillis(nowMillis, *r.RefreshTokenExpiresIn, "refresh_token_expires_in")
		if err != nil {
			return err
		}
		refreshExpires = &deadline
	}

	c.AccessToken = r.AccessToken
	if r.RefreshToken != nil {
		c.RefreshToken = r.RefreshToken
	}
	c.ExpiresAtMillis = expiresAt
	c.RefreshTokenExpiresAtMillis = refreshExpires
	if r.Scope != nil {
		c.Scopes = strings.Fields(*r.Scope)
	}
	return nil
}

// Identity returns who these credentials belong to, from the tokenAccount
// block alone — never from a directory path, and never from the last-login
// record a configuration directory keeps, which is evidence about the live
// row and nothing else.
func (c *Credentials) Identity() *Identity {
	if c.TokenAccount == nil || c.TokenAccount.UUID == nil {
		return nil
	}
	return &Identity{
		AccountUUID:      *c.TokenAccount.UUID,
		OrganizationUUID: c.TokenAccount.OrganizationUUID,
		Email:            c.TokenAccount.EmailAddress,
		OrgName:          c.TokenAccount.OrganizationName,
	}
}

// Digests returns fingerprints of the token material, computed inside the
// locked buffers.
func (c *Credentials) Digests() (Digests, error) {
	access, err := c.AccessToken.Digest()
	if err != nil {
		return Digests{}, err
	}
	digests := Digests{AccessSHA256: access}
	if c.RefreshToken != nil {
		refresh, err := c.RefreshToken.Digest()
		if err != nil {
			return Digests{}, err
		}
		digests.RefreshSHA256 = refresh
	}
	return digests, nil
}

// String renders digests where the tokens would go.
func (c *Credentials) String() string {
	accessDigest, refreshDigest := "<unavailable>", "<unavailable>"
	if digests, err := c.Digests(); err == nil {
		accessDigest = digests.AccessSHA256
		refreshDigest = digests.RefreshSHA256
	}
	refreshToken := "absent"
	if c.RefreshToken != nil {
		refreshToken = "<redacted>"
	} else {
		refreshDigest = "absent"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Credentials{access_token: <redacted>, access_sha256: %s, refresh_token: %s, refresh_sha256: %s, expires_at_ms: %d",
		accessDigest, refreshToken, refreshDigest, c.ExpiresAtMillis)
	if c.RefreshTokenExpiresAtMillis != nil {
		fmt.Fprintf(&builder, ", refresh_token_expires_at_ms: %d", *c.RefreshTokenExpiresAtMillis)
	}
	fmt.Fprintf(&builder, ", scopes: %v", c.Scopes)
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"subscription_type", c.SubscriptionType},
		{"rate_limit_tier", c.RateLimitTier},
		{"client_id", c.ClientID},
	} {
		if field.value != nil {
			fmt.Fprintf(&builder, ", %s: %s", field.name, *field.value)
		}
	}
	extraNames := make([]string, 0, len(c.Extra))
	for _, extra := range c.Extra {
		extraNames = append(extraNames, extra.Name)
	}
	fmt.Fprintf(&builder, ", extra_keys: %v}", extraNames)
	return builder.String()
}

// GoString renders the same redacted form as String.
func (c *Credentials) GoString() string { return c.String() }

// Format renders the same redacted form as String for every verb, width,
// precision and flag.
func (c *Credentials) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// LogValue renders the same redacted form as String.
func (c *Credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON returns a redaction marker. The credential document that
// carries the tokens is produced only by [Credentials.BlobJSON].
func (c *Credentials) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo writes a redaction marker through the streaming hook.
func (c *Credentials) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String("[REDACTED]"))
}

// TokenResponse is the document the token endpoint answers a refresh or an
// authorization-code exchange with.
type TokenResponse struct {
	// AccessToken is the new bearer token.
	AccessToken *secret.Secret
	// RefreshToken is the new refresh token, when the server rotated it.
	// Nil means keep the one already stored.
	RefreshToken *secret.Secret
	// ExpiresIn is the access-token lifetime, in seconds.
	ExpiresIn int64
	// RefreshTokenExpiresIn is the refresh-token lifetime, in seconds,
	// when the server sent one. It is anchored to the original login: a
	// rotation does not reset the refresh chain's own expiry.
	RefreshTokenExpiresIn *int64
	// Scope is the granted scope set, space-separated, when the server
	// named it.
	Scope *string
	// TokenType is Bearer in practice.
	TokenType *string
	// Account is the account the token belongs to, when the response
	// named it.
	Account *ExchangeAccount
	// Organization is the organization the token belongs to, when the
	// response named it.
	Organization *ExchangeOrganization
	// Workspace is the workspace block, kept raw because only its id and
	// name are read.
	Workspace jsontext.Value
}

// String never exposes token-response fields, which may contain credentials.
func (TokenResponse) String() string { return "TokenResponse{[REDACTED]}" }

// GoString returns the redacted diagnostic representation.
func (r TokenResponse) GoString() string { return r.String() }

// Format redacts every formatting verb, including structured formatting.
func (r TokenResponse) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, r.String())
}

// LogValue prevents structured loggers from traversing response members.
func (r TokenResponse) LogValue() slog.Value { return slog.StringValue(r.String()) }

// MarshalJSON redacts the response rather than serializing its secrets.
func (TokenResponse) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo redacts the streaming JSON representation.
func (TokenResponse) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String("[REDACTED]"))
}

// ExchangeAccount is the account block of a token-endpoint response.
type ExchangeAccount struct {
	// UUID is the Anthropic account UUID.
	UUID string `json:"uuid"`
	// EmailAddress is the account's email address.
	EmailAddress *string `json:"email_address"`
}

// ExchangeOrganization is the organization block of a token-endpoint
// response.
type ExchangeOrganization struct {
	// UUID is the organization UUID.
	UUID string `json:"uuid"`
	// Name is the organization's display name.
	Name *string `json:"name"`
}

// readWholeValue reads blob as exactly one JSON value with nothing after
// it, allowing duplicate object names the way the blob's author does.
func readWholeValue(blob []byte) (jsontext.Value, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(blob), jsontext.AllowDuplicateNames(true))
	value, err := dec.ReadValue()
	if err != nil {
		return nil, err
	}
	// The decoder owns the bytes it returned and reuses them on the next
	// read, so the value is copied before the end-of-input probe below.
	whole := jsontext.Value(bytes.Clone(value))
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		memguard.WipeBytes(whole)
		return nil, errors.New("data after the top-level value")
	}
	return whole, nil
}

// rawMember is one object member as it sits in the document.
type rawMember struct {
	name  string
	value jsontext.Value
}

// collapseObject lists an object's members in arrival order, collapsing a
// duplicated name onto its first position with its last value — the same
// rule the reference stores apply, so what this build reads is what a
// store holding that document would mean by it.
func collapseObject(object jsontext.Value) ([]rawMember, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(object), jsontext.AllowDuplicateNames(true))
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	var members []rawMember
	index := make(map[string]int)
	for {
		token, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		if token.Kind() == '}' {
			return members, nil
		}
		name := token.String()
		value, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		// The decoder reuses its buffer across reads, so each kept value is
		// copied; the copies can carry tokens, and the caller wipes them.
		cloned := jsontext.Value(bytes.Clone(value))
		if at, seen := index[name]; seen {
			memguard.WipeBytes(members[at].value)
			members[at].value = cloned
			continue
		}
		index[name] = len(members)
		members = append(members, rawMember{name: name, value: cloned})
	}
}

// wipeMembers wipes the copied member values a collapse produced, because
// any of them can carry a token.
func wipeMembers(members []rawMember) {
	for _, m := range members {
		memguard.WipeBytes(m.value)
	}
}

// hasMember reports whether the collapsed member list carries name.
func hasMember(members []rawMember, name string) bool {
	return memberValue(members, name) != nil
}

// memberValue returns the collapsed member's raw value, or nil.
func memberValue(members []rawMember, name string) jsontext.Value {
	for _, m := range members {
		if m.name == name {
			return m.value
		}
	}
	return nil
}

// takeSecret seals a string-valued member into a [secret.Secret], decoding
// the JSON string straight into a wipeable buffer so the plaintext never
// becomes an immutable string.
func takeSecret(value jsontext.Value) (*secret.Secret, bool) {
	if value.Kind() != '"' {
		return nil, false
	}
	plaintext, err := jsontext.AppendUnquote(nil, value)
	if err != nil {
		memguard.WipeBytes(plaintext)
		return nil, false
	}
	sealed, err := secret.NewSecret(plaintext)
	if err != nil {
		return nil, false
	}
	return sealed, true
}

// takeString unquotes a string-valued member.
func takeString(value jsontext.Value) (string, bool) {
	if value.Kind() != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", false
	}
	return s, true
}

// takeInt64 reads an integer-valued member. A number with a fraction or an
// exponent is not an integer member, whatever its mathematical value: the
// stores this blob round-trips through never write one, so one that
// appears is preserved as an unrecognised member rather than reinterpreted.
func takeInt64(value jsontext.Value) (int64, bool) {
	if !isInt64(value) {
		return 0, false
	}
	n, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// isInt64 reports whether value is an integer JSON number that fits int64.
func isInt64(value jsontext.Value) bool {
	if value.Kind() != '0' || bytes.ContainsAny(value, ".eE") {
		return false
	}
	_, err := strconv.ParseInt(string(value), 10, 64)
	return err == nil
}

// stringItems keeps the string elements of a JSON array, dropping every
// other element the way the blob's reference reader does.
func stringItems(value jsontext.Value) []string {
	var items []any
	if err := json.Unmarshal(value, &items); err != nil {
		return nil
	}
	kept := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			kept = append(kept, s)
		}
	}
	return kept
}

// deadlineMillis is nowMillis + seconds*1000, checked at both steps.
func deadlineMillis(nowMillis, seconds int64, field string) (int64, error) {
	if seconds > math.MaxInt64/1000 || seconds < math.MinInt64/1000 {
		return 0, &ExpiryOverflowError{Field: field}
	}
	deadline, ok := checkedAdd(nowMillis, seconds*1000)
	if !ok {
		return 0, &ExpiryOverflowError{Field: field}
	}
	return deadline, nil
}

// checkedAdd adds two int64 values, reporting whether the sum fits.
func checkedAdd(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}
