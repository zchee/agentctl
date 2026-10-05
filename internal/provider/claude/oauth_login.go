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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
)

// LoginClient adds authorization and code exchange to the shared OAuth transport.
type LoginClient struct {
	*OAuthClient
	authorizeURL string
}

// NewLoginClient builds a login client against explicit endpoints.
func NewLoginClient(authorizeURL, tokenURL, profileURL, userAgent string) (*LoginClient, error) {
	if _, err := url.ParseRequestURI(authorizeURL); err != nil {
		return nil, errs.NewConfig("the authorize URL is not a usable OAuth endpoint")
	}
	client, err := NewOAuthClient(tokenURL, profileURL, userAgent)
	if err != nil {
		return nil, err
	}
	return &LoginClient{OAuthClient: client, authorizeURL: authorizeURL}, nil
}

// NewLoginClientFromEnv selects the built-in or testing-only OAuth endpoints.
func NewLoginClientFromEnv() (*LoginClient, error) {
	return NewLoginClient(oauthAuthorizeURL(), oauthTokenURL(), oauthProfileURL(), provider.UserAgent(provider.Claude))
}

// AuthorizeURL constructs the URL for this login's verifier and redirect.
func (c *LoginClient) AuthorizeURL(pkce *PKCE, redirect Redirect, scopes []string) (string, error) {
	return AuthorizeURL(c.authorizeURL, pkce, redirect, scopes)
}

// Exchange trades a code for tokens, retrying only a definite rate limit once.
func (c *LoginClient) Exchange(ctx context.Context, code, state string, pkce *PKCE, redirect Redirect) (*TokenResponse, error) {
	return c.exchange(ctx, code, state, pkce, redirect, RateLimitRetryFloor)
}

func (c *LoginClient) exchange(ctx context.Context, code, state string, pkce *PKCE, redirect Redirect, retryFloor time.Duration) (*TokenResponse, error) {
	attempt := func() (*TokenResponse, error) {
		var response *TokenResponse
		err := pkce.verifier.WithPlaintext(func(verifier []byte) error {
			quoted, err := jsontext.AppendQuote(nil, verifier)
			if err != nil {
				return err
			}
			defer memguard.WipeBytes(quoted)
			body, err := json.Marshal(struct {
				GrantType    string         `json:"grant_type"`
				Code         string         `json:"code"`
				RedirectURI  string         `json:"redirect_uri"`
				ClientID     string         `json:"client_id"`
				CodeVerifier jsontext.Value `json:"code_verifier"`
				State        string         `json:"state"`
			}{"authorization_code", code, redirect.URI(), c.clientID, quoted, state})
			defer memguard.WipeBytes(body)
			if err != nil {
				return err
			}
			response, err = c.postExchange(ctx, body)
			return err
		})
		return response, err
	}
	response, err := attempt()
	httpErr, ok := errors.AsType[*errs.HTTPError](err)
	if !ok || httpErr.Status != http.StatusTooManyRequests {
		return response, err
	}
	wait := max(retryFloor, httpErr.RetryAfter)
	slog.Warn("the token endpoint is rate limited; retrying once", "wait_seconds", int64(wait/time.Second))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, provider.NewFetchCancelled()
	case <-timer.C:
		return attempt()
	}
}

func (c *LoginClient) postExchange(ctx context.Context, body []byte) (*TokenResponse, error) {
	if ctx.Err() != nil {
		return nil, provider.NewFetchCancelled()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewFetchTransport("could not build the token request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	response, err := c.tokenClient.Do(request)
	if err != nil {
		return nil, mapTransportError(err)
	}
	defer func() { _ = response.Body.Close() }()
	text, err := readOAuthBody(response.Body)
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(text)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		err := classifyTokenFailure(response.StatusCode, response.Header, text)
		if httpErr, ok := errors.AsType[*errs.HTTPError](err); ok && httpErr.Status == http.StatusTooManyRequests {
			// Only delta-seconds are part of the login retry contract.
			seconds, parseErr := strconv.ParseUint(strings.TrimSpace(response.Header.Get("Retry-After")), 10, 64)
			httpErr.HasRetryAfter = parseErr == nil && seconds <= math.MaxInt64/uint64(time.Second)
			httpErr.RetryAfter = 0
			if httpErr.HasRetryAfter {
				httpErr.RetryAfter = time.Duration(seconds) * time.Second
			}
		}
		return nil, err
	}
	token, err := parseTokenResponse(text)
	if err != nil {
		return nil, provider.NewFetchParse("the token response could not be parsed")
	}
	return token, nil
}

// LoginCredentials converts a fresh grant using the checked refresh expiry logic.
func LoginCredentials(response *TokenResponse, nowMillis int64, scopes []string) (*Credentials, error) {
	if response.TokenType != nil && !strings.EqualFold(*response.TokenType, "Bearer") {
		slog.Warn("the token endpoint returned a token type other than Bearer")
	}
	credentials := &Credentials{Scopes: slices.Clone(scopes), ClientID: new(ClientID)}
	workspace, hasWorkspace := objectMembers(response.Workspace)
	if response.Account != nil || response.Organization != nil || hasWorkspace {
		account := &TokenAccount{}
		if response.Account != nil {
			account.UUID = new(response.Account.UUID)
			account.EmailAddress = response.Account.EmailAddress
		}
		if response.Organization != nil {
			account.OrganizationUUID = new(response.Organization.UUID)
			account.OrganizationName = response.Organization.Name
		}
		if hasWorkspace {
			id, found := field(workspace, "id")
			if !found {
				id, _ = field(workspace, "uuid")
			}
			if value, ok := takeString(id); ok {
				account.WorkspaceID = new(value)
			}
			if name, ok := stringField(workspace, "name"); ok {
				account.WorkspaceName = new(name)
			}
		}
		credentials.TokenAccount = account
	}
	if err := credentials.MergeRefresh(response, nowMillis); err != nil {
		return nil, err
	}
	return credentials, nil
}
