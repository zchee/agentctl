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
	"errors"
	"sync/atomic"

	"github.com/zchee/agentctl/internal/secret"
)

// WriteKind is the non-secret outcome of one namespace mutation.
type WriteKind string

const (
	// WriteRefreshApplied records an applied rotated grant.
	WriteRefreshApplied WriteKind = "refresh_applied"
	// WriteRefreshSavedToPending records a grant preserved for replay.
	WriteRefreshSavedToPending WriteKind = "refresh_saved_to_pending"
	// WriteDiscardedExternal records a newer external grant kept unchanged.
	WriteDiscardedExternal WriteKind = "discarded_external"
	// WritePendingReplayed records a replayed pending credential.
	WritePendingReplayed WriteKind = "pending_replayed"
	// WritePendingDiscarded records a discarded pending credential.
	WritePendingDiscarded WriteKind = "pending_discarded"
	// WriteLoginInstall records an installed verified login.
	WriteLoginInstall WriteKind = "login_install"
	// WriteDelete records deletion of namespace credential files.
	WriteDelete WriteKind = "delete"
)

type receiptState struct {
	kind          WriteKind
	overwrote     bool
	user, account string
	before, after *string
	consumed      atomic.Bool
}

// WriteReceipt is one write's audit obligation; copies share consumption state.
type WriteReceipt struct{ state *receiptState }

func newWriteReceipt(kind WriteKind, user, account string, before, after *secret.Digests) *WriteReceipt {
	return &WriteReceipt{state: &receiptState{kind: kind, user: user, account: account, before: digest8Of(before), after: digest8Of(after)}}
}

func digest8Of(digests *secret.Digests) *string {
	if digests == nil {
		return nil
	}
	value := digests.RefreshSHA256
	if value == "" {
		value = digests.AccessSHA256
	}
	if len(value) < 8 {
		return nil
	}
	return new(value[:8])
}

// Kind returns what the mutation did.
func (r *WriteReceipt) Kind() WriteKind {
	if r == nil || r.state == nil {
		return ""
	}
	return r.state.kind
}

// Overwrote reports whether a login replaced an existing credential.
func (r *WriteReceipt) Overwrote() bool { return r != nil && r.state != nil && r.state.overwrote }

// IDs returns the namespace whose files changed.
func (r *WriteReceipt) IDs() (string, string) {
	if r == nil || r.state == nil {
		return "", ""
	}
	return r.state.user, r.state.account
}

// Digest8Before returns an isolated copy of the previous grant's prefix.
func (r *WriteReceipt) Digest8Before() *string {
	if r == nil || r.state == nil || r.state.before == nil {
		return nil
	}
	return new(*r.state.before)
}

// Digest8After returns an isolated copy of the written grant's prefix.
func (r *WriteReceipt) Digest8After() *string {
	if r == nil || r.state == nil || r.state.after == nil {
		return nil
	}
	return new(*r.state.after)
}

// Consume records the audit attempt before log I/O, rejecting a second attempt.
func (r *WriteReceipt) Consume() error {
	if r == nil || r.state == nil || r.state.consumed.Swap(true) {
		return errors.New("the Codex write receipt is invalid or has already reached the audit log")
	}
	return nil
}

// Consumed reports whether this receipt reached an audit attempt.
func (r *WriteReceipt) Consumed() bool { return r != nil && r.state != nil && r.state.consumed.Load() }

// CodexWriteKind classifies the result before a refresh driver handles its receipt.
type CodexWriteKind string

const (
	// CodexWriteLanded means the grant was installed or parked.
	CodexWriteLanded CodexWriteKind = "landed"
	// CodexWriteChangedSinceRead means a newer grant replaced the read base.
	CodexWriteChangedSinceRead CodexWriteKind = "changed_since_read"
	// CodexWriteTorn means the current credential is mid-write.
	CodexWriteTorn CodexWriteKind = "torn"
)

// CodexWrite carries the outcome and its mandatory audit receipt, if any.
type CodexWrite struct {
	Kind    CodexWriteKind
	Outcome secret.WriteOutcome
	Receipt *WriteReceipt
}
