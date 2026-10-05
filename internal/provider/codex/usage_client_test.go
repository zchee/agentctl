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
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/secret"
)

type usageTestAuth struct {
	token  *secret.Secret
	scheme string
	extra  []provider.Header
	active atomic.Bool
}

func (a *usageTestAuth) AuthorizationHeader() string     { return a.scheme }
func (a *usageTestAuth) ExtraHeaders() []provider.Header { return a.extra }
func (a *usageTestAuth) WithAuthorizationHeader(fn func(string) error) error {
	if a.token == nil {
		return fn(a.scheme)
	}
	return a.token.WithPlaintext(func(token []byte) error {
		a.active.Store(true)
		defer a.active.Store(false)
		return fn("Bearer " + string(token))
	})
}

func testUsageAuth(t *testing.T) *usageTestAuth {
	t.Helper()
	token, err := secret.NewSecret([]byte("usage-test-token"))
	if err != nil {
		t.Fatal(err)
	}
	return &usageTestAuth{token: token, extra: []provider.Header{{Name: "ChatGPT-Account-Id", Value: "account-test"}}}
}

func TestUsageRequestHeadersAndCookies(t *testing.T) {
	tests := map[string]struct {
		fedramp       bool
		accountHeader string
	}{
		"success: ordinary":                  {accountHeader: "ChatGPT-Account-Id"},
		"success: fedramp":                   {fedramp: true, accountHeader: "ChatGPT-Account-Id"},
		"success: mixed case account header": {accountHeader: "cHaTgPt-aCcOuNt-iD"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			auth := testUsageAuth(t)
			auth.extra[0].Name = tt.accountHeader
			if tt.fedramp {
				auth.extra = append(auth.extra, provider.Header{Name: "X-OpenAI-Fedramp", Value: "true"})
			}
			body := usageFixture(t, "usage-2026-09-16.json")
			requests := atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != UsagePath {
					t.Errorf("request: %s %s", r.Method, r.URL.Path)
				}
				if !auth.active.Load() {
					t.Error("request escaped secret window")
				}
				if r.Header.Get("Authorization") != "Bearer usage-test-token" || r.Header.Get("ChatGPT-Account-Id") != "account-test" || r.Header.Get("Cookie") != "" {
					t.Error("authorization/account/cookie mismatch")
				}
				var names []string
				for key := range r.Header {
					if key != "Accept-Encoding" {
						names = append(names, strings.ToLower(key))
					}
				}
				slices.Sort(names)
				want := []string{"accept", "authorization", "chatgpt-account-id", "user-agent"}
				if tt.fedramp {
					want = append(want, "x-openai-fedramp")
				}
				if diff := gocmp.Diff(want, names); diff != "" {
					t.Errorf("header names: %s", diff)
				}
				if r.Header.Get("User-Agent") != "agentctl/test" {
					t.Error("user agent missing")
				}
				w.Header().Set("Set-Cookie", "sample=private; Path=/")
				if _, err := w.Write(body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client := NewUsageClient(server.URL+"/", "agentctl/test", time.Second)
			if client.UsageURL() != server.URL+UsagePath {
				t.Fatal(client.UsageURL())
			}
			for range 2 {
				got, err := client.Fetch(t.Context(), provider.AccountRef{ID: "row", Auth: auth})
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Windows) != 3 {
					t.Fatal(got)
				}
			}
			if requests.Load() != 2 {
				t.Fatal(requests.Load())
			}
		})
	}
}

func TestUsageHTTPFailures(t *testing.T) {
	tests := map[string]struct {
		status int
		retry  string
		want   provider.FetchKind
	}{
		"error: unauthorized":              {401, "", provider.FetchUnauthorized},
		"error: policy refusal":            {403, "", provider.FetchHTTP},
		"error: rate limited":              {429, "30", provider.FetchRateLimited},
		"error: rate limited without hint": {429, "", provider.FetchRateLimited},
		"error: unavailable":               {503, "", provider.FetchHTTP},
		"error: redirect":                  {302, "", provider.FetchHTTP},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			requests := atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != UsagePath || r.Method != http.MethodGet {
					t.Error("unexpected request or refresh")
				}
				w.Header().Set("Retry-After", tt.retry)
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			_, err := NewUsageClient(server.URL, "agentctl/test", time.Second).Fetch(t.Context(), provider.AccountRef{ID: "row", Auth: testUsageAuth(t)})
			got, ok := errors.AsType[*provider.FetchError](err)
			if !ok || got.Kind != tt.want {
				t.Fatalf("error=%v want kind %v", err, tt.want)
			}
			if tt.want == provider.FetchRateLimited {
				if got.HasRetryAfter != (tt.retry != "") || (got.HasRetryAfter && got.RetryAfter != 30*time.Second) {
					t.Fatalf("retry-after: present=%t delay=%s", got.HasRetryAfter, got.RetryAfter)
				}
			}
			if requests.Load() != 1 {
				t.Fatal("retried or followed redirect", requests.Load())
			}
		})
	}
}

func TestUsageBodyLimitsAndParsing(t *testing.T) {
	normal := usageFixture(t, "usage-2026-09-16.json")
	tests := map[string]struct {
		body    []byte
		gzip    bool
		wantErr bool
	}{
		"success: plain":         {normal, false, false},
		"success: gzip":          {normal, true, false},
		"error: HTML":            {[]byte("<html>private-marker</html>"), false, true},
		"error: malformed JSON":  {[]byte(`{"private":"private-marker"`), false, true},
		"error: encoded ceiling": {[]byte(`{"padding":"` + strings.Repeat("x", MaxUsageBodyBytes) + `"}`), false, true},
		"error: decoded ceiling": {[]byte(`{"padding":"` + strings.Repeat("x", MaxUsageBodyBytes) + `"}`), true, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := tt.body
			if tt.gzip {
				var buffer bytes.Buffer
				encoder := gzip.NewWriter(&buffer)
				if _, err := encoder.Write(body); err != nil {
					t.Fatal(err)
				}
				if err := encoder.Close(); err != nil {
					t.Fatal(err)
				}
				body = buffer.Bytes()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.gzip {
					w.Header().Set("Content-Encoding", "gzip")
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			got, err := NewUsageClient(server.URL, "agentctl/test", time.Second).Fetch(t.Context(), provider.AccountRef{ID: "row", Auth: testUsageAuth(t)})
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v wantErr=%t", err, tt.wantErr)
			}
			if err != nil {
				failure, ok := errors.AsType[*provider.FetchError](err)
				if !ok || failure.Kind != provider.FetchParse {
					t.Fatal(err)
				}
				if strings.Contains(err.Error(), "private-marker") {
					t.Fatal("payload in error")
				}
				return
			}
			if len(got.Windows) != 3 {
				t.Fatal(got)
			}
		})
	}
}

func TestUsageFetchLogOmitsPrivateValues(t *testing.T) {
	body := usageFixture(t, "usage-sentinel-email.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	auth := testUsageAuth(t)
	_, err := NewUsageClient(server.URL, "agentctl/test", time.Second).Fetch(t.Context(), provider.AccountRef{ID: "row", Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "fetching codex usage") {
		t.Fatal("fetch trace missing")
	}
	for _, private := range []string{"agctl-test-codex-email-0001", "usage-test-token", "Bearer", "account-test"} {
		if strings.Contains(log.String(), private) {
			t.Fatalf("fetch trace contains private value %q", private)
		}
	}
}

func TestUsageHeaderRefusalsAndCancellation(t *testing.T) {
	tests := map[string]struct {
		authorization string
		headers       []provider.Header
		cancelled     bool
	}{
		"error: empty authorization": {},
		"error: no separator":        {authorization: "Bearer"},
		"error: empty bearer":        {authorization: "Bearer "},
		"error: extra space":         {authorization: "Bearer  secret"},
		"error: bearer newline":      {authorization: "Bearer secret\r\nprivate-marker"},
		"error: bearer extra token":  {authorization: "Bearer secret extra"},
		"error: missing account":     {authorization: "Bearer secret"},
		"error: fedramp alone":       {authorization: "Bearer secret", headers: []provider.Header{{Name: "X-OpenAI-Fedramp", Value: "true"}}},
		"error: account newline":     {authorization: "Bearer secret", headers: []provider.Header{{Name: "ChatGPT-Account-Id", Value: "id\r\nprivate-marker"}}},
		"error: account space":       {authorization: "Bearer secret", headers: []provider.Header{{Name: "ChatGPT-Account-Id", Value: "id id"}}},
		"error: account non ASCII":   {authorization: "Bearer secret", headers: []provider.Header{{Name: "ChatGPT-Account-Id", Value: "é"}}},
		"error: account empty":       {authorization: "Bearer secret", headers: []provider.Header{{Name: "ChatGPT-Account-Id", Value: ""}}},
		"error: cancelled":           {cancelled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			count := atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { count.Add(1); w.WriteHeader(200) }))
			defer server.Close()
			ctx := t.Context()
			if tt.cancelled {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			auth := &usageTestAuth{scheme: tt.authorization, extra: tt.headers}
			_, err := NewUsageClient(server.URL, "agentctl/test", time.Second).Fetch(ctx, provider.AccountRef{ID: "row", Auth: auth})
			if err == nil || count.Load() != 0 {
				t.Fatalf("error=%v requests=%d", err, count.Load())
			}
			if strings.Contains(fmt.Sprint(err), "private-marker") {
				t.Fatal("header in error")
			}
		})
	}
}
