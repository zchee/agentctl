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
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/secret"
)

type lockedCredentialState struct {
	inner         *Credentials
	guard         *Lock
	user, account string
	base          *secret.Digests
	consumed      atomic.Bool
}

// LockedCredentials is a credential bound to one live lock and its original grant.
type LockedCredentials struct{ state *lockedCredentialState }

// Valid reports whether this carrier remains bound to a live namespace lock.
func (c *LockedCredentials) Valid() bool {
	return c != nil && c.state != nil && c.state.guard.Valid() && !c.state.consumed.Load()
}

// Lock returns the live lock identity, or nil for an invalid carrier.
func (c *LockedCredentials) Lock() *Lock {
	if c == nil || c.state == nil {
		return nil
	}
	return c.state.guard
}

// IDs returns the namespace that produced this read, never token claims.
func (c *LockedCredentials) IDs() (string, string) {
	if c == nil || c.state == nil {
		return "", ""
	}
	return c.state.user, c.state.account
}

// Credentials exposes the read-only document without a refresh capability.
func (c *LockedCredentials) Credentials() *Credentials {
	if c == nil || c.state == nil {
		return nil
	}
	return c.state.inner
}

// IntoCredentials consumes this wrapper's refresh and write capabilities.
func (c *LockedCredentials) IntoCredentials() (*Credentials, error) {
	if c == nil || c.state == nil {
		return nil, errInvalidProof
	}
	c.state.guard.state.mu.Lock()
	defer c.state.guard.state.mu.Unlock()
	if !c.Valid() || c.state.consumed.Swap(true) {
		return nil, errInvalidProof
	}
	return c.state.inner, nil
}

// BaseDigests returns an isolated copy of the original locked-read grant.
func (c *LockedCredentials) BaseDigests() *secret.Digests {
	if c == nil || c.state == nil {
		return nil
	}
	return cloneDigests(c.state.base)
}

func cloneDigests(value *secret.Digests) *secret.Digests {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

// RefreshDigest8 returns only a refresh-token fingerprint.
func (c *LockedCredentials) RefreshDigest8() (string, bool) {
	if !c.Valid() {
		return "", false
	}
	return c.state.inner.RefreshDigest8()
}

// WithRefreshBody scopes the complete POST body to the token exposure window.
// The callback must perform one I/O and must not retain its body.
func (c *LockedCredentials) WithRefreshBody(clientID string, fn func([]byte) error) error {
	if c == nil || c.state == nil || c.state.guard == nil || c.state.guard.state == nil {
		return errInvalidProof
	}
	c.state.guard.state.mu.Lock()
	defer c.state.guard.state.mu.Unlock()
	if !c.Valid() {
		return errInvalidProof
	}
	token := c.state.inner.token("refresh_token")
	if token == nil {
		return errors.New("no refresh token")
	}
	return token.WithPlaintext(func(data []byte) error {
		var buffer bytes.Buffer
		defer func() { memguard.WipeBytes(buffer.Bytes()) }()
		enc := jsontext.NewEncoder(&buffer)
		for _, tok := range []jsontext.Token{jsontext.BeginObject, jsontext.String("client_id"), jsontext.String(clientID), jsontext.String("grant_type"), jsontext.String("refresh_token"), jsontext.String("refresh_token"), jsontext.String(string(data)), jsontext.EndObject} {
			if err := enc.WriteToken(tok); err != nil {
				return err
			}
		}
		return fn(bytes.TrimSpace(buffer.Bytes()))
	})
}

// MergeOutcome contains non-secret facts about an applied refresh response.
type MergeOutcome struct{ IdentityDrift, RefreshRotated, IDTokenUnreadable bool }

// MergeRefresh consumes this wrapper and applies optional token members in order.
// An unreadable id token is omitted without losing the rotated access/refresh grant.
func (c *LockedCredentials) MergeRefresh(response *RefreshResponse, now time.Time) (*LockedCredentials, MergeOutcome, error) {
	var outcome MergeOutcome
	if c == nil || c.state == nil || c.state.guard == nil || c.state.guard.state == nil || response == nil {
		return nil, outcome, errInvalidProof
	}
	c.state.guard.state.mu.Lock()
	defer c.state.guard.state.mu.Unlock()
	if !c.Valid() {
		return nil, outcome, errInvalidProof
	}
	if tokens := c.state.inner.document.root.member("tokens"); tokens == nil || tokens.kind != '{' {
		return nil, outcome, &CredentialsError{Kind: "missing", Field: "tokens"}
	}
	inner := &Credentials{document: AuthDocument{root: cloneAuthValue(c.state.inner.document.root)}}
	beforeRefresh, beforePresent := c.state.inner.RefreshDigest8()
	var beforeUser *string
	if claims := c.state.inner.claims; claims != nil {
		beforeUser = claims.ChatGPTUserID
	}
	idToken := response.idToken
	if idToken != nil {
		if err := idToken.WithPlaintext(func(data []byte) error { _, err := ParseClaims(data); return err }); err != nil {
			outcome.IDTokenUnreadable = true
			idToken = nil
		}
	}
	for _, entry := range []struct {
		name  string
		token *secret.Secret
	}{{"id_token", idToken}, {"access_token", response.accessToken}, {"refresh_token", response.refreshToken}} {
		if entry.token != nil {
			inner.document.root.member("tokens").set(entry.name, &authValue{kind: 'n', sealed: entry.token, stringSecret: true})
		}
	}
	stamp, err := jsontext.AppendQuote(nil, codexTimestamp(now))
	if err != nil {
		return nil, outcome, err
	}
	inner.document.root.set("last_refresh", &authValue{kind: '"', raw: stamp})
	if err := inner.derive(); err != nil {
		inner.document.root.wipe()
		return nil, outcome, err
	}
	outcome.IdentityDrift = inner.IdentityDrift()
	if beforeUser != nil && inner.claims != nil && inner.claims.ChatGPTUserID != nil && *beforeUser != *inner.claims.ChatGPTUserID {
		outcome.IdentityDrift = true
	}
	afterRefresh, afterPresent := inner.RefreshDigest8()
	outcome.RefreshRotated = beforePresent != afterPresent || beforeRefresh != afterRefresh
	c.state.consumed.Store(true)
	return &LockedCredentials{state: &lockedCredentialState{inner: inner, guard: c.state.guard, user: c.state.user, account: c.state.account, base: cloneDigests(c.state.base)}}, outcome, nil
}

func cloneAuthValue(v *authValue) *authValue {
	if v == nil {
		return nil
	}
	result := *v
	result.raw = bytes.Clone(v.raw)
	result.object = make([]authMember, len(v.object))
	for i, m := range v.object {
		result.object[i] = authMember{m.name, cloneAuthValue(m.value)}
	}
	result.array = make([]*authValue, len(v.array))
	for i, a := range v.array {
		result.array[i] = cloneAuthValue(a)
	}
	return &result
}

func (v *authValue) set(name string, value *authValue) {
	index := slices.IndexFunc(v.object, func(m authMember) bool { return m.name == name })
	if index < 0 {
		v.object = append(v.object, authMember{name, value})
		return
	}
	v.object[index].value.wipe()
	v.object[index].value = value
}

func codexTimestamp(now time.Time) string {
	at := now.UTC()
	base := at.Format("2006-01-02T15:04:05")
	nanos := at.Nanosecond()
	switch {
	case nanos == 0:
		return base + "Z"
	case nanos%1_000_000 == 0:
		return fmt.Sprintf("%s.%03dZ", base, nanos/1_000_000)
	case nanos%1_000 == 0:
		return fmt.Sprintf("%s.%06dZ", base, nanos/1_000)
	default:
		return fmt.Sprintf("%s.%09dZ", base, nanos)
	}
}

// String redacts the locked document.
func (LockedCredentials) String() string { return "LockedCredentials{[REDACTED]}" }

// Format redacts every formatting verb.
func (LockedCredentials) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "LockedCredentials{[REDACTED]}")
}

// LogValue redacts structured logging.
func (LockedCredentials) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalJSON redacts generic serialization.
func (LockedCredentials) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
