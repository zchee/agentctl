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

package secret

import (
	"context"
	"strings"

	"github.com/zchee/agentctl/internal/errs"
)

// Reader reads credential blobs out of the macOS keychain.
//
// It is deliberately a reader: it declares no write and no delete, and it
// never grows one. Giving the rest of the program no vocabulary for a
// mutating keychain call is the cheapest way to keep "the read path never
// writes or deletes a keychain item" true. The write side, when it exists,
// is a separate transport that constructs its own targets.
type Reader interface {
	// Preflight asks whether the keychain can be read at all, before any
	// item is named, by running security(1) show-keychain-info under the
	// read budget. The answer is not memoized: a process that runs for
	// hours must notice a keychain that locks, or is unlocked, between
	// passes.
	Preflight(ctx context.Context) KeychainStatus

	// ListServices lists the generic-password items whose service name
	// starts with prefix, attributes only, by running security(1)
	// dump-keychain under the dump budget. No password material is
	// requested and none is returned.
	ListServices(ctx context.Context, prefix string) ([]ServiceEntry, error)

	// Read reads one item's password into a sealed [Secret] by running
	// security(1) find-generic-password under the read budget. A keychain
	// that is readable but holds no item under that service name returns an
	// error matching [ErrItemNotFound]; callers treat that as the normal
	// "needs login" answer, never as a reason to consult another store.
	Read(ctx context.Context, service string) (*Secret, error)
}

// KeychainState is what the keychain preflight found.
type KeychainState int

const (
	// KeychainStateUnsupported means this platform does not provide the
	// keychain transport. It is the zero value so an unset status refuses
	// rather than passing for readable.
	KeychainStateUnsupported KeychainState = iota
	// KeychainStateUnlocked means the keychain is readable.
	KeychainStateUnlocked
	// KeychainStateLocked means the keychain is present but locked; the
	// user must unlock it.
	KeychainStateLocked
	// KeychainStateUnavailable means the keychain is not reachable at all.
	KeychainStateUnavailable
	// KeychainStateTimeout means security(1) did not answer inside its
	// budget.
	KeychainStateTimeout
)

// String names the state for diagnostics.
func (s KeychainState) String() string {
	switch s {
	case KeychainStateUnsupported:
		return "unsupported"
	case KeychainStateUnlocked:
		return "unlocked"
	case KeychainStateLocked:
		return "locked"
	case KeychainStateUnavailable:
		return "unavailable"
	case KeychainStateTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// KeychainStatus is the keychain preflight's answer: the state, plus the
// reason security(1) gave when the keychain is unavailable.
type KeychainStatus struct {
	// State is what the preflight found.
	State KeychainState
	// Reason says why the keychain is unreachable; it is set only for
	// [KeychainStateUnavailable].
	Reason string
}

// ServiceEntry is one keychain item, attributes only. Every field but the
// service name may be empty, because security(1) prints <NULL> for anything
// unset; an item without a service name is dropped before it becomes an
// entry at all.
type ServiceEntry struct {
	// Service is the svce attribute.
	Service string
	// Account is the acct attribute, or empty when the item has none.
	Account string
	// CreatedAt is the cdat attribute as printed, or empty.
	CreatedAt string
	// ModifiedAt is the mdat attribute as printed, or empty.
	ModifiedAt string
}

// KeychainClassUnsupported is the class carried by a platform with no
// keychain transport. The errs vocabulary carries it verbatim rather than
// naming it, because no data flow branches on it.
const KeychainClassUnsupported = errs.KeychainClass("unsupported on this platform")

// KeychainError reports one failed read through security(1), classified.
type KeychainError struct {
	// Class names the failure the way the user sees it. The named errs
	// classes cover the branches the data flow takes; any other classified
	// stderr travels verbatim as its own class value, so widening the
	// classifier does not change this type.
	Class errs.KeychainClass
	// Detail carries the trimmed stderr, the spawn failure, or the exceeded
	// budget, phrased for a diagnostic line. It never contains item payload
	// bytes: the payload travels on stdout and is sealed into a [Secret]
	// before any error is built.
	Detail string
	// Transient reports whether a later attempt might succeed. A locked
	// keychain gets unlocked and a timeout is a busy machine; a missing
	// binary and an absent platform transport will not fix themselves. A
	// transient keychain failure is never answered by falling back to the
	// credential file, because the two stores can hold different accounts'
	// credentials and two readers of one refresh chain is how it dies.
	Transient bool
}

// The classified keychain failures callers branch on, as errors.Is targets.
// Matching compares the class only, so a wrapped failure carrying extra
// detail still matches its named value.
var (
	// ErrKeychainLocked matches a read refused by a locked keychain.
	ErrKeychainLocked = &KeychainError{Class: errs.KeychainLocked, Transient: true}
	// ErrKeychainUnavailable matches a keychain that is not reachable at
	// all, including a security(1) that could not be started.
	ErrKeychainUnavailable = &KeychainError{Class: errs.KeychainUnavailable, Transient: true}
	// ErrKeychainTimeout matches a security(1) child that exceeded its
	// budget and was killed.
	ErrKeychainTimeout = &KeychainError{Class: errs.KeychainTimeout, Transient: true}
	// ErrItemNotFound matches a readable keychain that holds no item under
	// the requested service name — the normal "needs login" answer.
	ErrItemNotFound = &KeychainError{Class: errs.KeychainNotFound}
	// ErrKeychainUnsupported matches a platform with no keychain transport.
	ErrKeychainUnsupported = &KeychainError{Class: KeychainClassUnsupported}
)

// Error names the classified failure, with the diagnostic detail appended
// when one was captured.
func (e *KeychainError) Error() string {
	if e.Detail == "" {
		return "keychain read failed: " + string(e.Class)
	}
	return "keychain read failed: " + string(e.Class) + ": " + e.Detail
}

// Is reports whether target names the same failure class, which is what
// makes the named values above work with errors.Is regardless of detail.
func (e *KeychainError) Is(target error) bool {
	other, ok := target.(*KeychainError)
	return ok && other.Class == e.Class
}

// Unwrap exposes the errs vocabulary, so errors.As reaches an
// [errs.KeychainError] and main maps the failure onto a partial exit rather
// than a fatal one.
func (e *KeychainError) Unwrap() error {
	return errs.NewKeychain(e.Class)
}

// StderrClass is one of the ten classes a security(1) stderr message falls
// into. The order of the constants is the order the classifier tries them
// in, and both match the vendor tooling this program coexists with; agreeing
// about the order is agreeing about whether a given failure is transient.
type StderrClass int

const (
	// StderrEmpty means nothing was written to stderr.
	StderrEmpty StderrClass = iota
	// StderrDuplicateItem means an item with that service name already
	// exists.
	StderrDuplicateItem
	// StderrKeychainUnavailable means the keychain file could not be
	// opened.
	StderrKeychainUnavailable
	// StderrNoKeychain means there is no default keychain.
	StderrNoKeychain
	// StderrItemNotFound means no item matched.
	StderrItemNotFound
	// StderrInteractionNotAllowed means the item exists but this process
	// may not be shown it without a prompt.
	StderrInteractionNotAllowed
	// StderrUserCanceled means the user dismissed the prompt.
	StderrUserCanceled
	// StderrAuthFailed means authentication or authorization failed.
	StderrAuthFailed
	// StderrKeychainLocked means the keychain is locked.
	StderrKeychainLocked
	// StderrOther is anything else.
	StderrOther
)

// String names the class the way the error vocabulary carries it verbatim,
// so a class that reaches the user through an unnamed [errs.KeychainClass]
// reads the same here as in the vendor tooling's reports.
func (c StderrClass) String() string {
	switch c {
	case StderrEmpty:
		return "Empty"
	case StderrDuplicateItem:
		return "DuplicateItem"
	case StderrKeychainUnavailable:
		return "KeychainUnavailable"
	case StderrNoKeychain:
		return "NoKeychain"
	case StderrItemNotFound:
		return "ItemNotFound"
	case StderrInteractionNotAllowed:
		return "InteractionNotAllowed"
	case StderrUserCanceled:
		return "UserCanceled"
	case StderrAuthFailed:
		return "AuthFailed"
	case StderrKeychainLocked:
		return "KeychainLocked"
	default:
		return "Other"
	}
}

// KeychainClass maps the stderr class onto the user-facing failure class.
// The three classes the data flow branches on get their named errs value;
// everything else travels verbatim.
func (c StderrClass) KeychainClass() errs.KeychainClass {
	switch c {
	case StderrItemNotFound:
		return errs.KeychainNotFound
	case StderrKeychainLocked:
		return errs.KeychainLocked
	case StderrKeychainUnavailable, StderrNoKeychain:
		return errs.KeychainUnavailable
	default:
		return errs.KeychainClass(c.String())
	}
}

// ClassifyStderr classifies a security(1) stderr message. First match wins,
// tried in the order the [StderrClass] constants are declared, matching
// case-insensitively on substrings. Several real messages contain more than
// one of the substrings below, so the order is load-bearing: it decides
// which class — and therefore which recovery advice — the user is shown.
func ClassifyStderr(stderr string) StderrClass {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return StderrEmpty
	}
	haystack := strings.ToLower(trimmed)
	has := func(needle string) bool { return strings.Contains(haystack, needle) }

	switch {
	case has("errsecduplicateitem") || has("already exists"):
		return StderrDuplicateItem
	case has("unable to open") || has("could not open"):
		return StderrKeychainUnavailable
	case has("errsecnodefaultkeychain") || has("default keychain") || has("no keychain"):
		return StderrNoKeychain
	case has("errsecitemnotfound") || has("item could not be found"):
		return StderrItemNotFound
	case has("errsecinteractionnotallowed") || has("interaction is not allowed") || has("no user interaction"):
		return StderrInteractionNotAllowed
	case has("errsecusercanceled") || has("cancel"):
		return StderrUserCanceled
	case has("errsecauthfailed") || has("authorization") || has("authentication") || has("name or passphrase"):
		return StderrAuthFailed
	case has("locked") || has("unlock"):
		return StderrKeychainLocked
	default:
		return StderrOther
	}
}

// DisabledReader answers "there is no keychain here" to everything. It is
// what a tagged build falls back to when no stand-in is wired, so a test
// that has not decided to talk to a keychain cannot reach the developer's
// own by accident.
type DisabledReader struct{}

var _ Reader = DisabledReader{}

// Preflight reports the keychain as unavailable, with the reason naming the
// deliberate switch-off rather than a failure.
func (DisabledReader) Preflight(context.Context) KeychainStatus {
	return KeychainStatus{State: KeychainStateUnavailable, Reason: "disabled"}
}

// ListServices lists nothing.
func (DisabledReader) ListServices(context.Context, string) ([]ServiceEntry, error) {
	return nil, nil
}

// Read finds no item, which is the same normal answer an empty keychain
// gives.
func (DisabledReader) Read(context.Context, string) (*Secret, error) {
	return nil, ErrItemNotFound
}
