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

package provider

import (
	"errors"
	"testing"
	"time"
)

func TestTransienceDecidesWhetherAnotherPassIsWorthIt(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err       error
		transient bool
	}{
		"success: rate limited with a hint is worth another pass": {
			err:       NewFetchRateLimitedAfter(30 * time.Second),
			transient: true,
		},
		"success: rate limited without a hint is worth another pass": {
			err:       NewFetchRateLimited(),
			transient: true,
		},
		"success: a dropped connection is worth another pass": {
			err:       NewFetchTransport("connection reset"),
			transient: true,
		},
		"success: HTTP 500 is worth another pass": {
			err:       NewFetchHTTP(500),
			transient: true,
		},
		"success: HTTP 503 is worth another pass": {
			err:       NewFetchHTTP(503),
			transient: true,
		},
		"error: a rejected token fails the same way next pass": {
			err:       NewFetchUnauthorized(),
			transient: false,
		},
		"error: HTTP 400 fails the same way next pass": {
			err:       NewFetchHTTP(400),
			transient: false,
		},
		"error: HTTP 404 fails the same way next pass": {
			err:       NewFetchHTTP(404),
			transient: false,
		},
		"error: an unreadable document fails the same way next pass": {
			err:       NewFetchParse("no limits array"),
			transient: false,
		},
		"error: a cancelled pass is not retried by the next one": {
			err:       NewFetchCancelled(),
			transient: false,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fetchErr, ok := errors.AsType[*FetchError](tt.err)
			if !ok {
				t.Fatalf("every constructor must produce a *FetchError, got %T", tt.err)
			}
			if got := fetchErr.IsTransient(); got != tt.transient {
				t.Fatalf("IsTransient() = %v, want %v for %v", got, tt.transient, tt.err)
			}
		})
	}
}

func TestErrorMessagesNameTheFailureWithoutABody(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want string
	}{
		"success: rate limited renders its hint when the server sent one": {
			err:  NewFetchRateLimitedAfter(30 * time.Second),
			want: "rate limited, retry in 30s",
		},
		"success: rate limited claims no window the server never named": {
			err:  NewFetchRateLimited(),
			want: "rate limited",
		},
		"success: an HTTP failure names the status and nothing else": {
			err:  NewFetchHTTP(502),
			want: "HTTP 502",
		},
		"success: a rejected token is named without the token": {
			err:  NewFetchUnauthorized(),
			want: "the access token was rejected",
		},
		"success: a transport failure carries the classifier's words": {
			err:  NewFetchTransport("connection reset"),
			want: "connection reset",
		},
		"success: a parse failure names the document problem": {
			err:  NewFetchParse("no limits array"),
			want: "the usage response could not be read: no limits array",
		},
		"success: a cancelled pass says so": {
			err:  NewFetchCancelled(),
			want: "cancelled",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
