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

// The non-interactive half of the OAuth client: the refresh-token grant and
// the profile GET. These are the two calls a usage pass makes on its own,
// without a user at the terminal.
//
// The interactive login — building the authorize URL, the PKCE material,
// the loopback callback listener, the browser opener and the
// authorization-code exchange — is deliberately absent here and arrives
// with the login command. Nothing in this file mints a grant from anything
// but a stored refresh token.
//
// Secrets. The refresh POST body is built by [Credentials.RefreshBody], so
// the refresh token's plaintext stays inside that type's exposure path, and
// the body buffer is wiped after the request that consumes it. The profile
// GET's bearer header arrives finished from
// [Credentials.AuthorizationHeader]. A successful token response carries
// two tokens; its bytes are parsed straight into [secret.Secret] values and
// wiped. Copies made by net/http and the garbage collector are outside the
// guarantee, as everywhere else in this module.
//
// Errors. A failure is classified into the vocabulary the caller branches
// on: invalid_grant becomes [errs.AuthError] with the flag set, because a
// dead refresh chain needs a fresh login and nothing else; any other
// unusable status becomes [errs.HTTPError], carrying the Retry-After hint
// when the server sent one; a request that never completed is a transport
// [provider.FetchError]; a response that arrived but could not be read is a
// parse one. An error message never carries token material: response bodies
// are redacted before they are truncated, so a cut cannot leave a token
// prefix behind.
//
// The refresh itself never retries. A 429 from the token endpoint does not
// consume the grant, so retrying is safe — but the refresh path runs with
// the namespace lock held, and waiting inside it would block every other
// writer. The caller owns the decision, and [RateLimitRetryFloor] is the
// floor on any wait it chooses.

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
)

// TokenTimeout is the time budget for one call to the token endpoint.
const TokenTimeout = 30 * time.Second

// ProfileTimeout is the time budget for the profile call.
const ProfileTimeout = 10 * time.Second

// RateLimitRetryFloor is the floor on the wait before a rate-limited token
// request is retried.
//
// The token endpoint has been observed answering 429 with no Retry-After
// header, so there is often nothing to honour and a floor is what is left.
// The refresh in this file never waits itself — it runs under the namespace
// lock — so the floor is exported for the caller that decides to retry.
const RateLimitRetryFloor = 5 * time.Second

// MaxErrorBodyBytes is how much of a failing response body is kept for the
// error message.
const MaxErrorBodyBytes = 512

// MaxOAuthBodyBytes is the largest token or profile response this build
// will read.
const MaxOAuthBodyBytes = 1 << 20

// TokenRefresher mints a new access token from stored credentials.
//
// The production implementation is [OAuthClient]; pass tests substitute
// doubles so no unit test can reach the real token endpoint.
type TokenRefresher interface {
	// RefreshAccess trades the stored refresh token for a new access
	// token, folding the response into credentials in place.
	RefreshAccess(ctx context.Context, credentials *Credentials) (*Credentials, error)
}

// OAuthClient talks to the token and profile endpoints.
//
// It holds one HTTP client per endpoint so each call class keeps its own
// whole-request budget: [TokenTimeout] for the token endpoint,
// [ProfileTimeout] for the profile.
type OAuthClient struct {
	tokenURL      string
	profileURL    string
	clientID      string
	userAgent     string
	tokenClient   *http.Client
	profileClient *http.Client
}

var _ TokenRefresher = (*OAuthClient)(nil)

// NewOAuthClient builds a client against explicit endpoints.
//
// The production path reaches it through [NewOAuthClientFromEnv]; tests
// reach it directly with their own server's URL rather than by mutating
// the process environment.
func NewOAuthClient(tokenURL, profileURL, userAgent string) (*OAuthClient, error) {
	for _, endpoint := range []string{tokenURL, profileURL} {
		if _, err := url.ParseRequestURI(endpoint); err != nil {
			return nil, errs.NewConfig(fmt.Sprintf("`%s` is not a usable OAuth endpoint", endpoint))
		}
	}
	return &OAuthClient{
		tokenURL:      tokenURL,
		profileURL:    profileURL,
		clientID:      ClientID,
		userAgent:     userAgent,
		tokenClient:   &http.Client{Timeout: TokenTimeout},
		profileClient: &http.Client{Timeout: ProfileTimeout},
	}, nil
}

// NewOAuthClientFromEnv builds the client this process should use: the
// built-in endpoint selection (see the build-tag pair for what that means
// per build) with the shared User-Agent.
func NewOAuthClientFromEnv() (*OAuthClient, error) {
	return NewOAuthClient(oauthTokenURL(), oauthProfileURL(), provider.UserAgent(provider.Claude))
}

// RefreshAccess trades the stored refresh token for a new access token.
//
// One POST, never retried here: a 429 does not consume the grant, but this
// path runs under the namespace lock and waiting inside it would block
// every other writer, so the caller decides about retrying with
// [RateLimitRetryFloor] as the floor. A successful response is folded into
// credentials through [Credentials.MergeRefresh] — a response without a
// refresh token keeps the stored one — and credentials itself is returned,
// merged in place.
//
// invalid_grant means the refresh chain is dead and only a fresh login
// recovers it; it surfaces as [errs.AuthError] with the flag set and is
// never worth retrying.
func (c *OAuthClient) RefreshAccess(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	if ctx.Err() != nil {
		return nil, provider.NewFetchCancelled()
	}
	body, err := credentials.RefreshBody(c.clientID)
	if err != nil {
		return nil, err
	}
	// The body carries the refresh token in plaintext; it is wiped after
	// the one request that consumes it.
	defer memguard.WipeBytes(body)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewFetchTransport(err.Error())
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)

	response, err := c.tokenClient.Do(request)
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
	// A successful body carries both tokens in plaintext.
	defer memguard.WipeBytes(text)

	if status := response.StatusCode; status < 200 || status > 299 {
		return nil, classifyTokenFailure(status, response.Header, text)
	}

	token, err := parseTokenResponse(text)
	if err != nil {
		return nil, provider.NewFetchParse("the token response could not be parsed: " + err.Error())
	}
	if err := credentials.MergeRefresh(token, time.Now().UnixMilli()); err != nil {
		return nil, provider.NewFetchParse("the token response could not be used: " + err.Error())
	}
	return credentials, nil
}

// readOAuthBody reads a token or profile response body under the
// [MaxOAuthBodyBytes] ceiling, one byte past it so an oversized body is
// told apart from one exactly at it.
func readOAuthBody(body io.Reader) ([]byte, error) {
	text, err := io.ReadAll(io.LimitReader(body, MaxOAuthBodyBytes+1))
	if err != nil {
		memguard.WipeBytes(text)
		if isTimeout(err) {
			return nil, provider.NewFetchTransport(err.Error())
		}
		return nil, provider.NewFetchParse(err.Error())
	}
	if len(text) > MaxOAuthBodyBytes {
		memguard.WipeBytes(text)
		return nil, provider.NewFetchParse(fmt.Sprintf("the response body exceeds %d bytes", MaxOAuthBodyBytes))
	}
	return text, nil
}

// classifyTokenFailure maps a non-2xx token-endpoint answer onto the
// caller's vocabulary.
//
// invalid_grant is checked first, whatever the status: the body is the
// authoritative statement that the chain is dead, and a chain reported as
// a transient HTTP failure would send the user to wait instead of to log
// in. Everything else is an [errs.HTTPError] carrying the redacted body,
// with the Retry-After hint attached when a 429 sent one.
func classifyTokenFailure(status int, header http.Header, body []byte) error {
	if bodySaysInvalidGrant(body) {
		return errs.NewAuth(true)
	}
	kind := errs.NewHTTP(status)
	if status == http.StatusTooManyRequests {
		if retryAfter, ok := ParseRetryAfter(header.Get("Retry-After"), time.Now()); ok {
			kind = errs.NewHTTPRetryAfter(status, retryAfter)
		}
	}
	if redacted := redactBody(string(body)); redacted != "" {
		return fmt.Errorf("%w: %s", kind, redacted)
	}
	return kind
}

// bodySaysInvalidGrant reports whether an error body says invalid_grant.
//
// Both shapes seen in the wild are accepted: a bare `"error":
// "invalid_grant"` and the nested `"error": {"type": "invalid_grant"}` the
// endpoint uses for its typed errors.
func bodySaysInvalidGrant(body []byte) bool {
	var raw jsontext.Value
	if err := json.Unmarshal(body, &raw, jsontext.AllowDuplicateNames(true)); err != nil {
		return false
	}
	members, ok := objectMembers(raw)
	if !ok {
		return false
	}
	value, ok := field(members, "error")
	if !ok {
		return false
	}
	switch value.Kind() {
	case '"':
		text, ok := takeString(value)
		return ok && text == "invalid_grant"
	case '{':
		inner, ok := objectMembers(value)
		if !ok {
			return false
		}
		text, ok := stringField(inner, "type")
		return ok && text == "invalid_grant"
	default:
		return false
	}
}

// parseTokenResponse parses a successful token-endpoint body.
//
// access_token and expires_in are required; everything else is optional.
// Unknown members are ignored rather than rejected, which is what lets the
// endpoint add fields without breaking this build. The tokens are decoded
// straight into sealed secrets so their plaintext never becomes an
// immutable string; the caller keeps responsibility for wiping blob.
func parseTokenResponse(blob []byte) (*TokenResponse, error) {
	root, err := readWholeValue(blob)
	if err != nil {
		return nil, fmt.Errorf("the token response is not valid JSON: %w", err)
	}
	defer memguard.WipeBytes(root)
	if root.Kind() != '{' {
		return nil, errors.New("the token response is not a JSON object")
	}
	members, err := collapseObject(root)
	if err != nil {
		return nil, fmt.Errorf("the token response is not valid JSON: %w", err)
	}
	defer wipeMembers(members)

	response := &TokenResponse{}
	sawAccess, sawExpires := false, false
	for _, m := range members {
		switch m.name {
		case "access_token":
			if token, ok := takeSecret(m.value); ok {
				response.AccessToken = token
				sawAccess = true
			}
		case "refresh_token":
			if token, ok := takeSecret(m.value); ok {
				response.RefreshToken = token
			}
		case "expires_in":
			if n, ok := takeInt64(m.value); ok {
				response.ExpiresIn = n
				sawExpires = true
			}
		case "refresh_token_expires_in":
			if n, ok := takeInt64(m.value); ok {
				response.RefreshTokenExpiresIn = &n
			}
		case "scope":
			if s, ok := takeString(m.value); ok {
				response.Scope = &s
			}
		case "token_type":
			if s, ok := takeString(m.value); ok {
				response.TokenType = &s
			}
		case "account":
			var account ExchangeAccount
			if err := json.Unmarshal(m.value, &account); err == nil {
				response.Account = &account
			}
		case "organization":
			var organization ExchangeOrganization
			if err := json.Unmarshal(m.value, &organization); err == nil {
				response.Organization = &organization
			}
		case "workspace":
			response.Workspace = bytes.Clone(m.value)
		}
	}

	if !sawAccess {
		return nil, errors.New("the member `access_token` is missing or has the wrong type")
	}
	if !sawExpires {
		return nil, errors.New("the member `expires_in` is missing or has the wrong type")
	}
	return response, nil
}

// redactBody makes a response body safe to put in an error message.
//
// Token material — every sk-ant run — is replaced before the length limit
// is applied, so a truncation cannot leave a token prefix behind. The cut
// point walks back to a rune boundary rather than slicing a multi-byte
// character in half.
func redactBody(body string) string {
	var builder strings.Builder
	rest := body
	for {
		start := strings.Index(rest, "sk-ant")
		if start < 0 {
			break
		}
		builder.WriteString(rest[:start])
		builder.WriteString("<redacted>")
		tail := rest[start:]
		end := strings.IndexFunc(tail, func(r rune) bool {
			tokenByte := '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || r == '-' || r == '_'
			return !tokenByte
		})
		if end < 0 {
			end = len(tail)
		}
		rest = tail[end:]
	}
	builder.WriteString(rest)

	out := builder.String()
	if len(out) <= MaxErrorBodyBytes {
		return out
	}
	cut := MaxErrorBodyBytes
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + "…"
}
