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
	"fmt"

	"github.com/zchee/agentctl/internal/config"
)

// AccountStateKind is the stable, machine-readable token for one account
// state: what the JSON report puts in its state member and what a script
// branches on, so it changes only with the report version. It never
// carries the state's payload — the label spells that out for a reader.
type AccountStateKind string

// The account-state vocabulary. Every value is a statement about one row
// of the status table.
const (
	// StateOK: fresh credentials; usage was or can be fetched.
	StateOK AccountStateKind = "ok"
	// StateExpired: the access token has expired.
	StateExpired AccountStateKind = "expired"
	// StateNeedsLogin: there is no usable credential; the user must log
	// in.
	StateNeedsLogin AccountStateKind = "needs_login"
	// StateIdentityUnknown: a credential was found but does not say who
	// it belongs to. Older blobs carry no identity block, and a path is
	// not identity.
	StateIdentityUnknown AccountStateKind = "identity_unknown"
	// StateStaleSiblingOfLive: a keychain item naming the same physical
	// directory as the live store, holding different credentials. Hidden
	// by default.
	StateStaleSiblingOfLive AccountStateKind = "stale_sibling_of_live"
	// StateUnclaimed: a Claude Code credentials item that no registry
	// record claims.
	StateUnclaimed AccountStateKind = "unclaimed"
	// StateForeign: a credential belonging to something that is neither
	// agentctl nor Claude Code — another tool's keychain item. Listed so
	// the user can see it is there, hidden by default, and never read.
	// Distinct from StateUnclaimed, which is a Claude Code credentials
	// item agentctl could adopt; nothing here is adoptable, so a consumer
	// branching on the state token must be able to tell the two apart.
	StateForeign AccountStateKind = "foreign"
	// StateForgotten: hidden at the user's request.
	StateForgotten AccountStateKind = "forgotten"
	// StateMigratedToKeychain: a keychain item exists for this namespace,
	// because a Claude Code session migrated the credentials out of the
	// file. Displayed from the keychain, never refreshed, never written.
	StateMigratedToKeychain AccountStateKind = "migrated_to_keychain"
	// StateAdopted: this namespace's keychain item is held by a different
	// identity — a hot swap put another account there — and the record's
	// own credential is in the adopted copy beside it. Distinct from
	// StateMigratedToKeychain, which is the same account's credential in
	// a place agentctl may not write: here the item is readable and
	// writable but is not this row's, so it is never read for this row,
	// never refreshed for it, and never adopted into it. The row is still
	// perfectly usable, which is why this is not a failure state.
	StateAdopted AccountStateKind = "adopted"
	// StateClaudeSessionDetected: a Claude Code lock artefact is present
	// in the namespace.
	StateClaudeSessionDetected AccountStateKind = "claude_session_detected"
	// StateKeychainLocked: the keychain is locked; the user must unlock
	// it.
	StateKeychainLocked AccountStateKind = "keychain_locked"
	// StateKeychainTimeout: the keychain child did not answer inside its
	// budget.
	StateKeychainTimeout AccountStateKind = "keychain_timeout"
	// StateBusy: another process holds this namespace's lock.
	StateBusy AccountStateKind = "busy"
	// StateLockUnavailable: the namespace lock could not be taken for a
	// reason that is not contention — the fail-closed path.
	StateLockUnavailable AccountStateKind = "lock_unavailable"
	// StateStale: the displayed numbers came from the cache after a live
	// fetch failed.
	StateStale AccountStateKind = "stale"
	// StateNoSubscriptionLimits: the usage response carried no windows at
	// all, which is what an API or console account looks like.
	StateNoSubscriptionLimits AccountStateKind = "no_subscription_limits"
	// StatePendingReplayed: a pending write from an earlier run was moved
	// into place.
	StatePendingReplayed AccountStateKind = "pending_replayed"
	// StatePendingDiscarded: a pending write from an earlier run was
	// discarded.
	StatePendingDiscarded AccountStateKind = "pending_discarded"
	// StateRateLimited: the server asked us to slow down.
	StateRateLimited AccountStateKind = "rate_limited"
	// StateRefreshDiscarded: a refresh completed but the namespace
	// changed before it could be written, so the new credentials were
	// dropped.
	StateRefreshDiscarded AccountStateKind = "refresh_discarded"
	// StateEnvToken: the environment token variable is set, which
	// short-circuits every store.
	StateEnvToken AccountStateKind = "env_token"
	// StateError: anything else, with the reason.
	StateError AccountStateKind = "error"
)

// AccountState is what is going on with one account: the vocabulary the
// whole program shares. Two of its methods carry real weight beyond
// display: [AccountState.IsFailure] drives the process exit status, and
// [AccountState.AllowsNetwork] gates doing any HTTP at all.
//
// Values are built by the State constructors, which pin each kind to its
// payload; the fields beyond Kind are meaningful only for the kinds their
// constructors set them for.
type AccountState struct {
	// Kind is the machine-readable state token.
	Kind AccountStateKind
	// ReadOnly, on an expired row, reports one agentctl may not refresh —
	// the live row, a foreign configuration directory, a migrated
	// namespace. Such a row can only wait for its owner to refresh it.
	ReadOnly bool
	// Source, on a foreign row, is what owns it, as the row's kind also
	// spells it.
	Source string
	// Service, on a migrated row, is the keychain service name found.
	Service string
	// Occupant, on an adopted row, is who holds the item: an email
	// address when the blob named one and an account UUID otherwise.
	// Never a token and never a raw blob.
	Occupant string
	// Lock, on a detected session, is the lock artefact's name.
	Lock string
	// AgeMillis, on a detected session, is how long ago the artefact was
	// touched.
	AgeMillis uint64
	// Detail, on a locked keychain, is what the keychain child said, when
	// it said anything.
	Detail string
	// Reason, on a discarded pending write or an error, says why.
	Reason string
	// RetryAfterSeconds is the rate-limit hint when HasRetryAfter is
	// true, so a hint of zero seconds is distinct from no hint at all.
	RetryAfterSeconds uint64
	// HasRetryAfter reports whether the server sent a parseable hint.
	HasRetryAfter bool
}

// StateOfOK returns the healthy state.
func StateOfOK() AccountState { return AccountState{Kind: StateOK} }

// StateOfExpired returns the expired state; readOnly marks a row only its
// owner may refresh.
func StateOfExpired(readOnly bool) AccountState {
	return AccountState{Kind: StateExpired, ReadOnly: readOnly}
}

// StateOfNeedsLogin returns the no-usable-credential state.
func StateOfNeedsLogin() AccountState { return AccountState{Kind: StateNeedsLogin} }

// StateOfIdentityUnknown returns the anonymous-credential state.
func StateOfIdentityUnknown() AccountState { return AccountState{Kind: StateIdentityUnknown} }

// StateOfStaleSiblingOfLive returns the hidden stale-sibling state.
func StateOfStaleSiblingOfLive() AccountState { return AccountState{Kind: StateStaleSiblingOfLive} }

// StateOfUnclaimed returns the unclaimed-item state.
func StateOfUnclaimed() AccountState { return AccountState{Kind: StateUnclaimed} }

// StateOfForeign returns the foreign-item state, naming what owns it.
func StateOfForeign(source string) AccountState {
	return AccountState{Kind: StateForeign, Source: source}
}

// StateOfForgotten returns the user-hidden state.
func StateOfForgotten() AccountState { return AccountState{Kind: StateForgotten} }

// StateOfMigratedToKeychain returns the migrated state, naming the
// service found.
func StateOfMigratedToKeychain(service string) AccountState {
	return AccountState{Kind: StateMigratedToKeychain, Service: service}
}

// StateOfAdopted returns the adopted state, naming who holds the item.
func StateOfAdopted(occupant string) AccountState {
	return AccountState{Kind: StateAdopted, Occupant: occupant}
}

// StateOfClaudeSessionDetected returns the detected-session state, naming
// the lock artefact and its age.
func StateOfClaudeSessionDetected(lock string, ageMillis uint64) AccountState {
	return AccountState{Kind: StateClaudeSessionDetected, Lock: lock, AgeMillis: ageMillis}
}

// StateOfKeychainLocked returns the locked-keychain state; detail may be
// empty.
func StateOfKeychainLocked(detail string) AccountState {
	return AccountState{Kind: StateKeychainLocked, Detail: detail}
}

// StateOfKeychainTimeout returns the keychain-timeout state.
func StateOfKeychainTimeout() AccountState { return AccountState{Kind: StateKeychainTimeout} }

// StateOfBusy returns the lock-contention state.
func StateOfBusy() AccountState { return AccountState{Kind: StateBusy} }

// StateOfLockUnavailable returns the fail-closed lock state.
func StateOfLockUnavailable() AccountState { return AccountState{Kind: StateLockUnavailable} }

// StateOfStale returns the served-from-cache state.
func StateOfStale() AccountState { return AccountState{Kind: StateStale} }

// StateOfNoSubscriptionLimits returns the windowless-response state.
func StateOfNoSubscriptionLimits() AccountState { return AccountState{Kind: StateNoSubscriptionLimits} }

// StateOfPendingReplayed returns the pending-replayed state.
func StateOfPendingReplayed() AccountState { return AccountState{Kind: StatePendingReplayed} }

// StateOfPendingDiscarded returns the pending-discarded state with its
// reason.
func StateOfPendingDiscarded(reason string) AccountState {
	return AccountState{Kind: StatePendingDiscarded, Reason: reason}
}

// StateOfRateLimited returns the rate-limited state without a hint.
func StateOfRateLimited() AccountState { return AccountState{Kind: StateRateLimited} }

// StateOfRateLimitedAfter returns the rate-limited state with the
// server's hint.
func StateOfRateLimitedAfter(retryAfterSeconds uint64) AccountState {
	return AccountState{Kind: StateRateLimited, RetryAfterSeconds: retryAfterSeconds, HasRetryAfter: true}
}

// StateOfRefreshDiscarded returns the refresh-discarded state.
func StateOfRefreshDiscarded() AccountState { return AccountState{Kind: StateRefreshDiscarded} }

// StateOfEnvToken returns the environment-token state.
func StateOfEnvToken() AccountState { return AccountState{Kind: StateEnvToken} }

// StateOfError returns the catch-all failure state with its reason.
func StateOfError(reason string) AccountState {
	return AccountState{Kind: StateError, Reason: reason}
}

// Label returns the state as it appears in the table's State column: a
// sentence for a person, free to be reworded, unlike the Kind token.
func (s AccountState) Label() string {
	switch s.Kind {
	case StateOK:
		return "ok"
	case StateExpired:
		if s.ReadOnly {
			return "expired (read-only; refreshed by its owner)"
		}
		return "expired"
	case StateNeedsLogin:
		return "needs login"
	case StateIdentityUnknown:
		return "identity unknown"
	case StateStaleSiblingOfLive:
		return "stale sibling of live"
	case StateUnclaimed:
		return "unclaimed"
	case StateForeign:
		return fmt.Sprintf("foreign (%s)", s.Source)
	case StateForgotten:
		return "forgotten"
	case StateMigratedToKeychain:
		return fmt.Sprintf("migrated to keychain (%s)", s.Service)
	case StateAdopted:
		return fmt.Sprintf("adopted (its keychain item is held by another identity: %s)", s.Occupant)
	case StateClaudeSessionDetected:
		return fmt.Sprintf("claude session detected — refresh refused (lock %s, age %ds)", s.Lock, s.AgeMillis/1000)
	case StateKeychainLocked:
		if s.Detail == "" {
			return "keychain locked"
		}
		return fmt.Sprintf("keychain locked (%s)", s.Detail)
	case StateKeychainTimeout:
		return "keychain timeout (transient)"
	case StateBusy:
		return "busy"
	case StateLockUnavailable:
		return "lock unavailable"
	case StateStale:
		return "stale"
	case StateNoSubscriptionLimits:
		return "no subscription limits (API/console account?)"
	case StatePendingReplayed:
		return "pending replayed"
	case StatePendingDiscarded:
		return "pending discarded: " + s.Reason
	case StateRateLimited:
		if s.HasRetryAfter {
			return fmt.Sprintf("rate-limited (retry in %ds)", s.RetryAfterSeconds)
		}
		return "rate-limited"
	case StateRefreshDiscarded:
		return "refresh discarded: namespace changed during refresh"
	case StateEnvToken:
		return "env token"
	case StateError:
		return s.Reason
	default:
		return string(s.Kind)
	}
}

// Name returns the state as its stable, machine-readable token.
func (s AccountState) Name() string {
	return string(s.Kind)
}

// IsFailure reports whether a shown row in this state makes the process
// exit with the partial status.
//
// The informational states are not failures: an unclaimed item is a true
// statement about the machine, not something that went wrong, and a
// hidden row is not shown at all. Everything that means "you asked for
// numbers and did not get them" is.
func (s AccountState) IsFailure() bool {
	switch s.Kind {
	case StateOK, StateStaleSiblingOfLive, StateUnclaimed, StateForeign, StateForgotten,
		StateMigratedToKeychain, StateAdopted, StatePendingReplayed, StateEnvToken:
		return false
	default:
		return true
	}
}

// AllowsNetwork reports whether this row may make an HTTP request.
//
// False means there is no token worth spending a request on, or the
// server has told us to stop. Refreshing is a stricter question again: a
// detected session allows a usage fetch but never a refresh, and that is
// enforced separately in the refresh path.
func (s AccountState) AllowsNetwork() bool {
	switch s.Kind {
	case StateOK, StateMigratedToKeychain, StateAdopted, StateClaudeSessionDetected,
		StatePendingReplayed, StateStale, StateEnvToken:
		return true
	case StateExpired:
		return !s.ReadOnly
	default:
		return false
	}
}

// Source is where a row's credentials came from, as one stable token for
// the JSON report's source member and the accounts listing's Source
// column.
type Source string

const (
	// SourceKeychain is the macOS keychain.
	SourceKeychain Source = "keychain"
	// SourceFile is the store's credential file.
	SourceFile Source = "file"
	// SourceEnv is the environment token variable.
	SourceEnv Source = "env"
	// SourceNone is nowhere: there is no credential.
	SourceNone Source = "none"
)

// Name returns the source's stable token.
func (s Source) Name() string { return string(s) }

// AccountRow is one row of the status table.
type AccountRow struct {
	// ID is the identifier the account flag accepts for this row.
	ID string
	// Record is what the registry knows about it.
	Record config.AccountRecord
	// State is what is going on with it.
	State AccountState
	// Source is where its credentials came from.
	Source Source
	// Credentials are the credentials, when they were readable.
	Credentials *Credentials
	// VisibleByDefault reports whether the row is shown without the
	// show-all flag. Hidden rows are counted in the footer and never
	// affect the exit status.
	VisibleByDefault bool
	// Note is a short explanation shown alongside the state, when there
	// is one.
	Note string
}
