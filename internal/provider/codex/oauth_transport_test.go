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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRefreshRealSocketFailuresDoNotAuthorizeUncertainResend(t *testing.T) {
	tests := map[string]struct {
		phase string
		kind  OutcomeKind
		class UnknownClass
		posts int32
	}{
		"success: closed port proves never sent":       {"closed", OutcomeNotSent, UnknownTransport, 0},
		"error: reset after request body is ambiguous": {"reset", OutcomeUnknown, UnknownTransport, 1},
		"error: absent response is ambiguous":          {"headers", OutcomeUnknown, UnknownTransport, 1},
		"error: self signed TLS is not never sent":     {"certificate", OutcomeUnknown, UnknownTLS, 0},
		"error: plaintext TLS handshake is ambiguous":  {"handshake", OutcomeUnknown, UnknownTLS, 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var posts atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				switch test.phase {
				case "reset":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if tcp, ok := connection.(*net.TCPConn); ok {
						if err := tcp.SetLinger(0); err != nil {
							t.Error(err)
						}
					}
					_ = connection.Close()
				case "headers":
					<-r.Context().Done()
				default:
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			})
			server := httptest.NewUnstartedServer(handler)
			if test.phase == "certificate" {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			endpoint := server.URL
			if test.phase == "handshake" {
				endpoint = strings.Replace(endpoint, "http:", "https:", 1)
			}
			if test.phase == "closed" {
				server.Close()
			}
			phases := DefaultPhaseTimeouts()
			phases.RecvResponse = 100 * time.Millisecond
			out := newRefreshClient(phases).post(t.Context(), endpoint, "socket-test", []byte(`{"grant_type":"refresh_token"}`))
			if diff := gocmp.Diff(test.kind, out.Kind); diff != "" {
				t.Fatalf("%s reason=%s", diff, out.Reason)
			}
			if diff := gocmp.Diff(test.class, out.Class); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(test.posts, posts.Load()); diff != "" {
				t.Fatal(diff)
			}
			if out.AllowsAutomaticResend() != (test.kind == OutcomeNotSent) {
				t.Fatal("uncertain failure authorized automatic resend")
			}
		})
	}
}

func TestRefreshCookieFromPreviousAnswerIsNeverSent(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.Header.Get("Cookie") != "" {
			t.Error("a prior token-host cookie was sent")
		}
		w.Header().Set("Set-Cookie", "token-host-cookie=private; Path=/")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client := newRefreshClient()
	for range 2 {
		out := client.post(t.Context(), server.URL, "socket-test", []byte(`{}`))
		clear(out.Body)
		if out.Kind != OutcomeResponse || !out.BodyComplete {
			t.Fatal("loopback response failed")
		}
	}
	if diff := gocmp.Diff(int32(2), posts.Load()); diff != "" {
		t.Fatal(diff)
	}
}
