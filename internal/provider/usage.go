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
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// Header is one HTTP header pair a credential requires on a usage request
// beyond the authorization itself.
type Header struct {
	// Name is the header field name.
	Name string
	// Value is the header field value as it will be sent.
	Value string
}

// UsageAuth is what one account authenticates a usage request with.
//
// It is the seam between a provider's credential type and the request its
// usage client builds. It is deliberately two methods and no accessor: a
// caller gets a finished header value, never the sealed secret and never a
// field it could expose again, so the plaintext is taken out at each
// provider's single private exposure site and nowhere else. The header
// value does of course carry the token — that is what a bearer header is —
// so it is a value to build a request from and not one to log.
//
// Nothing about this interface redacts. What keeps a token out of a log
// line or a panic payload is [AccountRef]'s own formatting, which never
// renders its credential. Each provider's credential type still owes its
// own redacting formatter for when it is rendered directly.
type UsageAuth interface {
	// AuthorizationHeader returns the Authorization header value for a
	// request made as this account.
	AuthorizationHeader() string

	// ExtraHeaders returns the header pairs this credential requires beyond
	// the authorization, such as Codex's account id and its FedRAMP flag.
	//
	// Nil for Claude: the anthropic-beta header is a property of the
	// endpoint, not of the credential, so it stays where the request is
	// built.
	ExtraHeaders() []Header
}

// AccountRef is the one account a [UsageProvider] fetch call is about.
//
// The credential arrives as a [UsageAuth] interface value rather than as a
// concrete credential type, which is what lets a second provider's usage
// client be written against this one type. It also narrows what a holder
// can reach to the finished header values: there is no accessor beyond
// those, where a concrete credential would put the sealed token one method
// call away.
type AccountRef struct {
	// ID is the row's display id, for log lines. Never a secret.
	ID string
	// Auth is how to authenticate as this account.
	Auth UsageAuth
}

// Format renders the row id and nothing about the credential, whatever the
// verb and flags.
//
// Hand-written rather than left to the fmt defaults because the default
// struct rendering prints whatever sits behind Auth, and nothing forces an
// implementation to redact itself — a credential type that holds its token
// in a plain field would put it into every %+v of an account. Redaction
// here must not depend on that discipline.
func (a AccountRef) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, a.String())
}

// String returns the row id and nothing about the credential.
func (a AccountRef) String() string {
	return "AccountRef{ID: " + strconv.Quote(a.ID) + "}"
}

// GoString returns the row id and nothing about the credential.
func (a AccountRef) GoString() string {
	return a.String()
}

// LogValue returns the row id and nothing about the credential, so a
// structured log line that attaches an account carries its identity only.
func (a AccountRef) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", a.ID))
}

// UsageProvider is one vendor's usage API.
//
// It is generic over the snapshot a fetch produces so this package can pin
// the seam the commands consume without depending on the model package
// that defines the snapshot; the commands instantiate it with the
// normalized usage model.
type UsageProvider[Snapshot any] interface {
	// Fetch fetches one account's current usage.
	//
	// Implementations must honour ctx — a pass that has been cancelled or
	// has passed its deadline gets the cancelled [FetchError] rather than a
	// request — and must bound the request in time themselves, because a
	// caller can abandon a fetch but not interrupt a blocked socket read
	// that ignores its context.
	Fetch(ctx context.Context, account AccountRef) (Snapshot, error)
}
