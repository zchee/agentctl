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
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/secret"
)

// ManualRedirectURI displays the authorization code for a manual login.
const ManualRedirectURI = "https://platform.claude.com/oauth/code/callback"

// ErrStateMismatch means the callback did not belong to this login.
var ErrStateMismatch = errors.New("the authorization response carried the wrong `state`; nothing was written")

// Redirect selects manual login when Port is zero, otherwise the loopback port.
type Redirect struct {
	Port uint16
}

// URI returns the identical redirect used for authorization and exchange.
func (r Redirect) URI() string {
	if r.Port == 0 {
		return ManualRedirectURI
	}
	return "http://localhost:" + strconv.Itoa(int(r.Port)) + "/callback"
}

// PKCE contains one login's sealed verifier, public challenge, and random state.
type PKCE struct {
	verifier  *secret.Secret
	Challenge string
	State     string
}

// NewPKCE generates two independent 32-byte nonces using the system CSPRNG.
func NewPKCE() (*PKCE, error) {
	var nonce [32]byte
	_, _ = rand.Read(nonce[:])
	verifierBytes := base64.RawURLEncoding.AppendEncode(nil, nonce[:])
	digest := sha256.Sum256(verifierBytes)
	verifier, err := secret.NewSecret(verifierBytes)
	if err != nil {
		return nil, err
	}
	_, _ = rand.Read(nonce[:])
	state := base64.RawURLEncoding.EncodeToString(nonce[:])
	memguard.WipeBytes(nonce[:])
	return &PKCE{verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(digest[:]), State: state}, nil
}

// String returns a representation without the verifier.
func (PKCE) String() string { return "PKCE{verifier: [REDACTED]}" }

// Format redacts the verifier for every formatting verb.
func (p PKCE) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, p.String()) }

// LogValue excludes the verifier from structured logging.
func (p PKCE) LogValue() slog.Value { return slog.StringValue(p.String()) }

// MarshalJSON excludes the verifier from serialization.
func (PKCE) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// CodeChallenge returns the unpadded S256 challenge from RFC 7636.
func CodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// RequestedScopes returns the configured space-separated scopes or the defaults.
func RequestedScopes() []string {
	if scopes := strings.Fields(os.Getenv("AGENTCTL_CLAUDE_OAUTH_SCOPES")); len(scopes) != 0 {
		return scopes
	}
	return slices.Clone(DefaultScopes)
}

// AuthorizeURL builds the ordered authorization query without exposing the verifier.
func AuthorizeURL(endpoint string, pkce *PKCE, redirect Redirect, scopes []string) (string, error) {
	u, err := url.ParseRequestURI(endpoint)
	if err != nil {
		return "", errors.New("the authorize URL is unusable")
	}
	pairs := [][2]string{
		{"code", "true"},
		{"client_id", ClientID},
		{"response_type", "code"},
		{"redirect_uri", redirect.URI()},
		{"scope", strings.Join(scopes, " ")},
		{"code_challenge", pkce.Challenge},
		{"code_challenge_method", "S256"},
		{"state", pkce.State},
	}
	for _, pair := range pairs {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += url.QueryEscape(pair[0]) + "=" + url.QueryEscape(pair[1])
	}
	return u.String(), nil
}

// ParseManualCode splits at the first separator and refuses incomplete pastes.
func ParseManualCode(input string) (code, state string, err error) {
	code, state, found := strings.Cut(strings.TrimSpace(input), "#")
	if !found {
		return "", "", errors.New("expected a `code#state` value; the pasted text has no `#`")
	}
	code, state = strings.TrimSpace(code), strings.TrimSpace(state)
	if code == "" || state == "" {
		return "", "", errors.New("expected a `code#state` value; one half of the pasted text is empty")
	}
	return code, state, nil
}

// VerifyState compares the returned state in constant time for equal lengths.
func VerifyState(expected, actual string) error {
	if subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) != 1 {
		return ErrStateMismatch
	}
	return nil
}
