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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

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
	// Siblings is every top-level member of the document other than
	// claudeAiOauth, in arrival order, with its value kept opaque. Claude
	// Code keeps MCP OAuth tokens, MCP client secrets and plugin secrets
	// there, in the same keychain item as the account credential; they are
	// identities at third parties, so only [Credentials.KeychainBlobJSON]
	// writes them back and no credential file ever receives them.
	Siblings []ExtraMember
}

// SiblingMeta describes one top-level sibling without carrying its value,
// so it can be reported, compared and kept after the value is wiped.
type SiblingMeta struct {
	// Name is the member name.
	Name string
	// Entries is the number of distinct member names of an object-valued
	// sibling, or nil when the value is not an object.
	Entries *int
	// SHA256 is the lowercase hex SHA-256 of the member exactly as
	// [Credentials.KeychainBlobJSON] writes it: the quoted name, a colon
	// and the value.
	SHA256 string
}

// ParseBlob parses a credential blob in Claude Code's shape.
//
// accessToken and expiresAt are required; everything else is optional,
// because older blobs genuinely lack refreshTokenExpiresAt, clientId and
// tokenAccount, and refusing them would hide real accounts from the user.
//
// Every top-level member other than claudeAiOauth is kept in Siblings; a
// duplicated top-level name keeps its first position and its last value,
// the rule every reader of the document applies.
//
// The caller keeps responsibility for wiping blob: it carries the tokens
// in plaintext. The returned credentials are the caller's to dispose of
// with [Credentials.Wipe]; on an error nothing copied from blob is left
// behind.
func ParseBlob(blob []byte) (*Credentials, error) {
	return parseBlob(blob, func(jsontext.Value) {})
}

// parseBlob is [ParseBlob], calling observe with every buffer it copies
// part of blob into, at the moment of the copy. The copies are wiped on
// every error path and none is reachable from outside, so this is the
// only way a test can hold them and check that each was zeroed.
func parseBlob(blob []byte, observe func(jsontext.Value)) (*Credentials, error) {
	root, err := readWholeValue(blob)
	if err != nil {
		return nil, fmt.Errorf("the credentials blob is not valid JSON: %w", err)
	}
	observe(root)
	defer memguard.WipeBytes(root)

	if root.Kind() != '{' {
		return nil, ErrMissingRoot
	}
	rootMembers, err := collapseObject(root)
	if err != nil {
		return nil, fmt.Errorf("the credentials blob is not valid JSON: %w", err)
	}
	defer wipeMembers(rootMembers)
	for _, m := range rootMembers {
		observe(m.value)
	}
	inner := memberValue(rootMembers, BlobRoot)
	if inner == nil || inner.Kind() != '{' {
		return nil, ErrMissingRoot
	}

	members, err := collapseObject(inner)
	if err != nil {
		return nil, fmt.Errorf("the credentials blob is not valid JSON: %w", err)
	}
	defer wipeMembers(members)
	for _, m := range members {
		observe(m.value)
	}
	keep := func(value jsontext.Value) jsontext.Value {
		kept := jsontext.Value(bytes.Clone(value))
		observe(kept)
		return kept
	}

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
			credentials.Extra = append(credentials.Extra, ExtraMember{Name: m.name, Value: keep(m.value)})
			continue
		}
		// A recognised key of the wrong type keeps its place among the
		// unknown members, so writing the blob back does not move it.
		credentials.Extra = append(credentials.Extra, ExtraMember{Name: m.name, Value: keep(m.value)})
	}

	if credentials.AccessToken == nil {
		credentials.Wipe()
		return nil, &MissingFieldError{Field: "accessToken"}
	}
	if !hasMember(members, "expiresAt") || !isInt64(memberValue(members, "expiresAt")) {
		credentials.Wipe()
		return nil, &MissingFieldError{Field: "expiresAt"}
	}

	// The root members are wiped when this function returns, so each kept
	// sibling is a copy the credentials own.
	for _, m := range rootMembers {
		if m.name == BlobRoot {
			continue
		}
		credentials.Siblings = append(credentials.Siblings, ExtraMember{Name: m.name, Value: keep(m.value)})
	}
	return credentials, nil
}

// Wipe zeroes every sibling and unrecognised member value and empties both
// lists, because those values can carry third-party tokens in plaintext.
// The token fields are sealed secrets with their own lifetime and are left
// alone. Wipe is safe on a nil receiver, so an owner can defer it before
// knowing whether a parse succeeded.
func (c *Credentials) Wipe() {
	if c == nil {
		return
	}
	wipeExtraMembers(c.Siblings)
	wipeExtraMembers(c.Extra)
	c.Siblings = nil
	c.Extra = nil
}

// CarrySiblingsFrom replaces these credentials' siblings with a copy of
// src's, wiping the ones they held. A nil src clears them: a destination
// with no keychain item has nothing to carry, and siblings that arrived
// from anywhere else must not reach the item.
func (c *Credentials) CarrySiblingsFrom(src *Credentials) {
	// Wiping first would destroy the very values being carried.
	if src == c {
		return
	}
	wipeExtraMembers(c.Siblings)
	c.Siblings = nil
	if src == nil || len(src.Siblings) == 0 {
		return
	}
	c.Siblings = make([]ExtraMember, 0, len(src.Siblings))
	for _, m := range src.Siblings {
		c.Siblings = append(c.Siblings, ExtraMember{Name: m.Name, Value: bytes.Clone(m.Value)})
	}
}

// SiblingSummary returns the name, entry count and digest of every sibling
// in order: metadata that carries no value and outlives [Credentials.Wipe].
//
// The digest covers the bytes the keychain serializer emits for the member,
// not the bytes it arrived as, so two documents that differ only in
// insignificant whitespace or escaping summarize equal.
func (c *Credentials) SiblingSummary() ([]SiblingMeta, error) {
	summary := make([]SiblingMeta, 0, len(c.Siblings))
	for _, m := range c.Siblings {
		digest, err := siblingDigest(m)
		if err != nil {
			return nil, fmt.Errorf("the credentials blob member `%s` is not valid JSON: %w", m.Name, err)
		}
		meta := SiblingMeta{Name: m.Name, SHA256: digest}
		if m.Value.Kind() == '{' {
			entries, err := objectEntries(m.Value)
			if err != nil {
				return nil, fmt.Errorf("the credentials blob member `%s` is not valid JSON: %w", m.Name, err)
			}
			meta.Entries = &entries
		}
		summary = append(summary, meta)
	}
	return summary, nil
}

// BlobJSON serializes the blob, ready to be written to its store.
//
// Known members are emitted in the order Claude Code writes them, then
// every unrecognised member in the order it arrived. Absent optional
// members are omitted rather than written as null, which is what Claude
// Code does and what makes the round-trip of an old blob byte-stable.
//
// Siblings are never written: this is the document credential files hold,
// and those files must not receive third-party identities.
//
// The output carries the tokens in plaintext. It exists only to be handed
// to a credential store, and the caller must wipe it after that I/O.
func (c *Credentials) BlobJSON() ([]byte, error) {
	bound, err := c.documentBound(false)
	if err != nil {
		return nil, err
	}
	return c.encodeDocument(encoderBacking(bound), false)
}

// KeychainBlobJSON serializes the whole keychain document: claudeAiOauth
// as [Credentials.BlobJSON] writes it, then every sibling in order.
//
// Sibling values are written as the decoder read them, re-encoded under
// options that accept a duplicated name and invalid UTF-8 anywhere inside
// a value, so a document Claude Code wrote never makes the write fail.
// Re-encoding removes insignificant whitespace and normalizes escapes;
// what is preserved is the value, and [Credentials.SiblingSummary]
// digests the re-encoded bytes for that reason.
//
// The output carries the account tokens and every sibling's secrets in
// plaintext. It exists only to be handed to the keychain, and the caller
// must wipe it after that I/O.
func (c *Credentials) KeychainBlobJSON() ([]byte, error) {
	bound, err := c.documentBound(true)
	if err != nil {
		return nil, err
	}
	return c.encodeDocument(encoderBacking(bound), true)
}

// encodeDocument writes the credential document into backing: the
// account object, then, when siblings is set, every sibling under the
// keychain options.
func (c *Credentials) encodeDocument(backing []byte, siblings bool) ([]byte, error) {
	var opts []jsontext.Options
	if siblings {
		opts = keychainValueOptions
	}
	return encodeInto(backing, opts, func(enc *jsontext.Encoder) error {
		if err := writeTokens(enc, jsontext.BeginObject, jsontext.String(BlobRoot)); err != nil {
			return err
		}
		if err := c.writeAccountObject(enc); err != nil {
			return err
		}
		if siblings {
			emitted := map[string]bool{BlobRoot: true}
			for _, m := range c.Siblings {
				// A second member under a name already written would leave
				// the meaning of the document to whichever reader parses
				// it; the account credential and the first sibling of a
				// name are the ones written.
				if emitted[m.Name] {
					continue
				}
				emitted[m.Name] = true
				if err := enc.WriteToken(jsontext.String(m.Name)); err != nil {
					return err
				}
				if err := enc.WriteValue(m.Value); err != nil {
					return fmt.Errorf("the credentials blob member `%s` is not valid JSON: %w", m.Name, err)
				}
			}
		}
		return enc.WriteToken(jsontext.EndObject)
	})
}

// documentBound is an upper bound on the length of the credential
// document, trailing newline included, computed from what every field
// encodes to rather than from its decoded length: an escaped name or a
// token full of characters that need escaping is several times longer
// written than read.
func (c *Credentials) documentBound(siblings bool) (int, error) {
	// The root name, the account object's member names, punctuation, two
	// integers of at most 20 digits and an all-null tokenAccount fit well
	// within this; everything of variable length is added below.
	const skeleton = 512
	n := skeleton
	for _, token := range []*secret.Secret{c.AccessToken, c.RefreshToken} {
		if token == nil {
			continue
		}
		if err := token.WithPlaintext(func(b []byte) error {
			n += quotedBound(b)
			return nil
		}); err != nil {
			return 0, err
		}
	}
	for _, scope := range c.Scopes {
		n += quotedBound(scope) + len(",")
	}
	for _, optional := range []*string{c.SubscriptionType, c.RateLimitTier, c.ClientID} {
		if optional != nil {
			n += quotedBound(*optional)
		}
	}
	if account := c.TokenAccount; account != nil {
		for _, field := range []*string{
			account.UUID, account.EmailAddress, account.OrganizationUUID,
			account.OrganizationName, account.WorkspaceID, account.WorkspaceName,
		} {
			if field != nil {
				n += quotedBound(*field)
			}
		}
	}
	for _, m := range c.Extra {
		n += memberBound(m)
	}
	if siblings {
		for _, m := range c.Siblings {
			n += memberBound(m)
		}
	}
	return n, nil
}

// memberBound is an upper bound on the bytes one member occupies once
// written: its quoted name, a colon, the re-encoded value and a comma,
// plus the braces and newline of a one-member document.
func memberBound(m ExtraMember) int {
	return len("{") + quotedBound(m.Name) + len(":") + encodedBound(m.Value) + len(",}\n")
}

// quotedBound is an upper bound on the length of s written as a JSON
// string, quotes included.
func quotedBound[T ~string | ~[]byte](s T) int {
	return len(`""`) + encodedBound(s)
}

// encodedBound is an upper bound on what the encoder emits for b, whether
// b is quoted as a string or is a JSON value being re-encoded.
//
// No input byte becomes more output than its cost here: a control
// character or an HTML-sensitive one may become a six-byte \u escape, a
// quote or backslash a two-byte escape, and a byte of a multi-byte
// sequence either itself or, when invalid, the three-byte replacement
// character (U+2028 and U+2029 escape to six bytes from three). When b is
// a value, an escape sequence it already carries is never written longer
// than it arrived, whitespace between tokens is dropped, and numbers and
// literals are copied as they arrived: no serializer here enables the
// options that canonicalize numbers, which could lengthen them.
func encodedBound[T ~string | ~[]byte](b T) int {
	n := 0
	for i := range len(b) {
		switch c := b[i]; {
		case c < 0x20, c == '<', c == '>', c == '&':
			n += len(`\u0000`)
		case c == '"', c == '\\':
			n += len(`\"`)
		case c >= utf8.RuneSelf:
			n += len("\ufffd")
		default:
			n++
		}
	}
	return n
}

// encoderBacking returns the array an encoder bounded by bound writes
// into without ever reallocating it.
//
// A reallocation copies the plaintext written so far and leaves the old
// array where nothing can wipe it. Within bound no append outgrows the
// array, and the extra quarter covers the encoder's own flush, which grows
// a bytes.Buffer whenever less than a quarter of its length is free.
func encoderBacking(bound int) []byte {
	return make([]byte, 0, bound+bound/4+1)
}

// encodeInto runs write against an encoder that appends directly into
// backing, and returns the value it wrote without the trailing newline.
//
// The newline is cleared from the array as well, so wiping the result
// wipes everything the encoder wrote. On error everything written is
// wiped before returning.
func encodeInto(backing []byte, opts []jsontext.Options, write func(*jsontext.Encoder) error) ([]byte, error) {
	buffer := bytes.NewBuffer(backing)
	if err := write(jsontext.NewEncoder(buffer, opts...)); err != nil {
		// The encoder writes past the buffer's length before it flushes,
		// so the whole capacity is wiped, not only what was flushed.
		written := buffer.Bytes()
		memguard.WipeBytes(written[:cap(written)])
		memguard.WipeBytes(backing[:cap(backing)])
		return nil, err
	}
	full := buffer.Bytes()
	out := bytes.TrimSuffix(full, []byte("\n"))
	clear(full[len(out):])
	return out, nil
}

// keychainValueOptions are the options sibling values are written and read
// under. The parser already accepted a duplicated name, and Claude Code's
// own serializer never emits invalid UTF-8; accepting both keeps the write
// from failing on a value this build does not own.
var keychainValueOptions = []jsontext.Options{
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
}

// writeTokens writes each token in order.
func writeTokens(enc *jsontext.Encoder, tokens ...jsontext.Token) error {
	for _, token := range tokens {
		if err := enc.WriteToken(token); err != nil {
			return err
		}
	}
	return nil
}

// writeAccountObject writes the claudeAiOauth object: known members in the
// order Claude Code writes them, then every unrecognised member in the
// order it arrived.
func (c *Credentials) writeAccountObject(enc *jsontext.Encoder) error {
	write := func(tokens ...jsontext.Token) error {
		return writeTokens(enc, tokens...)
	}
	if err := write(jsontext.BeginObject); err != nil {
		return err
	}

	emitted := make(map[string]bool)
	writeSecretMember := func(name string, token *secret.Secret) error {
		if err := write(jsontext.String(name)); err != nil {
			return err
		}
		emitted[name] = true
		var quoted []byte
		err := token.WithPlaintext(func(b []byte) error {
			// Sized for the escaped token, so quoting never reallocates
			// and leaves a copy behind that the wipe below cannot reach.
			var quoteErr error
			if quoted, quoteErr = jsontext.AppendQuote(make([]byte, 0, quotedBound(b)), b); quoteErr != nil {
				return quoteErr
			}
			return enc.WriteValue(quoted)
		})
		memguard.WipeBytes(quoted)
		return err
	}

	if err := writeSecretMember("accessToken", c.AccessToken); err != nil {
		return err
	}
	if c.RefreshToken != nil {
		if err := writeSecretMember("refreshToken", c.RefreshToken); err != nil {
			return err
		}
	}
	if err := write(jsontext.String("expiresAt"), jsontext.Int(c.ExpiresAtMillis)); err != nil {
		return err
	}
	emitted["expiresAt"] = true
	if c.RefreshTokenExpiresAtMillis != nil {
		if err := write(jsontext.String("refreshTokenExpiresAt"), jsontext.Int(*c.RefreshTokenExpiresAtMillis)); err != nil {
			return err
		}
		emitted["refreshTokenExpiresAt"] = true
	}
	if err := write(jsontext.String("scopes"), jsontext.BeginArray); err != nil {
		return err
	}
	for _, scope := range c.Scopes {
		if err := write(jsontext.String(scope)); err != nil {
			return err
		}
	}
	if err := write(jsontext.EndArray); err != nil {
		return err
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
			return err
		}
		emitted[optional.name] = true
	}
	if c.TokenAccount != nil {
		if err := write(jsontext.String("tokenAccount")); err != nil {
			return err
		}
		if err := json.MarshalEncode(enc, c.TokenAccount); err != nil {
			return err
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
			return err
		}
		if err := enc.WriteValue(extra.Value); err != nil {
			return err
		}
	}
	return write(jsontext.EndObject)
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
	scope := strings.Join(c.Scopes, " ")
	bound, err := c.refreshBodyBound(clientID, scope)
	if err != nil {
		return nil, err
	}
	return c.encodeRefreshBody(encoderBacking(bound), clientID, scope)
}

// refreshBodyBound is an upper bound on the length of the refresh body,
// trailing newline included, computed from what each field encodes to,
// for the reason [Credentials.documentBound] gives.
func (c *Credentials) refreshBodyBound(clientID, scope string) (int, error) {
	// The member names, the grant type and the punctuation fit well within
	// this; everything of variable length is added below.
	const skeleton = 128
	n := skeleton + quotedBound(clientID) + quotedBound(scope)
	if err := c.RefreshToken.WithPlaintext(func(b []byte) error {
		n += quotedBound(b)
		return nil
	}); err != nil {
		return 0, err
	}
	return n, nil
}

// encodeRefreshBody writes the refresh body into backing.
func (c *Credentials) encodeRefreshBody(backing []byte, clientID, scope string) ([]byte, error) {
	return encodeInto(backing, nil, func(enc *jsontext.Encoder) error {
		if err := writeTokens(enc,
			jsontext.BeginObject,
			jsontext.String("grant_type"), jsontext.String("refresh_token"),
			jsontext.String("refresh_token"),
		); err != nil {
			return err
		}
		var quoted []byte
		err := c.RefreshToken.WithPlaintext(func(b []byte) error {
			// Sized for the escaped token, as in writeAccountObject.
			var quoteErr error
			if quoted, quoteErr = jsontext.AppendQuote(make([]byte, 0, quotedBound(b)), b); quoteErr != nil {
				return quoteErr
			}
			return enc.WriteValue(quoted)
		})
		memguard.WipeBytes(quoted)
		if err != nil {
			return err
		}
		return writeTokens(enc,
			jsontext.String("client_id"), jsontext.String(clientID),
			jsontext.String("scope"), jsontext.String(scope),
			jsontext.EndObject,
		)
	})
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

// wipeExtraMembers wipes the kept member values, because any of them can
// carry a token.
func wipeExtraMembers(members []ExtraMember) {
	for _, m := range members {
		memguard.WipeBytes(m.Value)
	}
}

// siblingDigest is the SHA-256 of the member exactly as the keychain
// serializer writes it inside the document: the quoted name, a colon and
// the re-encoded value.
func siblingDigest(m ExtraMember) (string, error) {
	backing := encoderBacking(memberBound(m))
	// The scratch encoding carries the value's secrets.
	defer memguard.WipeBytes(backing[:cap(backing)])
	object, err := encodeMember(backing, m)
	if err != nil {
		return "", err
	}
	// A one-member object holds, between its braces, exactly the bytes the
	// member occupies in the full document.
	sum := sha256.Sum256(object[1 : len(object)-1])
	return hex.EncodeToString(sum[:]), nil
}

// encodeMember writes m into backing as a one-member object under the
// keychain options.
func encodeMember(backing []byte, m ExtraMember) ([]byte, error) {
	return encodeInto(backing, keychainValueOptions, func(enc *jsontext.Encoder) error {
		if err := writeTokens(enc, jsontext.BeginObject, jsontext.String(m.Name)); err != nil {
			return err
		}
		if err := enc.WriteValue(m.Value); err != nil {
			return err
		}
		return enc.WriteToken(jsontext.EndObject)
	})
}

// objectEntries counts the distinct member names of an object value, so a
// duplicated name counts once, as every reader of the document sees it.
func objectEntries(object jsontext.Value) (int, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(object), keychainValueOptions...)
	if _, err := dec.ReadToken(); err != nil {
		return 0, err
	}
	names := make(map[string]struct{})
	for {
		token, err := dec.ReadToken()
		if err != nil {
			return 0, err
		}
		if token.Kind() == '}' {
			return len(names), nil
		}
		names[token.String()] = struct{}{}
		if err := dec.SkipValue(); err != nil {
			return 0, err
		}
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
