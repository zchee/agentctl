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
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
)

// storedBlob is a credential blob as it would come off disk: a refresh
// token, two scopes, and an access token the profile tests also send.
const storedBlob = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-stored-access","refreshToken":"sk-ant-ort01-stored-refresh","expiresAt":1757300000000,"scopes":["user:inference","user:profile"]}}`

// storedRefreshBody is the exact POST body [Credentials.RefreshBody]
// builds from storedBlob, which is what the token endpoint must receive.
const storedRefreshBody = `{"grant_type":"refresh_token","refresh_token":"sk-ant-ort01-stored-refresh","client_id":"` + ClientID + `","scope":"user:inference user:profile"}`

// testUserAgent is the User-Agent every request in these tests must carry.
const testUserAgent = "agentctl-test"

// storedCredentials parses storedBlob.
func storedCredentials(t *testing.T) *Credentials {
	t.Helper()
	credentials, err := ParseBlob([]byte(storedBlob))
	if err != nil {
		t.Fatalf("ParseBlob(storedBlob) = %v", err)
	}
	return credentials
}

// sha256Hex is the lowercase hex SHA-256 of a plaintext, the form
// [Credentials.Digests] reports.
func sha256Hex(plaintext string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(plaintext)))
}

// oauthClientFor builds a client whose token and profile endpoints are both
// the given server.
func oauthClientFor(t *testing.T, serverURL string) *OAuthClient {
	t.Helper()
	client, err := NewOAuthClient(serverURL, serverURL+"/api/oauth/profile", testUserAgent)
	if err != nil {
		t.Fatalf("NewOAuthClient(%q) = %v", serverURL, err)
	}
	return client
}

func TestRefreshAccessSendsTheDocumentedRequestAndMergesTheResponse(t *testing.T) {
	fixture, err := os.ReadFile("testdata/oauth/exchange-response.json")
	if err != nil {
		t.Fatalf("reading the exchange fixture: %v", err)
	}

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		for header, want := range map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"User-Agent":   testUserAgent,
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("header %s = %q, want %q", header, got, want)
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		if diff := gocmp.Diff(storedRefreshBody, string(body)); diff != "" {
			t.Errorf("request body mismatch (-want +got):\n%s", diff)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	credentials := storedCredentials(t)
	before := time.Now().UnixMilli()
	merged, err := oauthClientFor(t, server.URL).RefreshAccess(t.Context(), credentials)
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatalf("RefreshAccess() = %v", err)
	}
	if merged != credentials {
		t.Error("RefreshAccess must return the same credentials it merged into")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the endpoint was contacted %d times, want 1", got)
	}

	digests, err := credentials.Digests()
	if err != nil {
		t.Fatalf("Digests() = %v", err)
	}
	if want := sha256Hex("<REDACTED-ACCESS-TOKEN>"); digests.AccessSHA256 != want {
		t.Errorf("access digest = %s, want the fixture's token digest %s", digests.AccessSHA256, want)
	}
	if want := sha256Hex("<REDACTED-REFRESH-TOKEN>"); digests.RefreshSHA256 != want {
		t.Errorf("refresh digest = %s, want the rotated token digest %s", digests.RefreshSHA256, want)
	}
	if credentials.ExpiresAtMillis < before+28_800_000 || credentials.ExpiresAtMillis > after+28_800_000 {
		t.Errorf("ExpiresAtMillis = %d, want now + 28800s", credentials.ExpiresAtMillis)
	}
	if credentials.RefreshTokenExpiresAtMillis == nil {
		t.Fatal("RefreshTokenExpiresAtMillis = nil, want the fixture's lifetime merged")
	}
	if got := *credentials.RefreshTokenExpiresAtMillis; got < before+2_377_445_000 || got > after+2_377_445_000 {
		t.Errorf("RefreshTokenExpiresAtMillis = %d, want now + 2377445s", got)
	}
	wantScopes := []string{"user:file_upload", "user:inference", "user:mcp_servers", "user:profile", "user:sessions:claude_code"}
	if diff := gocmp.Diff(wantScopes, credentials.Scopes); diff != "" {
		t.Errorf("scopes mismatch (-want +got):\n%s", diff)
	}

	// The merged credentials render redacted under every verb.
	for _, rendered := range []string{
		fmt.Sprintf("%v", credentials),
		fmt.Sprintf("%+v", credentials),
		fmt.Sprintf("%#v", credentials),
		fmt.Sprintf("%s", credentials),
	} {
		if strings.Contains(rendered, "REDACTED-ACCESS-TOKEN") || strings.Contains(rendered, "REDACTED-REFRESH-TOKEN") {
			t.Errorf("a render leaked token plaintext: %s", rendered)
		}
	}
}

func TestRefreshAccessKeepsTheStoredRefreshTokenWhenTheServerSendsNone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":28800,"token_type":"Bearer"}`))
	}))
	defer server.Close()

	credentials := storedCredentials(t)
	if _, err := oauthClientFor(t, server.URL).RefreshAccess(t.Context(), credentials); err != nil {
		t.Fatalf("RefreshAccess() = %v", err)
	}

	digests, err := credentials.Digests()
	if err != nil {
		t.Fatalf("Digests() = %v", err)
	}
	if want := sha256Hex("new-access"); digests.AccessSHA256 != want {
		t.Errorf("access digest = %s, want the new token digest %s", digests.AccessSHA256, want)
	}
	if want := sha256Hex("sk-ant-ort01-stored-refresh"); digests.RefreshSHA256 != want {
		t.Errorf("refresh digest = %s, want the stored token kept (%s)", digests.RefreshSHA256, want)
	}
	wantScopes := []string{"user:inference", "user:profile"}
	if diff := gocmp.Diff(wantScopes, credentials.Scopes); diff != "" {
		t.Errorf("a response without scope must keep the stored scopes (-want +got):\n%s", diff)
	}
}

func TestRefreshAccessClassifiesEveryFailure(t *testing.T) {
	tests := map[string]struct {
		status     int
		body       string
		retryAfter string
		precancel  bool
		noRefresh  bool
		closed     bool
		wantCalls  int64
		check      func(t *testing.T, err error)
	}{
		"error: invalid_grant as a bare string is a dead chain": {
			status:    http.StatusBadRequest,
			body:      `{"error":"invalid_grant","error_description":"expired"}`,
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				authErr, ok := errors.AsType[*errs.AuthError](err)
				if !ok || !authErr.InvalidGrant {
					t.Errorf("err = %v, want AuthError{InvalidGrant: true}", err)
				}
			},
		},
		"error: invalid_grant in the nested spelling is a dead chain": {
			status:    http.StatusBadRequest,
			body:      `{"error":{"type":"invalid_grant","message":"expired"}}`,
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				authErr, ok := errors.AsType[*errs.AuthError](err)
				if !ok || !authErr.InvalidGrant {
					t.Errorf("err = %v, want AuthError{InvalidGrant: true}", err)
				}
			},
		},
		"error: a rate limit surfaces once and is not retried": {
			status:    http.StatusTooManyRequests,
			body:      `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`,
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				httpErr, ok := errors.AsType[*errs.HTTPError](err)
				if !ok || httpErr.Status != http.StatusTooManyRequests || httpErr.HasRetryAfter {
					t.Errorf("err = %v, want HTTPError{Status: 429} without a retry hint", err)
				}
			},
		},
		"error: a rate limit carries the server's retry hint": {
			status:     http.StatusTooManyRequests,
			body:       `{"error":{"type":"rate_limit_error"}}`,
			retryAfter: "7",
			wantCalls:  1,
			check: func(t *testing.T, err error) {
				httpErr, ok := errors.AsType[*errs.HTTPError](err)
				if !ok || httpErr.Status != http.StatusTooManyRequests || !httpErr.HasRetryAfter || httpErr.RetryAfter != 7*time.Second {
					t.Errorf("err = %v, want HTTPError{Status: 429, RetryAfter: 7s}", err)
				}
			},
		},
		"error: a server failure carries its redacted body": {
			status:    http.StatusServiceUnavailable,
			body:      "upstream unavailable",
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				httpErr, ok := errors.AsType[*errs.HTTPError](err)
				if !ok || httpErr.Status != http.StatusServiceUnavailable {
					t.Errorf("err = %v, want HTTPError{Status: 503}", err)
				}
				if !strings.Contains(err.Error(), "upstream unavailable") {
					t.Errorf("err = %q, want the body kept in the message", err)
				}
			},
		},
		"error: token material is replaced before the body reaches the message": {
			status:    http.StatusServiceUnavailable,
			body:      "bad token sk-ant-oat01-AbC123_xyz for account 9",
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				if !strings.Contains(err.Error(), "bad token <redacted> for account 9") {
					t.Errorf("err = %q, want the token run replaced", err)
				}
			},
		},
		"error: an unparseable success body is a parse failure": {
			status:    http.StatusOK,
			body:      "not json",
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchParse {
					t.Errorf("err = %v, want a parse FetchError", err)
				}
			},
		},
		"error: a success body without the access token is a parse failure": {
			status:    http.StatusOK,
			body:      `{"expires_in":28800}`,
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchParse {
					t.Errorf("err = %v, want a parse FetchError", err)
				}
				if !strings.Contains(err.Error(), "access_token") {
					t.Errorf("err = %q, want the missing member named", err)
				}
			},
		},
		"error: a cancelled run never contacts the endpoint": {
			precancel: true,
			wantCalls: 0,
			check: func(t *testing.T, err error) {
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchCancelled {
					t.Errorf("err = %v, want a cancelled FetchError", err)
				}
			},
		},
		"error: an unreachable endpoint is a transport failure": {
			closed:    true,
			wantCalls: 0,
			check: func(t *testing.T, err error) {
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchTransport {
					t.Errorf("err = %v, want a transport FetchError", err)
				}
			},
		},
		"error: an account with no refresh token refuses before any request": {
			noRefresh: true,
			wantCalls: 0,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoRefreshToken) {
					t.Errorf("err = %v, want ErrNoRefreshToken", err)
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			if tt.closed {
				server.Close()
			}

			blob := storedBlob
			if tt.noRefresh {
				blob = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-stored-access","expiresAt":1757300000000,"scopes":["user:inference"]}}`
			}
			credentials, err := ParseBlob([]byte(blob))
			if err != nil {
				t.Fatalf("ParseBlob() = %v", err)
			}

			ctx := t.Context()
			if tt.precancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			_, err = oauthClientFor(t, server.URL).RefreshAccess(ctx, credentials)
			if err == nil {
				t.Fatal("RefreshAccess() = nil, want an error")
			}
			tt.check(t, err)
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("the endpoint was contacted %d times, want %d", got, tt.wantCalls)
			}
			// No error may carry the stored tokens or any sk-ant run.
			if message := err.Error(); strings.Contains(message, "sk-ant") {
				t.Errorf("the error leaked token material: %q", message)
			}
		})
	}
}

func TestParseTokenResponseReadsTheCapturedExchangeResponse(t *testing.T) {
	fixture, err := os.ReadFile("testdata/oauth/exchange-response.json")
	if err != nil {
		t.Fatalf("reading the exchange fixture: %v", err)
	}

	response, err := parseTokenResponse(fixture)
	if err != nil {
		t.Fatalf("parseTokenResponse() = %v", err)
	}
	if response.ExpiresIn != 28_800 {
		t.Errorf("ExpiresIn = %d, want 28800", response.ExpiresIn)
	}
	if response.RefreshTokenExpiresIn == nil || *response.RefreshTokenExpiresIn != 2_377_445 {
		t.Errorf("RefreshTokenExpiresIn = %v, want 2377445", response.RefreshTokenExpiresIn)
	}
	if response.Account == nil || response.Account.UUID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("Account = %+v, want the fixture's account UUID", response.Account)
	}
	if response.Organization == nil || response.Organization.Name == nil || *response.Organization.Name != "Example Org" {
		t.Errorf("Organization = %+v, want the fixture's organization", response.Organization)
	}
	if response.TokenType == nil || *response.TokenType != "Bearer" {
		t.Errorf("TokenType = %v, want Bearer", response.TokenType)
	}
	// The unknown token_uuid member is ignored rather than rejected, and
	// the tokens landed in sealed secrets.
	access, err := response.AccessToken.Digest()
	if err != nil {
		t.Fatalf("AccessToken.Digest() = %v", err)
	}
	if want := sha256Hex("<REDACTED-ACCESS-TOKEN>"); access != want {
		t.Errorf("access digest = %s, want %s", access, want)
	}
}

func TestRedactBodyReplacesTokenRunsBeforeTruncating(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"success: a token run is replaced in place": {
			body: "bad token sk-ant-oat01-AbC123_xyz for account 9",
			want: "bad token <redacted> for account 9",
		},
		"success: a long token collapses to the marker alone": {
			body: "sk-ant-oat01-" + strings.Repeat("A", 4096),
			want: "<redacted>",
		},
		"success: two runs are both replaced": {
			body: "sk-ant-a and sk-ant-b",
			want: "<redacted> and <redacted>",
		},
		"success: an empty body stays empty": {
			body: "",
			want: "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := redactBody(tt.body); got != tt.want {
				t.Errorf("redactBody(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}

	t.Run("success: a bulk body is truncated after redaction with an ellipsis", func(t *testing.T) {
		redacted := redactBody(strings.Repeat("x", 4096))
		if len(redacted) > MaxErrorBodyBytes+len("…") {
			t.Errorf("len = %d, want at most %d", len(redacted), MaxErrorBodyBytes+len("…"))
		}
		if !strings.HasSuffix(redacted, "…") {
			t.Errorf("redacted = %q, want an ellipsis suffix", redacted[max(0, len(redacted)-8):])
		}
	})

	t.Run("success: the cut never splits a rune", func(t *testing.T) {
		redacted := redactBody(strings.Repeat("é", 4096))
		if !strings.HasSuffix(redacted, "…") {
			t.Fatal("want an ellipsis suffix")
		}
		trimmed := strings.TrimSuffix(redacted, "…")
		for _, r := range trimmed {
			if r == '�' {
				t.Fatal("the cut split a rune")
			}
		}
	})
}

func TestNewOAuthClientRefusesAnUnusableEndpoint(t *testing.T) {
	if _, err := NewOAuthClient("::not-a-url", "http://127.0.0.1:9", testUserAgent); err == nil {
		t.Error("NewOAuthClient must refuse an endpoint that is not a URL")
	} else if _, ok := errors.AsType[*errs.ConfigError](err); !ok {
		t.Errorf("err = %v, want a ConfigError", err)
	}
}
