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
	"time"

	"github.com/zchee/agentctl/internal/config"
)

// SendModeKind identifies which set of refresh gates applies.
type SendModeKind string

const (
	// SendProactive refreshes an expired or soon-expiring access token.
	SendProactive SendModeKind = "proactive"
	// SendAfterUnauthorized refreshes the access token rejected by usage.
	SendAfterUnauthorized SendModeKind = "after_unauthorized"
	// SendResend is the user's confirmed one-shot resend of an unknown grant.
	SendResend SendModeKind = "resend"
)

// SendMode contains the triggering facts for one attempt.
// A resend is invalid without an unconsumed, confirmed consent.
type SendMode struct {
	Kind                  SendModeKind
	RejectedAccessDigest8 string
	Consent               *ResendConsent
}

// RefreshContext names the store and reserves a caller deadline and namespace lock wait.
type RefreshContext struct {
	Paths      *config.Paths
	Deadline   time.Time
	LockBudget time.Duration
}

// RefreshStepKind is the terminal state of a refresh attempt.
type RefreshStepKind string

const (
	// RefreshStepStale leaves the grant untouched for a later pass.
	RefreshStepStale RefreshStepKind = "stale"
	// RefreshStepBusy means another process holds the namespace lock.
	RefreshStepBusy RefreshStepKind = "busy"
	// RefreshStepAdopted means no POST was needed.
	RefreshStepAdopted RefreshStepKind = "adopted"
	// RefreshStepRefreshed means a rotated grant was written or parked.
	RefreshStepRefreshed RefreshStepKind = "refreshed"
	// RefreshStepNeedsLogin means the grant cannot be used again.
	RefreshStepNeedsLogin RefreshStepKind = "needs_login"
	// RefreshStepRacedExternal keeps a newer grant after a permanent answer.
	RefreshStepRacedExternal RefreshStepKind = "raced_external"
	// RefreshStepDiscardedExternal discards an applied response for an old grant.
	RefreshStepDiscardedExternal RefreshStepKind = "discarded_external"
	// RefreshStepOutcomeUnknown keeps the marker and forbids automatic resend.
	RefreshStepOutcomeUnknown RefreshStepKind = "outcome_unknown"
	// RefreshStepStateUnavailable means no marker could safely authorize a send.
	RefreshStepStateUnavailable RefreshStepKind = "state_unavailable"
	// RefreshStepSessionDetected means a live daemon owns the namespace.
	RefreshStepSessionDetected RefreshStepKind = "session_detected"
	// RefreshStepUnauthorizedFloor means a rejected access token is inside its floor.
	RefreshStepUnauthorizedFloor RefreshStepKind = "unauthorized_floor"
	// RefreshStepUnauthorizedTerminal stops after three refreshes did not help.
	RefreshStepUnauthorizedTerminal RefreshStepKind = "unauthorized_terminal"
	// RefreshStepNotBefore honors a token host's grant-specific floor.
	RefreshStepNotBefore RefreshStepKind = "not_before"
	// RefreshStepDisabled means the account policy forbids refreshing.
	RefreshStepDisabled RefreshStepKind = "disabled"
	// RefreshStepResendRefused means one of the resend gates failed.
	RefreshStepResendRefused RefreshStepKind = "resend_refused"
	// RefreshStepFailed means a prerequisite failed before a send.
	RefreshStepFailed RefreshStepKind = "failed"
)

// RefreshStep contains non-secret facts about the terminal state.
// Reason is a fixed reason word except on failed and state-unavailable steps,
// which carry sanitized file-operation explanations. Until belongs to floors
// and too-early resends; Since and Class belong to unknown outcomes.
type RefreshStep struct {
	Kind             RefreshStepKind
	Reason           string
	Detail           string
	Status           int
	Parked           bool
	PID              uint32
	Until            time.Time
	Since            time.Time
	Class            RefreshUnknownClass
	ResendEligibleAt *time.Time
}

// RefreshNoteKind identifies a note that does not undo a landed write.
type RefreshNoteKind string

const (
	// RefreshNoteAuditLogRefused leaves the write standing after an audit refusal.
	RefreshNoteAuditLogRefused RefreshNoteKind = "audit_log_refused"
	// RefreshNoteCodexSession reports daemon evidence that did not block the send.
	RefreshNoteCodexSession RefreshNoteKind = "codex_session"
	// RefreshNoteIdentityDrift means the received id token names another account.
	RefreshNoteIdentityDrift RefreshNoteKind = "identity_drift"
	// RefreshNoteIDTokenUnreadable means the stored id token was retained.
	RefreshNoteIDTokenUnreadable RefreshNoteKind = "id_token_unreadable"
	// RefreshNotePendingReplayed means the parked credential was replayed first.
	RefreshNotePendingReplayed RefreshNoteKind = "pending_replayed"
	// RefreshNotePendingDiscarded means the parked credential was discarded first.
	RefreshNotePendingDiscarded RefreshNoteKind = "pending_discarded"
	// RefreshNoteStaleMarkerCleared means a marker for another grant was cleared.
	RefreshNoteStaleMarkerCleared RefreshNoteKind = "stale_marker_cleared"
)

// RefreshNote carries a fixed note and optional daemon facts.
type RefreshNote struct {
	Kind     RefreshNoteKind
	PID      uint32
	Evidence string
}

// RefreshReport records where one attempt ended and any associated notes.
type RefreshReport struct {
	Step  RefreshStep
	Notes []RefreshNote
}

// RetryGetResult records what a usage retry answered after a sent refresh.
type RetryGetResult uint8

const (
	// RetryGetSucceeded lifts the 401 floor and count.
	RetryGetSucceeded RetryGetResult = iota
	// RetryGetUnauthorized doubles the floor and increments the count.
	RetryGetUnauthorized
)
