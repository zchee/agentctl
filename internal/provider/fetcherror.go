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
	"fmt"
	"time"
)

// FetchKind classifies why a usage fetch did not produce a snapshot.
type FetchKind string

const (
	// FetchUnauthorized means the server rejected the bearer token.
	//
	// For an account agentctl owns this is the refresh-once trigger: the
	// access token expired between the expiry check and the request, which
	// happens routinely because the two use different clocks.
	FetchUnauthorized FetchKind = "unauthorized"

	// FetchRateLimited means the server asked the caller to slow down.
	FetchRateLimited FetchKind = "rate limited"

	// FetchHTTP means the server answered with a status the run cannot use.
	FetchHTTP FetchKind = "http"

	// FetchTransport means the request never completed: connection, TLS,
	// timeout.
	FetchTransport FetchKind = "transport"

	// FetchParse means the response arrived but was not the document this
	// build understands.
	FetchParse FetchKind = "parse"

	// FetchCancelled means the pass was cancelled or hit its deadline
	// before the request.
	FetchCancelled FetchKind = "cancelled"
)

// FetchError reports why a usage fetch did not produce a snapshot.
//
// Its message never carries a response body: a token can appear in an
// echoed request, and this string reaches stderr and the JSON report.
type FetchError struct {
	// Kind is the classified failure.
	Kind FetchKind
	// Status is the HTTP status as received. Meaningful only for
	// [FetchHTTP].
	Status int
	// RetryAfter is the server's retry-after hint on a [FetchRateLimited]
	// failure. It is meaningful only when HasRetryAfter is true, so a hint
	// of zero seconds is distinct from no hint at all.
	RetryAfter time.Duration
	// HasRetryAfter reports whether the server sent a parseable
	// retry-after hint. The message claims a retry window only when it
	// did: a user who reads "retry in 0s" and retries immediately gets
	// another rejection and no explanation.
	HasRetryAfter bool
	// Message carries the transport failure or the parse failure in the
	// classifier's words, for [FetchTransport] and [FetchParse].
	Message string
}

// NewFetchUnauthorized returns the [FetchError] for a rejected bearer
// token.
func NewFetchUnauthorized() error {
	return &FetchError{Kind: FetchUnauthorized}
}

// NewFetchRateLimited returns the [FetchError] for a rate-limit answer
// that carried no usable retry-after hint.
func NewFetchRateLimited() error {
	return &FetchError{Kind: FetchRateLimited}
}

// NewFetchRateLimitedAfter returns the [FetchError] for a rate-limit
// answer whose retry-after hint parsed.
func NewFetchRateLimitedAfter(retryAfter time.Duration) error {
	return &FetchError{Kind: FetchRateLimited, RetryAfter: retryAfter, HasRetryAfter: true}
}

// NewFetchHTTP returns the [FetchError] for any other status the run
// cannot use.
func NewFetchHTTP(status int) error {
	return &FetchError{Kind: FetchHTTP, Status: status}
}

// NewFetchTransport returns the [FetchError] for a request that never
// completed.
func NewFetchTransport(message string) error {
	return &FetchError{Kind: FetchTransport, Message: message}
}

// NewFetchParse returns the [FetchError] for a response that was not the
// document this build understands.
func NewFetchParse(message string) error {
	return &FetchError{Kind: FetchParse, Message: message}
}

// NewFetchCancelled returns the [FetchError] for a pass that was cancelled
// or hit its deadline before the request.
func NewFetchCancelled() error {
	return &FetchError{Kind: FetchCancelled}
}

// Error states the classified failure in the user's terms, without a
// response body.
func (e *FetchError) Error() string {
	switch e.Kind {
	case FetchUnauthorized:
		return "the access token was rejected"
	case FetchRateLimited:
		if e.HasRetryAfter {
			return fmt.Sprintf("rate limited, retry in %ds", int64(e.RetryAfter/time.Second))
		}
		return "rate limited"
	case FetchHTTP:
		return fmt.Sprintf("HTTP %d", e.Status)
	case FetchTransport:
		return e.Message
	case FetchParse:
		return "the usage response could not be read: " + e.Message
	case FetchCancelled:
		return "cancelled"
	default:
		return "usage fetch failed"
	}
}

// IsTransient reports whether retrying later stands a chance.
//
// It drives whether the row is shown as stale — worth another pass — or as
// a hard failure. A 5xx and a dropped connection are transient; a rejected
// token and an unreadable document are not, because the next pass would do
// exactly the same thing and fail exactly the same way.
func (e *FetchError) IsTransient() bool {
	switch e.Kind {
	case FetchRateLimited, FetchTransport:
		return true
	case FetchHTTP:
		return e.Status >= 500
	default:
		return false
	}
}
