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
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/provider/claude"
)

// UsagePath is the fixed path under the usage base URL.
const UsagePath = "/backend-api/wham/usage"

// ConnectTimeout bounds the resolve, connect and request-header phases.
const ConnectTimeout = 5 * time.Second

// MaxUsageBodyBytes bounds both encoded and decoded response bytes.
const MaxUsageBodyBytes = 1 << 22

// UsageClient fetches Codex usage without following redirects or storing cookies.
type UsageClient struct {
	baseURL   string
	userAgent string
	timeout   time.Duration
	client    *http.Client
}

var _ provider.UsageProvider[*Usage] = (*UsageClient)(nil)

// NewUsageClient constructs a client with separately bounded HTTP phases.
func NewUsageClient(baseURL, userAgent string, timeout time.Duration) *UsageClient {
	short := min(timeout, ConnectTimeout)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: short}).DialContext
	transport.TLSHandshakeTimeout = short
	transport.ResponseHeaderTimeout = timeout
	transport.DisableCompression = true
	return &UsageClient{baseURL: strings.TrimRight(baseURL, "/"), userAgent: userAgent, timeout: timeout, client: &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

// NewUsageClientFromEnv selects the build's fixed or testing-only endpoint.
func NewUsageClientFromEnv(timeout time.Duration) *UsageClient {
	return NewUsageClient(usageBaseURL(), provider.UserAgent(provider.Codex), timeout)
}

// UsageURL returns the complete endpoint URL.
func (c *UsageClient) UsageURL() string { return c.baseURL + UsagePath }

// Fetch fetches one account, classifying 401 separately from policy refusals.
// Invalid header values are rejected before any connection and never quoted.
func (c *UsageClient) Fetch(ctx context.Context, account provider.AccountRef) (*Usage, error) {
	if ctx.Err() != nil {
		return nil, provider.NewFetchCancelled()
	}
	slog.DebugContext(ctx, "fetching codex usage", "account.id", account.ID)
	if account.Auth == nil {
		return nil, provider.NewFetchParse("the authorization header value cannot be sent")
	}
	var result *Usage
	fetch := func(authorization string) error {
		scheme, credentials, ok := strings.Cut(authorization, " ")
		if !ok || !visibleHeader(scheme) || !visibleHeader(credentials) {
			return provider.NewFetchParse("the authorization header value cannot be sent")
		}
		headers := account.Auth.ExtraHeaders()
		foundID := false
		for _, header := range headers {
			if !visibleHeader(header.Value) {
				return provider.NewFetchParse("the " + header.Name + " header value cannot be sent")
			}
			foundID = foundID || strings.EqualFold(header.Name, "ChatGPT-Account-Id")
		}
		if !foundID {
			return provider.NewFetchParse("the ChatGPT-Account-Id header is missing")
		}
		var err error
		result, err = c.fetchAuthorized(ctx, authorization, headers)
		return err
	}
	// Concrete credentials keep the request and body read inside one exposure.
	if auth, ok := account.Auth.(interface {
		WithAuthorizationHeader(func(string) error) error
	}); ok {
		err := auth.WithAuthorizationHeader(fetch)
		return result, err
	}
	err := fetch(account.Auth.AuthorizationHeader())
	return result, err
}

func (c *UsageClient) fetchAuthorized(ctx context.Context, authorization string, headers []provider.Header) (*Usage, error) {
	var connection net.Conn
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			connection = info.Conn
			_ = connection.SetWriteDeadline(time.Now().Add(min(c.timeout, ConnectTimeout)))
		},
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			if connection != nil {
				_ = connection.SetWriteDeadline(time.Time{})
			}
		},
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, c.UsageURL(), nil)
	if err != nil {
		return nil, provider.NewFetchTransport("the usage request could not be constructed")
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("User-Agent", c.userAgent)
	for _, header := range headers {
		request.Header.Set(header.Name, header.Value)
	}
	defer request.Header.Del("Authorization")
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, provider.NewFetchCancelled()
		}
		return nil, provider.NewFetchTransport("the usage request did not complete")
	}
	defer func() { _ = response.Body.Close() }()
	switch status := response.StatusCode; {
	case status >= 200 && status <= 299:
	case status == 401:
		return nil, provider.NewFetchUnauthorized()
	case status == 429:
		if after, ok := claude.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()); ok {
			return nil, provider.NewFetchRateLimitedAfter(after)
		}
		return nil, provider.NewFetchRateLimited()
	default:
		return nil, provider.NewFetchHTTP(status)
	}
	if connection != nil {
		_ = connection.SetReadDeadline(time.Now().Add(c.timeout))
		defer func() { _ = connection.SetReadDeadline(time.Time{}) }()
	}
	wire := &io.LimitedReader{R: response.Body, N: MaxUsageBodyBytes + 1}
	var decoded io.Reader = wire
	if strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		compressed, err := gzip.NewReader(wire)
		if err != nil {
			return nil, provider.NewFetchParse("the response body could not be decoded")
		}
		defer func() { _ = compressed.Close() }()
		decoded = compressed
	}
	body, err := io.ReadAll(io.LimitReader(decoded, MaxUsageBodyBytes+1))
	if err != nil {
		return nil, provider.NewFetchParse("the response body could not be read")
	}
	if len(body) > MaxUsageBodyBytes || wire.N == 0 {
		return nil, provider.NewFetchParse("the response body is larger than this build reads")
	}
	parsed, err := Normalize(body, time.Now(), true)
	if err != nil {
		return nil, provider.NewFetchParse("the response was not a JSON object")
	}
	return parsed, nil
}

func visibleHeader(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range []byte(value) {
		if ch < 0x21 || ch > 0x7e {
			return false
		}
	}
	return true
}
