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
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/provider/claude"
)

// TokenURL is the production refresh endpoint.
const TokenURL = "https://auth.openai.com/oauth/token"

// ClientID is the OAuth application's client identifier.
const ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// AmbiguousClass qualifies an unknown refresh outcome.
type AmbiguousClass string

const (
	// AmbiguousTransport means the request may have reached the server.
	AmbiguousTransport AmbiguousClass = "transport"
	// AmbiguousServerBody means a response could not be used.
	AmbiguousServerBody AmbiguousClass = "server_body"
	// AmbiguousTLS means a TLS failure could not prove absence of a send.
	AmbiguousTLS AmbiguousClass = "tls"
	// AmbiguousWriteFailed means a rotated credential could not be saved.
	AmbiguousWriteFailed AmbiguousClass = "write_failed"
)

// Label returns the fixed word rendered in status and audit records.
func (c AmbiguousClass) Label() string {
	if c == AmbiguousTransport || c == AmbiguousServerBody {
		return "ambiguous"
	}
	return string(c)
}

// PermanentClass is the server's dead-grant answer.
type PermanentClass string

const (
	// PermanentUnauthorized means an HTTP 401.
	PermanentUnauthorized PermanentClass = "unauthorized"
	// PermanentInvalidGrant means a 400 invalid_grant.
	PermanentInvalidGrant PermanentClass = "invalid_grant"
	// PermanentExpired means the refresh grant expired.
	PermanentExpired PermanentClass = "refresh_token_expired"
	// PermanentReused means the grant was reused.
	PermanentReused PermanentClass = "refresh_token_reused"
	// PermanentInvalidated means the grant was invalidated.
	PermanentInvalidated PermanentClass = "refresh_token_invalidated"
)

// RefreshOutcomeKind describes the policy outcome of a single POST.
type RefreshOutcomeKind string

const (
	// RefreshApplied carries a usable rotated credential.
	RefreshApplied RefreshOutcomeKind = "applied"
	// RefreshPermanent means the grant is dead.
	RefreshPermanent RefreshOutcomeKind = "permanent"
	// RefreshPreSend proves the request was never sent.
	RefreshPreSend RefreshOutcomeKind = "pre_send"
	// RefreshRejected means a non-429 4xx answered before processing.
	RefreshRejected RefreshOutcomeKind = "rejected"
	// RefreshRateLimited means the token may have been consumed on a 429.
	RefreshRateLimited RefreshOutcomeKind = "rate_limited"
	// RefreshServerError means the token may have been consumed on a 5xx.
	RefreshServerError RefreshOutcomeKind = "server_error"
	// RefreshAmbiguous means the token may have been consumed.
	RefreshAmbiguous RefreshOutcomeKind = "ambiguous"
)

// RefreshOutcome carries fixed policy facts, never a raw response body or transport error.
type RefreshOutcome struct {
	Kind       RefreshOutcomeKind
	Response   *RefreshResponse
	Permanent  PermanentClass
	Ambiguous  AmbiguousClass
	Reason     string
	Status     int
	RetryAfter *time.Duration
}

// String renders classes only, without the received credential.
func (o RefreshOutcome) String() string {
	return fmt.Sprintf("RefreshOutcome{%s %s %s %d}", o.Kind, o.Permanent, o.Ambiguous, o.Status)
}

// GoString renders classes only.
func (o RefreshOutcome) GoString() string { return o.String() }

// Format keeps every formatting verb from inspecting the credential carrier.
func (o RefreshOutcome) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, o.String()) }

// LogValue renders classes only.
func (o RefreshOutcome) LogValue() slog.Value { return slog.StringValue(o.String()) }

// MarshalJSONTo prevents streaming JSON from inspecting the credential carrier.
func (o RefreshOutcome) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String(o.String()))
}

// MarshalJSON prevents token-bearing values from entering ordinary JSON output.
func (o RefreshOutcome) MarshalJSON() ([]byte, error) { return json.Marshal(o.String()) }

// InflightToken authorizes one send after a durable marker; only the state writer can create it.
type InflightToken struct {
	authorization *inflightAuthorization
}

type inflightAuthorization struct {
	digest8 string
	lock    *Lock
	claimed atomic.Bool
}

// RefreshClient owns the single-send transport and a token endpoint.
type RefreshClient struct {
	tokenURL  string
	userAgent string
	transport *refreshClient
}

// NewRefreshClient constructs a client with independently bounded transport phases.
func NewRefreshClient(tokenURL, userAgent string) *RefreshClient {
	return &RefreshClient{tokenURL: tokenURL, userAgent: userAgent, transport: newRefreshClient()}
}

// NewRefreshClientFromEnv uses the production endpoint or the build-tagged loopback seam.
func NewRefreshClientFromEnv() *RefreshClient {
	return NewRefreshClient(codexTokenURL(), provider.UserAgent(provider.Codex))
}

// TokenURL returns the configured endpoint.
func (c *RefreshClient) TokenURL() string { return c.tokenURL }

func refreshOAuth(ctx context.Context, credentials *LockedCredentials, token *InflightToken, client *RefreshClient) RefreshOutcome {
	if token == nil || token.authorization == nil || token.authorization.claimed.Swap(true) {
		return RefreshOutcome{Kind: RefreshPreSend, Reason: "the marker's send authorization was already consumed"}
	}
	if credentials == nil || client == nil || client.transport == nil || token.authorization.lock != credentials.Lock() {
		return RefreshOutcome{Kind: RefreshPreSend, Reason: "the marker's send authorization belongs to another lock"}
	}
	grant, ok := credentials.RefreshDigest8()
	if !ok || grant != token.authorization.digest8 {
		return RefreshOutcome{Kind: RefreshPreSend, Reason: "the marker records a different grant than the one to send"}
	}
	if ctx.Err() != nil {
		return RefreshOutcome{Kind: RefreshPreSend, Reason: "cancelled before the send"}
	}
	var result Outcome
	err := credentials.WithRefreshBody(ClientID, func(body []byte) error {
		result = client.transport.post(ctx, client.tokenURL, client.userAgent, body)
		return nil
	})
	if err != nil {
		return RefreshOutcome{Kind: RefreshPreSend, Reason: "the request body could not be built"}
	}
	defer memguard.WipeBytes(result.Body)
	if result.Kind == OutcomeNotSent {
		return RefreshOutcome{Kind: RefreshPreSend, Reason: result.Reason}
	}
	if result.Kind == OutcomeUnknown {
		class := AmbiguousTransport
		if result.Class == UnknownTLS {
			class = AmbiguousTLS
		}
		return RefreshOutcome{Kind: RefreshAmbiguous, Ambiguous: class}
	}
	var retryAfter *time.Duration
	if wait, ok := claude.ParseRetryAfter(result.Header.Get("Retry-After"), time.Now()); ok {
		retryAfter = new(wait)
	}
	return classifyRefreshResponse(result.Status, retryAfter, result.Body, result.BodyComplete)
}

func classifyRefreshResponse(status int, retryAfter *time.Duration, body []byte, complete bool) RefreshOutcome {
	if status >= 200 && status < 300 {
		if complete {
			response, err := ParseRefreshResponse(body)
			if err == nil && response.HasAccessToken() {
				return RefreshOutcome{Kind: RefreshApplied, Response: response}
			}
		}
		return RefreshOutcome{Kind: RefreshAmbiguous, Ambiguous: AmbiguousServerBody}
	}
	code := ""
	if complete {
		code = refreshErrorCode(body)
	}
	for _, class := range []PermanentClass{PermanentExpired, PermanentReused, PermanentInvalidated} {
		if strings.EqualFold(code, string(class)) {
			return RefreshOutcome{Kind: RefreshPermanent, Permanent: class}
		}
	}
	switch {
	case status == 401:
		return RefreshOutcome{Kind: RefreshPermanent, Permanent: PermanentUnauthorized}
	case status == 400 && strings.EqualFold(code, "invalid_grant"):
		return RefreshOutcome{Kind: RefreshPermanent, Permanent: PermanentInvalidGrant}
	case status == 429:
		return RefreshOutcome{Kind: RefreshRateLimited, RetryAfter: retryAfter}
	case status >= 500 && status <= 599:
		return RefreshOutcome{Kind: RefreshServerError, Status: status}
	case status >= 400 && status <= 499:
		return RefreshOutcome{Kind: RefreshRejected, Status: status}
	default:
		return RefreshOutcome{Kind: RefreshAmbiguous, Ambiguous: AmbiguousServerBody}
	}
}

func refreshErrorCode(body []byte) string {
	var doc struct {
		Error jsontext.Value `json:"error"`
		Code  jsontext.Value `json:"code"`
	}
	defer func() { memguard.WipeBytes(doc.Error); memguard.WipeBytes(doc.Code) }()
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	if doc.Error.Kind() == '"' {
		return knownRefreshErrorCode(doc.Error)
	}
	var nested struct {
		Code jsontext.Value `json:"code"`
	}
	defer func() { memguard.WipeBytes(nested.Code) }()
	if json.Unmarshal(doc.Error, &nested) == nil && nested.Code.Kind() == '"' {
		return knownRefreshErrorCode(nested.Code)
	}
	return knownRefreshErrorCode(doc.Code)
}

func knownRefreshErrorCode(value jsontext.Value) string {
	if value.Kind() != '"' {
		return ""
	}
	code, err := jsontext.AppendUnquote(nil, bytes.TrimSpace(value))
	defer memguard.WipeBytes(code)
	if err != nil {
		return ""
	}
	for _, fixed := range []string{"invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated"} {
		if len(code) == len(fixed) && bytes.EqualFold(code, []byte(fixed)) {
			return fixed
		}
	}
	return ""
}
