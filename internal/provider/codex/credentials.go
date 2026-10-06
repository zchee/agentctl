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

package codex

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/secret"
)

// AccessRefreshMargin renews an access grant before a request can outlive it.
const AccessRefreshMargin = 5 * time.Minute

// LastRefreshInterval bounds the age of a grant without a readable expiry.
const LastRefreshInterval = 8 * 24 * time.Hour

// AccountIDHeader selects the workspace for a usage request.
const AccountIDHeader = "ChatGPT-Account-Id"

// AuthMode is a sanitized explicit or inferred credential mode.
type AuthMode string

const (
	// AuthAPIKey authenticates with an API key, not ChatGPT usage.
	AuthAPIKey AuthMode = "apikey"
	// AuthChatGPT is a native ChatGPT login.
	AuthChatGPT AuthMode = "chatgpt"
	// AuthChatGPTTokens is externally supplied ChatGPT authentication.
	AuthChatGPTTokens AuthMode = "chatgptauthtokens"
	// AuthHeaders has no ChatGPT usage endpoint.
	AuthHeaders AuthMode = "headers"
	// AuthAgentIdentity authenticates an agent identity.
	AuthAgentIdentity AuthMode = "agentidentity"
	// AuthPersonalAccessToken authenticates a personal access token.
	AuthPersonalAccessToken AuthMode = "personalaccesstoken"
	// AuthBedrockAPIKey authenticates a Bedrock API key.
	AuthBedrockAPIKey AuthMode = "bedrockapikey"
	// AuthBedrockAccessKeys authenticates Bedrock access keys.
	AuthBedrockAccessKeys AuthMode = "bedrockaccesskeys"
)

// Label returns the sanitized wire label.
func (m AuthMode) Label() string { return string(m) }

// Identity contains labels only, never tokens.
type Identity struct {
	UserID    string
	AccountID string
	Email     *string
	Plan      *string
}

// CredentialsError reports a fixed structural failure, never a document value.
type CredentialsError struct {
	Kind   string
	Field  string
	Line   int
	Column int
	Cause  error
}

// Error reports the parse rule the document broke.
func (e *CredentialsError) Error() string {
	switch e.Kind {
	case "truncated":
		return "the Codex credential file ends before its JSON does; it may be mid-write"
	case "json":
		return fmt.Sprintf("the Codex credential file is not valid JSON (line %d, column %d)", e.Line, e.Column)
	case "object":
		return "the Codex credential file is not a JSON object"
	case "type":
		return fmt.Sprintf("the Codex credential file's member `%s` has the wrong type", e.Field)
	case "missing":
		return fmt.Sprintf("the Codex credential file has no `%s`", e.Field)
	case "claims":
		return "the id token could not be read: " + e.Cause.Error()
	default:
		return "the Codex credential file could not be used"
	}
}

// Unwrap retains the fixed JWT error for classification.
func (e *CredentialsError) Unwrap() error { return e.Cause }

type authMember struct {
	name  string
	value *authValue
}
type authValue struct {
	kind         byte
	raw          jsontext.Value
	object       []authMember
	array        []*authValue
	sealed       *secret.Secret
	stringSecret bool
}

// AuthDocument preserves member order while sealing known credential leaves.
type AuthDocument struct{ root *authValue }

// Credentials is an ordered credential document with only non-secret labels exposed.
type Credentials struct {
	document    AuthDocument
	mode        AuthMode
	accountID   *string
	claims      *Claims
	accessExp   *int64
	lastRefresh *time.Time
}

// ParseCredentials parses an object, seals tokens, and derives allowlisted labels.
func ParseCredentials(data []byte) (*Credentials, error) {
	decoder := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true))
	root, err := readAuthValue(decoder)
	if err == nil {
		_, err = decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = errors.New("trailing value")
		}
	}
	if err != nil {
		if root != nil {
			root.wipe()
		}
		return nil, credentialJSONError(data, err)
	}
	if root.kind != '{' {
		root.wipe()
		return nil, &CredentialsError{Kind: "object"}
	}
	result := &Credentials{document: AuthDocument{root: root}}
	for _, name := range []string{"OPENAI_API_KEY", "personal_access_token", "bedrock_api_key", "bedrock_access_keys", "agent_identity"} {
		if value := root.member(name); value != nil {
			if err := value.seal(false, name); err != nil {
				root.wipe()
				return nil, err
			}
		}
	}
	tokens := root.member("tokens")
	if tokens != nil && tokens.kind != 'n' {
		if tokens.kind != '{' {
			root.wipe()
			return nil, &CredentialsError{Kind: "type", Field: "tokens"}
		}
		for _, name := range []string{"id_token", "access_token", "refresh_token"} {
			if value := tokens.member(name); value != nil {
				if err := value.seal(true, name); err != nil {
					root.wipe()
					return nil, err
				}
			}
		}
	}
	if err := result.derive(); err != nil {
		root.wipe()
		return nil, err
	}
	return result, nil
}

func credentialJSONError(data []byte, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &CredentialsError{Kind: "truncated"}
	}
	offset := int64(0)
	if syntax, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
		offset = syntax.ByteOffset
	}
	offset = max(min(offset, int64(len(data))), 0)
	line := bytes.Count(data[:offset], []byte("\n")) + 1
	last := bytes.LastIndexByte(data[:offset], '\n')
	return &CredentialsError{Kind: "json", Line: line, Column: int(offset) - last}
}

func readAuthValue(decoder *jsontext.Decoder) (*authValue, error) {
	token, err := decoder.ReadToken()
	if err != nil {
		return nil, err
	}
	value := &authValue{kind: byte(token.Kind())}
	fail := func(err error) (*authValue, error) { value.wipe(); return nil, err }
	switch value.kind {
	case '{':
		indices := make(map[string]int)
		for decoder.PeekKind() != '}' {
			key, err := decoder.ReadToken()
			if err != nil {
				return fail(err)
			}
			name := key.String()
			child, err := readAuthValue(decoder)
			if err != nil {
				return fail(err)
			}
			if index, ok := indices[name]; ok {
				value.object[index].value.wipe()
				value.object[index].value = child
			} else {
				indices[name] = len(value.object)
				value.object = append(value.object, authMember{name, child})
			}
		}
		if _, err := decoder.ReadToken(); err != nil {
			return fail(err)
		}
	case '[':
		for decoder.PeekKind() != ']' {
			child, err := readAuthValue(decoder)
			if err != nil {
				return fail(err)
			}
			value.array = append(value.array, child)
		}
		if _, err := decoder.ReadToken(); err != nil {
			return fail(err)
		}
	default:
		var buffer bytes.Buffer
		enc := jsontext.NewEncoder(&buffer)
		if err := enc.WriteToken(token); err != nil {
			return fail(err)
		}
		value.raw = bytes.Clone(bytes.TrimSpace(buffer.Bytes()))
		memguard.WipeBytes(buffer.Bytes())
	}
	return value, nil
}

func (v *authValue) member(name string) *authValue {
	if v == nil {
		return nil
	}
	for _, member := range v.object {
		if member.name == name {
			return member.value
		}
	}
	return nil
}

func (v *authValue) wipe() {
	memguard.WipeBytes(v.raw)
	for _, member := range v.object {
		member.value.wipe()
	}
	for _, child := range v.array {
		child.wipe()
	}
}

func (v *authValue) seal(mustString bool, name string) error {
	if v.kind == 'n' {
		return nil
	}
	var data []byte
	if v.kind == '"' {
		var text string
		if json.Unmarshal(v.raw, &text) != nil {
			return &CredentialsError{Kind: "type", Field: name}
		}
		data = []byte(text)
		v.stringSecret = true
	} else if mustString {
		return &CredentialsError{Kind: "type", Field: name}
	} else {
		var buffer bytes.Buffer
		if err := v.write(&buffer, 0, false); err != nil {
			return err
		}
		data = buffer.Bytes()
	}
	defer memguard.WipeBytes(data)
	sealed, err := secret.NewSecret(data)
	if err != nil {
		return err
	}
	v.wipe()
	v.kind = 'n'
	v.raw = nil
	v.array = nil
	v.object = nil
	v.sealed = sealed
	return nil
}

func (v *authValue) write(sink io.Writer, depth int, pretty bool) error {
	if v.sealed != nil {
		return v.sealed.WithPlaintext(func(data []byte) error {
			if v.stringSecret {
				quoted, err := jsontext.AppendQuote(nil, string(data))
				if err != nil {
					return err
				}
				defer memguard.WipeBytes(quoted)
				_, err = sink.Write(quoted)
				return err
			}
			node, err := readAuthValue(jsontext.NewDecoder(bytes.NewReader(data)))
			if err != nil {
				return err
			}
			defer node.wipe()
			return node.write(sink, depth, pretty)
		})
	}
	write := func(text string) error { _, err := io.WriteString(sink, text); return err }
	switch v.kind {
	case '{', '[':
		close := "}"
		count := len(v.object)
		if v.kind == '[' {
			close = "]"
			count = len(v.array)
		}
		if err := write(string(v.kind)); err != nil {
			return err
		}
		for i := range count {
			if i > 0 {
				if err := write(","); err != nil {
					return err
				}
			}
			if pretty {
				if err := write("\n" + strings.Repeat("  ", depth+1)); err != nil {
					return err
				}
			}
			var child *authValue
			if v.kind == '{' {
				member := v.object[i]
				name, err := jsontext.AppendQuote(nil, member.name)
				if err != nil {
					return err
				}
				if _, err = sink.Write(name); err != nil {
					return err
				}
				separator := ":"
				if pretty {
					separator = ": "
				}
				if err = write(separator); err != nil {
					return err
				}
				child = member.value
			} else {
				child = v.array[i]
			}
			if err := child.write(sink, depth+1, pretty); err != nil {
				return err
			}
		}
		if pretty && count > 0 {
			if err := write("\n" + strings.Repeat("  ", depth)); err != nil {
				return err
			}
		}
		return write(close)
	case '"':
		var text string
		if err := json.Unmarshal(v.raw, &text); err != nil {
			return err
		}
		quoted, err := jsontext.AppendQuote(nil, text)
		if err != nil {
			return err
		}
		_, err = sink.Write(quoted)
		return err
	case '0':
		raw := string(v.raw)
		if strings.ContainsAny(raw, ".eE") {
			number, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return err
			}
			raw = strconv.FormatFloat(number, 'g', -1, 64)
			if !strings.ContainsAny(raw, ".eE") {
				raw += ".0"
			}
		}
		return write(raw)
	default:
		_, err := sink.Write(v.raw)
		return err
	}
}

func (c *Credentials) derive() error {
	root := c.document.root
	mode := root.member("auth_mode")
	if mode == nil || mode.kind == 'n' {
		c.mode = AuthChatGPT
		for _, entry := range []struct {
			name string
			mode AuthMode
		}{{"personal_access_token", AuthPersonalAccessToken}, {"bedrock_api_key", AuthBedrockAPIKey}, {"bedrock_access_keys", AuthBedrockAccessKeys}, {"OPENAI_API_KEY", AuthAPIKey}} {
			if value := root.member(entry.name); value != nil && value.sealed != nil {
				c.mode = entry.mode
				break
			}
		}
	} else if mode.kind == '"' {
		c.mode = safeAuthMode(*jsonString(mode.raw))
	} else {
		return &CredentialsError{Kind: "type", Field: "auth_mode"}
	}
	tokens := root.member("tokens")
	if account := tokens.member("account_id"); account != nil && account.kind != 'n' {
		if account.kind != '"' {
			return &CredentialsError{Kind: "type", Field: "account_id"}
		}
		c.accountID = jsonString(account.raw)
	}
	if token := c.token("id_token"); token != nil {
		err := token.WithPlaintext(func(data []byte) error {
			claims, err := ParseClaims(data)
			if err != nil {
				return err
			}
			c.claims = &claims
			return nil
		})
		if err != nil {
			return &CredentialsError{Kind: "claims", Cause: err}
		}
	}
	if token := c.token("access_token"); token != nil {
		if err := token.WithPlaintext(func(data []byte) error {
			expiry, err := ParseExpiry(data)
			if err == nil {
				c.accessExp = expiry
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if value := root.member("last_refresh"); value != nil {
		if text := jsonString(value.raw); text != nil {
			if at, err := time.Parse(time.RFC3339Nano, *text); err == nil {
				c.lastRefresh = &at
			}
		}
	}
	return nil
}

func safeAuthMode(value string) AuthMode {
	switch AuthMode(value) {
	case AuthAPIKey, AuthChatGPT, AuthChatGPTTokens, AuthHeaders, AuthAgentIdentity, AuthPersonalAccessToken, AuthBedrockAPIKey, AuthBedrockAccessKeys:
		return AuthMode(value)
	}
	if len(value) > 32 {
		return "<unrecognised>"
	}
	for _, b := range []byte(value) {
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '_' {
			return "<unrecognised>"
		}
	}
	return AuthMode(value)
}

func (c *Credentials) token(name string) *secret.Secret {
	if c == nil || c.document.root == nil {
		return nil
	}
	value := c.document.root.member("tokens").member(name)
	if value == nil {
		return nil
	}
	return value.sealed
}

// WriteJSONTo writes the private ordered document directly to a sink, with no trailing newline.
func (c *Credentials) WriteJSONTo(sink io.Writer) error { return c.document.root.write(sink, 0, true) }

// AuthMode returns the sanitized explicit or inferred mode.
func (c *Credentials) AuthMode() AuthMode { return c.mode }

// HasUsageSource reports a ChatGPT mode with an access token.
func (c *Credentials) HasUsageSource() bool {
	return (c.mode == AuthChatGPT || c.mode == AuthChatGPTTokens) && c.token("access_token") != nil
}

// AccessExpired applies checked expiry arithmetic and the last-refresh fallback.
func (c *Credentials) AccessExpired(now time.Time, margin time.Duration) bool {
	if c.accessExp != nil {
		seconds := int64(margin / time.Second)
		if seconds < 0 || now.Unix() > math.MaxInt64-seconds {
			return true
		}
		return *c.accessExp <= now.Unix()+seconds
	}
	return c.lastRefresh == nil || c.lastRefresh.Before(now.Add(-LastRefreshInterval))
}

// Identity returns the token-file workspace id in preference to the claim's workspace id.
func (c *Credentials) Identity() *Identity {
	if c.claims == nil || c.claims.ChatGPTUserID == nil {
		return nil
	}
	account := c.accountID
	if account == nil {
		account = c.claims.ChatGPTAccountID
	}
	if account == nil {
		return nil
	}
	plan := c.claims.PlanType
	if plan != nil && !IsKnownPlan(*plan) {
		plan = new("unknown")
	}
	return &Identity{UserID: *c.claims.ChatGPTUserID, AccountID: *account, Email: c.claims.Email, Plan: plan}
}

// IdentityDrift reports a disagreement between the stored workspace and refreshed identity.
func (c *Credentials) IdentityDrift() bool {
	return c.accountID != nil && c.claims != nil && c.claims.ChatGPTAccountID != nil && *c.accountID != *c.claims.ChatGPTAccountID
}

// Digests returns fingerprints only when an access token exists.
func (c *Credentials) Digests() *secret.Digests {
	access := c.token("access_token")
	if access == nil {
		return nil
	}
	a, err := access.Digest()
	if err != nil {
		return nil
	}
	result := &secret.Digests{AccessSHA256: a}
	if refresh := c.token("refresh_token"); refresh != nil {
		result.RefreshSHA256, _ = refresh.Digest()
	}
	return result
}

// RefreshDigest8 returns the short refresh fingerprint, when present.
func (c *Credentials) RefreshDigest8() (string, bool) {
	if refresh := c.token("refresh_token"); refresh != nil {
		value, err := refresh.Digest8()
		return value, err == nil
	}
	return "", false
}

// AccessDigest8 returns the short access fingerprint, when present.
func (c *Credentials) AccessDigest8() (string, bool) {
	if access := c.token("access_token"); access != nil {
		value, err := access.Digest8()
		return value, err == nil
	}
	return "", false
}

// LastRefresh returns the last successfully parsed refresh stamp.
func (c *Credentials) LastRefresh() *time.Time { return c.lastRefresh }

// AccessExpiresAt returns the access token's integer expiry.
func (c *Credentials) AccessExpiresAt() *int64 { return c.accessExp }

var knownMembers = []string{"auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh", "agent_identity", "personal_access_token", "bedrock_api_key", "bedrock_access_keys"}

// MissingKnownMembers returns compiled-in field names only.
func (c *Credentials) MissingKnownMembers() []string {
	var result []string
	for _, name := range knownMembers {
		if c.document.root.member(name) == nil {
			result = append(result, name)
		}
	}
	return result
}

// UnknownMemberCount returns only the count, never file-supplied member names.
func (c *Credentials) UnknownMemberCount() int {
	count := 0
	for _, member := range c.document.root.object {
		if !slices.Contains(knownMembers, member.name) {
			count++
		}
	}
	return count
}

// WithAuthorizationHeader scopes a usage request to the token's exposure window.
func (c *Credentials) WithAuthorizationHeader(fn func(string) error) error {
	access := c.token("access_token")
	if access == nil {
		return fn("")
	}
	return access.WithPlaintext(func(data []byte) error { return fn("Bearer " + string(data)) })
}

// AuthorizationHeader implements the provider compatibility seam; callers must not retain the result.
func (c *Credentials) AuthorizationHeader() string {
	var value string
	_ = c.WithAuthorizationHeader(func(header string) error { value = header; return nil })
	return value
}

// ExtraHeaders adds a safe workspace id and the allowlisted FedRAMP flag.
func (c *Credentials) ExtraHeaders() []provider.Header {
	var headers []provider.Header
	if c.accountID != nil && headerSafe(*c.accountID) {
		headers = append(headers, provider.Header{Name: AccountIDHeader, Value: *c.accountID})
	}
	if c.claims != nil && c.claims.IsFedramp {
		headers = append(headers, provider.Header{Name: "X-OpenAI-Fedramp", Value: "true"})
	}
	return headers
}

func headerSafe(value string) bool {
	if value == "" {
		return false
	}
	for _, b := range []byte(value) {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

// String redacts the entire document, including unknown fields.
func (Credentials) String() string { return "Credentials{[REDACTED]}" }

// GoString redacts the entire document.
func (c Credentials) GoString() string { return c.String() }

// Format redacts every formatting verb.
func (c Credentials) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, c.String()) }

// LogValue redacts structured logging.
func (Credentials) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalJSON redacts generic serialization; only WriteJSONTo carries credential bytes.
func (Credentials) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo redacts streaming serialization.
func (Credentials) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("[REDACTED]"))
}

// String prevents the ordered document from being formatted as plaintext.
func (AuthDocument) String() string { return "[REDACTED]" }

// Format prevents every formatting verb from traversing private document fields.
func (AuthDocument) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, "[REDACTED]") }

// LogValue redacts the document.
func (AuthDocument) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalJSON redacts the document.
func (AuthDocument) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo redacts the document.
func (AuthDocument) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("[REDACTED]"))
}

// RefreshResponse holds optional sealed token leaves from a refresh response.
type RefreshResponse struct {
	idToken, accessToken, refreshToken *secret.Secret
	earliest                           *time.Time
}

// ParseRefreshResponse parses token leaves without retaining unrelated server fields.
func ParseRefreshResponse(data []byte) (*RefreshResponse, error) {
	decoder := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true))
	root, err := readAuthValue(decoder)
	if err != nil {
		return nil, credentialJSONError(data, err)
	}
	defer root.wipe()
	if _, err = decoder.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, credentialJSONError(data, err)
	}
	if root.kind != '{' {
		return nil, &CredentialsError{Kind: "object"}
	}
	result := &RefreshResponse{}
	for _, entry := range []struct {
		name   string
		target **secret.Secret
	}{{"id_token", &result.idToken}, {"access_token", &result.accessToken}, {"refresh_token", &result.refreshToken}} {
		if value := root.member(entry.name); value != nil {
			if err := value.seal(true, entry.name); err != nil {
				return nil, err
			}
			*entry.target = value.sealed
		}
	}
	if value := root.member("earliest_refresh_at"); value != nil {
		if seconds := jsonInteger(value.raw); seconds != nil && *seconds >= -62135596800 && *seconds <= 253402300799 {
			result.earliest = new(time.Unix(*seconds, 0).UTC())
		}
	}
	return result, nil
}

// HasAccessToken reports a usable access-token member, including an empty string.
func (r *RefreshResponse) HasAccessToken() bool { return r.accessToken != nil }

// EarliestRefreshAt returns the usable integer server hint.
func (r *RefreshResponse) EarliestRefreshAt() *time.Time { return r.earliest }

// String redacts all token material.
func (RefreshResponse) String() string { return "RefreshResponse{[REDACTED]}" }

// Format redacts all formatting verbs.
func (r RefreshResponse) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, r.String()) }

// LogValue redacts structured output.
func (RefreshResponse) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalJSON redacts token response serialization.
func (RefreshResponse) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo redacts streaming output.
func (RefreshResponse) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("[REDACTED]"))
}
