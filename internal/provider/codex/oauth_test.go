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
	json "encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awnumar/memguard"
	gocmp "github.com/google/go-cmp/cmp"
)

func TestRefreshResponsePolicyUsesRealLoopbackAnswers(t *testing.T) {
	tests := map[string]struct {
		status    int
		body      string
		kind      RefreshOutcomeKind
		permanent PermanentClass
	}{
		"success: usable grant":              {200, `{"access_token":"test-access","refresh_token":"test-rotated","earliest_refresh_at":1790417597}`, RefreshApplied, ""},
		"error: unauthorized":                {401, `{}`, RefreshPermanent, PermanentUnauthorized},
		"error: invalid grant":               {400, `{"error":"INVALID_GRANT"}`, RefreshPermanent, PermanentInvalidGrant},
		"error: nested reused":               {400, `{"error":{"code":"refresh_token_reused"}}`, RefreshPermanent, PermanentReused},
		"error: expired":                     {401, `{"error":"refresh_token_expired"}`, RefreshPermanent, PermanentExpired},
		"error: top level invalidated":       {403, `{"code":"refresh_token_invalidated"}`, RefreshPermanent, PermanentInvalidated},
		"error: permanent before rate limit": {429, `{"error":{"code":"refresh_token_reused"}}`, RefreshPermanent, PermanentReused},
		"error: rejected request":            {400, `{"error":"invalid_request"}`, RefreshRejected, ""},
		"error: forbidden":                   {403, `forbidden`, RefreshRejected, ""},
		"error: rate limited":                {429, `{}`, RefreshRateLimited, ""},
		"error: server unavailable":          {503, `busy`, RefreshServerError, ""},
		"error: malformed success":           {200, `<html>not json</html>`, RefreshAmbiguous, ""},
		"error: empty success":               {200, ``, RefreshAmbiguous, ""},
		"error: missing access":              {200, `{"refresh_token":"test-rotated","id_token":null}`, RefreshAmbiguous, ""},
		"error: typed access":                {200, `{"access_token":42}`, RefreshAmbiguous, ""},
		"error: array success":               {201, `["access_token"]`, RefreshAmbiguous, ""},
		"error: no content":                  {204, ``, RefreshAmbiguous, ""},
		"error: oversized success":           {200, `{"access_token":"` + strings.Repeat("a", 512<<10) + `"}`, RefreshAmbiguous, ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "agentctl/test" {
					t.Error("request contract drift")
				}
				if r.Header.Get("Originator") != "" || r.Header.Get("Cookie") != "" {
					t.Error("vendor identity or cookie sent")
				}
				w.Header().Set("Retry-After", "120")
				w.Header().Set("Set-Cookie", "test-cookie=private")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := NewRefreshClient(server.URL, "agentctl/test")
			wire := client.transport.post(t.Context(), client.tokenURL, client.userAgent, []byte(`{"grant_type":"refresh_token"}`))
			defer memguard.WipeBytes(wire.Body)
			wait := 120 * time.Second
			out := classifyRefreshResponse(wire.Status, &wait, wire.Body, wire.BodyComplete)
			if diff := gocmp.Diff(test.kind, out.Kind); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(test.permanent, out.Permanent); diff != "" {
				t.Fatal(diff)
			}
			if calls.Load() != 1 {
				t.Fatalf("POST count=%d; never resend uncertain outcomes", calls.Load())
			}
			if out.Kind == RefreshRateLimited && (out.RetryAfter == nil || *out.RetryAfter != wait) {
				t.Fatal("retry-after lost")
			}
		})
	}
}

func TestRefreshRedirectAndGzipDoNotBecomeApplied(t *testing.T) {
	tests := map[string]struct{ gzip bool }{"error: redirect": {}, "error: gzip expansion": {true}}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var calls, targetCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)
				_, _ = io.WriteString(w, `{"access_token":"test-access"}`)
			}))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !test.gzip {
					w.Header().Set("Location", target.URL)
					w.WriteHeader(307)
					return
				}
				w.Header().Set("Content-Encoding", "gzip")
				z := gzip.NewWriter(w)
				_, _ = io.WriteString(z, `{"access_token":"test-access","pad":"`+strings.Repeat(" ", 4<<20)+`"}`)
				_ = z.Close()
			}))
			defer server.Close()
			wire := newRefreshClient().post(t.Context(), server.URL, "agentctl/test", []byte(`{}`))
			defer memguard.WipeBytes(wire.Body)
			out := classifyRefreshResponse(wire.Status, nil, wire.Body, wire.BodyComplete)
			if out.Kind != RefreshAmbiguous || out.Ambiguous != AmbiguousServerBody || calls.Load() != 1 || targetCalls.Load() != 0 {
				t.Fatalf("unsafe policy result: %v calls=%d target=%d", out, calls.Load(), targetCalls.Load())
			}
		})
	}
}

func TestRefreshTransportHasNoCookieJarOrWholeRequestDeadline(t *testing.T) {
	client := newRefreshClient()
	if client.client.Jar != nil || client.client.Timeout != 0 {
		t.Fatal("cookie jar or global deadline enabled")
	}
	if diff := gocmp.Diff(DefaultPhaseTimeouts(), client.phases); diff != "" {
		t.Fatal(diff)
	}
	total := TimeoutResolve + TimeoutConnect + TimeoutSendRequest + TimeoutSendBody + TimeoutRecvResponse + TimeoutRecvBody
	if total != 19*time.Second {
		t.Fatalf("phase total=%s", total)
	}
}

func TestRefreshErrorCodePrecedence(t *testing.T) {
	tests := map[string]struct{ body, want string }{
		"success: string error first":            {`{"error":"invalid_grant","code":"refresh_token_reused"}`, "invalid_grant"},
		"success: nested error first":            {`{"error":{"code":"refresh_token_expired"},"code":"refresh_token_reused"}`, "refresh_token_expired"},
		"success: top level fallback":            {`{"error":42,"code":"refresh_token_reused"}`, "refresh_token_reused"},
		"success: escaped and case folded code":  {`{"error":"INVALID_gRANT"}`, "invalid_grant"},
		"error: unknown error prevents fallback": {`{"error":"planted-private","code":"refresh_token_reused"}`, ""},
		"error: malformed":                       {`{"error":`, ""},
		"error: array":                           {`["invalid_grant"]`, ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, refreshErrorCode([]byte(test.body))); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestRefreshOutcomeRedactsAppliedMaterial(t *testing.T) {
	response, err := ParseRefreshResponse([]byte(`{"access_token":"planted-private-access","refresh_token":"planted-private-refresh","oai_is":"planted-private-opaque"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := RefreshOutcome{Kind: RefreshApplied, Response: response}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := json.MarshalWrite(&stream, out); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("refresh", "outcome", out)
	for _, shown := range []string{string(encoded), stream.String(), logged.String(), fmt.Sprintf("%v %+v %#v %s %q", out, out, out, out, out)} {
		if strings.Contains(shown, "planted-private") {
			t.Fatal("outcome leaked token material")
		}
	}
}

func TestRefreshCancelledBeforeTransportSendsNothing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out := newRefreshClient().post(ctx, server.URL, "agentctl/test", []byte(`{}`))
	if out.Kind != OutcomeNotSent || calls.Load() != 0 {
		t.Fatal("cancelled POST sent")
	}
}
