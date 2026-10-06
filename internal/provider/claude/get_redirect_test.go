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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
)

func TestClaudeGETDoesNotFollowRedirects(t *testing.T) {
	tests := map[string]struct {
		status int
	}{
		"error: moved permanently":  {http.StatusMovedPermanently},
		"error: found":              {http.StatusFound},
		"error: see other":          {http.StatusSeeOther},
		"error: temporary redirect": {http.StatusTemporaryRedirect},
		"error: permanent redirect": {http.StatusPermanentRedirect},
	}
	endpoints := map[string]struct {
		path    string
		profile bool
	}{
		"usage":   {path: UsagePath},
		"profile": {path: "/api/oauth/profile", profile: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for endpoint, mode := range endpoints {
				t.Run(endpoint, func(t *testing.T) {
					var sourceCalls, sinkCalls, sinkAuth atomic.Int64
					sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						sinkCalls.Add(1)
						if len(r.Header.Values("Authorization")) != 0 {
							sinkAuth.Add(1)
						}
						_, _ = io.WriteString(w, `{}`)
					}))
					defer sink.Close()
					origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						sourceCalls.Add(1)
						if r.TLS == nil || r.Method != http.MethodGet || r.URL.Path != mode.path {
							t.Errorf("origin request: TLS=%t method=%s path=%s", r.TLS != nil, r.Method, r.URL.Path)
						}
						if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-stored-access" {
							t.Error("origin did not receive the stored bearer")
						}
						w.Header().Set("Location", sink.URL+mode.path)
						w.WriteHeader(test.status)
					}))
					defer origin.Close()
					originURL, err := url.Parse(origin.URL)
					if err != nil {
						t.Fatal(err)
					}
					sinkURL, err := url.Parse(sink.URL)
					if err != nil {
						t.Fatal(err)
					}
					if originURL.Hostname() != sinkURL.Hostname() || originURL.Port() == sinkURL.Port() || originURL.Scheme != "https" || sinkURL.Scheme != "http" {
						t.Fatal("the redirect must downgrade TLS on the same hostname at a different port")
					}

					// Trust the loopback origin's certificate without replacing the
					// constructor's redirect policy or using a fake transport.
					if mode.profile {
						client := oauthClientFor(t, origin.URL)
						client.profileClient.Transport = origin.Client().Transport
						_, err = client.ProfileDocument(t.Context(), storedCredentials(t))
						httpErr, ok := errors.AsType[*errs.HTTPError](err)
						if !ok || httpErr.Status != test.status {
							t.Errorf("outcome=%v, want HTTPError with original status %d", err, test.status)
						}
					} else {
						client := testClient(origin)
						client.client.Transport = origin.Client().Transport
						_, err = client.Fetch(t.Context(), provider.AccountRef{ID: "acct", Auth: storedCredentials(t)})
						fetchErr, ok := errors.AsType[*provider.FetchError](err)
						if !ok || fetchErr.Kind != provider.FetchHTTP || fetchErr.Status != test.status {
							t.Errorf("outcome=%v, want FetchHTTP with original status %d", err, test.status)
						}
					}
					origin.Close()
					sink.Close()
					got := [3]int64{sourceCalls.Load(), sinkCalls.Load(), sinkAuth.Load()}
					t.Logf("source=%d sink=%d sink Authorization requests=%d outcome=%v", got[0], got[1], got[2], err)
					if diff := gocmp.Diff([3]int64{1, 0, 0}, got); diff != "" {
						t.Errorf("origin/sink/Authorization counts (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}
