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
	json "encoding/json/v2"
	"errors"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
)

func TestLoginExchange(t *testing.T) {
	fixture, err := os.ReadFile("testdata/oauth/exchange-response.json")
	if err != nil {
		t.Fatal(err)
	}
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		status      int
		body, retry string
		wantCalls   int
		wantError   bool
	}{
		"success: exchange fixture":      {status: 200, body: string(fixture), wantCalls: 1},
		"success: retry rate limit once": {status: 429, body: string(fixture), retry: "0", wantCalls: 2},
		"success: date hint uses floor":  {status: 429, body: string(fixture), retry: "Wed, 21 Oct 2099 07:28:00 GMT", wantCalls: 2},
		"error: invalid grant":           {status: 400, body: `{"error":"invalid_grant"}`, wantCalls: 1, wantError: true},
		"error: unavailable":             {status: 503, body: `{"error":"sk-ant-planted"}`, wantCalls: 1, wantError: true},
		"error: malformed success":       {status: 200, body: `{"access_token":"sk-ant-planted","expires_in":sk-ant-marker}`, wantCalls: 1, wantError: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "test-login" {
					t.Error("incorrect exchange request headers")
				}
				blob, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var body map[string]string
				if err := json.Unmarshal(blob, &body); err != nil {
					t.Error(err)
					return
				}
				if diff := gocmp.Diff(map[string]string{"grant_type": "authorization_code", "code": "authorization-code", "redirect_uri": Redirect{Port: 4321}.URI(), "client_id": ClientID, "state": pkce.State}, map[string]string{"grant_type": body["grant_type"], "code": body["code"], "redirect_uri": body["redirect_uri"], "client_id": body["client_id"], "state": body["state"]}); diff != "" {
					t.Error(diff)
				}
				if CodeChallenge(body["code_verifier"]) != pkce.Challenge {
					t.Error("incorrect code verifier")
				}
				if test.status == 429 && call == 1 {
					w.Header().Set("Retry-After", test.retry)
					w.WriteHeader(429)
					return
				}
				status := test.status
				if status == 429 {
					status = 200
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := NewLoginClient(server.URL, server.URL, server.URL, "test-login")
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.exchange(t.Context(), "authorization-code", pkce.State, pkce, Redirect{Port: 4321}, time.Millisecond)
			if (err != nil) != test.wantError {
				t.Fatalf("exchange error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "sk-ant") {
				t.Fatal("exchange error leaked token")
			}
			if int(calls.Load()) != test.wantCalls {
				t.Fatalf("requests = %d; want %d", calls.Load(), test.wantCalls)
			}
			if test.wantError {
				return
			}
			credentials, err := LoginCredentials(response, 1000, []string{"fallback"})
			if err != nil {
				t.Fatal(err)
			}
			want := &Identity{AccountUUID: "11111111-1111-4111-8111-111111111111", OrganizationUUID: new("22222222-2222-4222-8222-222222222222"), Email: new("user@example.com"), OrgName: new("Example Org")}
			if diff := gocmp.Diff(want, credentials.Identity()); diff != "" {
				t.Fatal(diff)
			}
			if credentials.ExpiresAtMillis != 28_801_000 || *credentials.RefreshTokenExpiresAtMillis != 2_377_446_000 {
				t.Fatal("expiry did not use milliseconds")
			}
			if diff := gocmp.Diff(DefaultScopes, credentials.Scopes); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestLoginExchangeWipesEachBodyBeforeRetryWait(t *testing.T) {
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
			return
		}
		if CodeChallenge(body["code_verifier"]) != pkce.Challenge {
			t.Error("attempt did not contain a fresh verifier body")
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"token","expires_in":20}`)
	}))
	defer server.Close()
	client, err := NewLoginClient(server.URL, server.URL, server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	var bodies []bytes.Reader
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client.tokenClient.Transport = exchangeBodyTransport(func(request *http.Request) (*http.Response, error) {
		body, ok := request.Body.(*oauthBodyReader)
		if !ok || request.GetBody != nil {
			return nil, errors.New("exchange body is replayable or unsynchronized")
		}
		bodies = append(bodies, *body.reader)
		return transport.RoundTrip(request)
	})
	assertWiped := func(index int) {
		t.Helper()
		body := bodies[index]
		blob, err := io.ReadAll(&body)
		if err != nil {
			t.Fatal(err)
		}
		if len(blob) == 0 || !bytes.Equal(blob, make([]byte, len(blob))) {
			t.Error("exchange body was not wiped")
		}
	}
	checkedBeforeWait := false
	previous := slog.Default()
	previousWriter, previousFlags := log.Writer(), log.Flags()
	defer func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()
	slog.SetDefault(slog.New(slog.NewTextHandler(exchangeLogWriter(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("retrying once")) {
			if len(bodies) != 1 {
				t.Fatalf("requests before wait = %d; want 1", len(bodies))
			}
			assertWiped(0)
			checkedBeforeWait = true
		}
		return len(p), nil
	}), nil)))
	if _, err := client.exchange(t.Context(), "code", pkce.State, pkce, Redirect{}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !checkedBeforeWait || calls.Load() != 2 || len(bodies) != 2 {
		t.Fatalf("retry observation: checked=%v calls=%d bodies=%d", checkedBeforeWait, calls.Load(), len(bodies))
	}
	assertWiped(0)
	assertWiped(1)
}

type exchangeBodyTransport func(*http.Request) (*http.Response, error)

func (f exchangeBodyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type exchangeLogWriter func([]byte) (int, error)

func (f exchangeLogWriter) Write(p []byte) (int, error) { return f(p) }

func TestLoginExchangeStopsAfterSecondRateLimit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(429) }))
	defer server.Close()
	client, err := NewLoginClient(server.URL, server.URL, server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.exchange(t.Context(), "code", pkce.State, pkce, Redirect{}, 0)
	if httpErr, ok := errors.AsType[*errs.HTTPError](err); !ok || httpErr.Status != 429 || calls.Load() != 2 {
		t.Fatalf("second rate limit: calls=%d err=%v", calls.Load(), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.Exchange(ctx, "code", pkce.State, pkce, Redirect{})
	if err == nil || calls.Load() != 2 {
		t.Fatal("cancelled exchange sent a request")
	}
}

func TestLoginCredentials(t *testing.T) {
	tests := map[string]struct {
		body        string
		wantAccount *TokenAccount
		wantError   bool
	}{
		"success: absent identity and scope fallback": {body: `{"access_token":"token","expires_in":20}`},
		"success: workspace UUID":                     {body: `{"access_token":"token","expires_in":20,"workspace":{"uuid":"workspace","name":"Example"}}`, wantAccount: &TokenAccount{WorkspaceID: new("workspace"), WorkspaceName: new("Example")}},
		"success: workspace id takes precedence":      {body: `{"access_token":"token","expires_in":20,"workspace":{"id":null,"uuid":"ignored"}}`, wantAccount: &TokenAccount{}},
		"error: expiry overflow":                      {body: `{"access_token":"token","expires_in":9223372036854775807}`, wantError: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			response, err := parseTokenResponse([]byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			credentials, err := LoginCredentials(response, 1, []string{"fallback"})
			if (err != nil) != test.wantError {
				t.Fatalf("credentials error = %v", err)
			}
			if test.wantError {
				return
			}
			if diff := gocmp.Diff(test.wantAccount, credentials.TokenAccount); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff([]string{"fallback"}, credentials.Scopes); diff != "" {
				t.Fatal(diff)
			}
			if *credentials.ClientID != ClientID {
				t.Fatal("client id missing")
			}
		})
	}
	_, err := LoginCredentials(&TokenResponse{ExpiresIn: 1}, math.MaxInt64, nil)
	if err == nil {
		t.Fatal("overflow accepted")
	}
}
