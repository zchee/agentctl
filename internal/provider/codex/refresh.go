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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
)

const (
	refreshTornRetry     = 50 * time.Millisecond
	refreshWriteAttempts = 3
)

// RunRefresh is the only driver that can send an owned namespace's refresh grant.
// It serializes the POST under the namespace lock and requires a durable marker.
func RunRefresh(ctx context.Context, permit *PostPermit, owned *OwnedRecord, mode SendMode, refresh RefreshContext) RefreshReport {
	report := RefreshReport{Notes: []RefreshNote{}}
	if owned == nil || refresh.Paths == nil || permit.client() == nil {
		report.Step = RefreshStep{Kind: RefreshStepFailed, Reason: "Codex refresh requires a valid owned namespace and POST permit"}
		return report
	}
	if owned.Refresh() != config.RefreshAuto {
		report.Step = RefreshStep{Kind: RefreshStepDisabled}
		return report
	}
	if refresh.LockBudget < 0 || time.Until(refresh.Deadline)-RefreshPostBudget-RefreshWriteAllowance < refresh.LockBudget {
		report.Step = RefreshStep{Kind: RefreshStepStale, Reason: "not_enough_time"}
		return report
	}
	if ctx.Err() != nil {
		report.Step = RefreshStep{Kind: RefreshStepStale, Reason: "cancelled"}
		return report
	}
	guard, err := AcquireCodex(ctx, refresh.Paths, owned, refresh.LockBudget)
	if err != nil {
		switch {
		case errors.Is(err, secret.ErrLockBusy):
			report.Step = RefreshStep{Kind: RefreshStepBusy}
		case ctx.Err() != nil:
			report.Step = RefreshStep{Kind: RefreshStepStale, Reason: "cancelled"}
		default:
			report.Step = RefreshStep{Kind: RefreshStepFailed, Reason: err.Error()}
		}
		return report
	}
	defer func() { _ = guard.Release() }()
	namespace, err := OpenOwnedNamespace(refresh.Paths, owned, guard)
	if err != nil {
		report.Step = RefreshStep{Kind: RefreshStepFailed, Reason: err.Error()}
		return report
	}
	defer func() { _ = namespace.Close() }()
	store, err := NewRefreshStateStore(refresh.Paths, owned.User(), owned.Account())
	if err != nil {
		report.Step = RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
		return report
	}
	driver := refreshDriver{ctx: ctx, refresh: refresh, permit: permit, namespace: namespace, store: store, guard: guard, notes: &report.Notes}
	report.Step = driver.drive(mode)
	return report
}

type refreshDriver struct {
	ctx       context.Context
	refresh   RefreshContext
	permit    *PostPermit
	namespace *OwnedNamespace
	store     *RefreshStateStore
	guard     *Lock
	notes     *[]RefreshNote
}

func (d *refreshDriver) drive(mode SendMode) RefreshStep {
	decision, receipt, _, err := d.namespace.ResolvePending(d.ctx)
	if err != nil {
		return RefreshStep{Kind: RefreshStepFailed, Reason: fmt.Sprintf("a parked credential could not be resolved, so nothing was sent: %v", err)}
	}
	d.auditReceipt(receipt)
	switch decision.Kind {
	case secret.PendingReplayed:
		d.note(RefreshNote{Kind: RefreshNotePendingReplayed})
	case secret.PendingDiscarded:
		d.note(RefreshNote{Kind: RefreshNotePendingDiscarded})
	}
	read, err := d.read()
	if err != nil {
		return RefreshStep{Kind: RefreshStepFailed, Reason: err.Error()}
	}
	if read.Kind == ResolvedAbsent {
		return RefreshStep{Kind: RefreshStepNeedsLogin, Reason: "absent"}
	}
	if read.Kind == ResolvedTorn {
		return d.torn()
	}
	credentials := read.Credentials
	if credentials == nil {
		return RefreshStep{Kind: RefreshStepFailed, Reason: "the owned namespace returned no locked credential"}
	}
	state, err := d.store.loadForUpdate(d.ctx)
	if err != nil {
		return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
	}
	grant, ok := credentials.RefreshDigest8()
	if !ok {
		return RefreshStep{Kind: RefreshStepNeedsLogin, Reason: "no_refresh_token"}
	}
	if state.DeadDigest8 != nil && *state.DeadDigest8 == grant {
		return RefreshStep{Kind: RefreshStepNeedsLogin, Reason: "dead"}
	}
	if mode.Kind == SendAfterUnauthorized {
		rejected, ok := credentials.Credentials().AccessDigest8()
		if !ok || rejected != mode.RejectedAccessDigest8 {
			return RefreshStep{Kind: RefreshStepAdopted, Reason: "external_access"}
		}
	}
	state, unknown, step := d.compareMarker(state, grant)
	if step != nil {
		return *step
	}
	switch mode.Kind {
	case SendProactive:
		if unknown {
			return refreshUnknownStep(state)
		}
		if step := d.evidenceGate(); step != nil {
			return *step
		}
		if !credentials.Credentials().AccessExpired(time.Now(), AccessRefreshMargin) {
			return RefreshStep{Kind: RefreshStepAdopted, Reason: "fresh"}
		}
		if step := refreshServerFloor(state, grant); step != nil {
			return *step
		}
	case SendAfterUnauthorized:
		if unknown {
			return refreshUnknownStep(state)
		}
		if step := d.evidenceGate(); step != nil {
			return *step
		}
		if state.DidNotHelp >= TerminalDidNotHelp {
			return RefreshStep{Kind: RefreshStepUnauthorizedTerminal}
		}
		if until := refreshFloorUntil(&state); until != nil && time.Now().Before(*until) {
			return RefreshStep{Kind: RefreshStepUnauthorizedFloor, Until: *until}
		}
		if step := refreshServerFloor(state, grant); step != nil {
			return *step
		}
	case SendResend:
		if !unknown {
			return RefreshStep{Kind: RefreshStepResendRefused, Reason: "not_unknown"}
		}
		if state.Resent {
			return RefreshStep{Kind: RefreshStepResendRefused, Reason: "already_resent"}
		}
		eligible := ResendEligibleAt(refreshUnknownSince(state), *state.Class, state.RetryAfter)
		if time.Now().Before(eligible) {
			return RefreshStep{Kind: RefreshStepResendRefused, Reason: "too_early", Until: eligible}
		}
		stray, err := d.namespace.HasStrayTmp()
		if err != nil {
			return RefreshStep{Kind: RefreshStepFailed, Reason: err.Error()}
		}
		if stray {
			return RefreshStep{Kind: RefreshStepResendRefused, Reason: "stray_tmp"}
		}
		if step := d.evidenceGate(); step != nil {
			return *step
		}
		if !mode.Consent.consume() {
			return RefreshStep{Kind: RefreshStepResendRefused, Reason: "not_confirmed"}
		}
	default:
		return RefreshStep{Kind: RefreshStepFailed, Reason: "unrecognized Codex refresh mode"}
	}
	return d.send(credentials, grant, mode.Kind == SendResend)
}

func (d *refreshDriver) read() (NamespaceRead, error) {
	read, err := d.namespace.Read()
	if err != nil || read.Kind != ResolvedTorn {
		return read, err
	}
	if err := waitRefresh(d.ctx, refreshTornRetry); err != nil {
		return NamespaceRead{}, err
	}
	return d.namespace.Read()
}

func waitRefresh(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *refreshDriver) torn() RefreshStep {
	read := d.store.Load(d.ctx)
	switch read.Kind {
	case RefreshStateUnavailable:
		return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: read.Reason}
	case RefreshStatePresent:
		if read.State.Inflight != nil {
			return refreshUnknownStep(read.State)
		}
	}
	return RefreshStep{Kind: RefreshStepStale, Reason: "torn"}
}

func (d *refreshDriver) compareMarker(state RefreshState, grant string) (RefreshState, bool, *RefreshStep) {
	if state.Inflight == nil {
		return state, false, nil
	}
	if state.Inflight.SentDigest8 != grant {
		if err := d.store.clearInflight(d.ctx, d.guard); err != nil {
			return state, false, &RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
		}
		d.note(RefreshNote{Kind: RefreshNoteStaleMarkerCleared})
		state, err := d.store.loadForUpdate(d.ctx)
		if err != nil {
			return state, false, &RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
		}
		return state, false, nil
	}
	if state.Class == nil {
		if err := d.store.markInterrupted(d.ctx, d.guard); err != nil {
			return state, false, &RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
		}
		d.auditEvent(CodexEvent{Outcome: AuditAmbiguous, Class: new(RefreshUnknownInterrupted.Label()), Digest8Before: new(grant)})
	}
	refreshed, err := d.store.loadForUpdate(d.ctx)
	if err != nil {
		return state, false, &RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
	}
	return refreshed, true, nil
}

func (d *refreshDriver) evidenceGate() *RefreshStep {
	evidence := d.namespace.DaemonEvidence(d.ctx)
	switch evidence.Kind {
	case DaemonAlive:
		return &RefreshStep{Kind: RefreshStepSessionDetected, PID: evidence.PID}
	case DaemonUnreadable:
		return &RefreshStep{Kind: RefreshStepStale, Reason: "daemon_record_unreadable"}
	case DaemonRecycled, DaemonArtefact:
		d.note(RefreshNote{Kind: RefreshNoteCodexSession, PID: evidence.PID, Evidence: string(evidence.Kind)})
	}
	return nil
}

func (d *refreshDriver) send(credentials *LockedCredentials, grant string, resend bool) RefreshStep {
	active := fault.Active()
	active.PausePoint("codex_before_post_snapshot")
	snapshot, err := d.namespace.SnapshotForPost()
	if err != nil {
		return RefreshStep{Kind: RefreshStepFailed, Reason: err.Error()}
	}
	current, ok := snapshot.RefreshDigest8()
	if !ok || current != grant {
		return d.changedBeforePost()
	}
	if d.ctx.Err() != nil {
		return RefreshStep{Kind: RefreshStepStale, Reason: "cancelled"}
	}
	var token *InflightToken
	if resend {
		token, err = d.store.writeResend(d.ctx, credentials)
	} else {
		token, err = d.store.writeInflight(d.ctx, credentials)
	}
	if err != nil {
		return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
	}
	if resend {
		d.auditEvent(CodexEvent{Outcome: AuditResend, Digest8Before: new(grant)})
	}
	abortRefreshFault("codex_abort_after_marker")
	active.PausePoint("codex_after_post_snapshot")
	outcome := refreshOAuth(d.ctx, credentials, token, d.permit.client())
	// Once sent, the rotated grant and its outcome must survive cancellation.
	d.ctx = context.WithoutCancel(d.ctx)
	return d.settle(credentials, grant, resend, outcome)
}

func (d *refreshDriver) changedBeforePost() RefreshStep {
	current, err := d.read()
	if err != nil {
		return RefreshStep{Kind: RefreshStepFailed, Reason: err.Error()}
	}
	if current.Kind == ResolvedCredentials && !current.Credentials.Credentials().AccessExpired(time.Now(), AccessRefreshMargin) {
		return RefreshStep{Kind: RefreshStepAdopted, Reason: "changed_before_post"}
	}
	if current.Kind == ResolvedAbsent {
		return RefreshStep{Kind: RefreshStepNeedsLogin, Reason: "absent"}
	}
	return RefreshStep{Kind: RefreshStepStale, Reason: "changed_before_post"}
}

func (d *refreshDriver) settle(original *LockedCredentials, grant string, resend bool, outcome RefreshOutcome) RefreshStep {
	switch outcome.Kind {
	case RefreshApplied:
		return d.applied(original, grant, outcome.Response)
	case RefreshPermanent:
		return d.permanent(grant, outcome.Permanent)
	case RefreshPreSend:
		if resend {
			return d.restoreUnknown(false)
		}
		return d.clear(RefreshStep{Kind: RefreshStepStale, Reason: "pre_send", Detail: outcome.Reason})
	case RefreshRejected:
		if resend {
			if err := d.store.restoreUnknown(d.ctx, d.guard, true); err != nil {
				return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
			}
			return RefreshStep{Kind: RefreshStepNeedsLogin, Reason: "resend_rejected", Status: outcome.Status}
		}
		return d.clear(RefreshStep{Kind: RefreshStepStale, Reason: "rejected", Status: outcome.Status})
	case RefreshRateLimited:
		wait := outcome.RetryAfter
		if wait != nil {
			wait = new(min(*wait, MaxRefreshRetryAfter))
		}
		return d.unknown(grant, RefreshUnknownRateLimited, wait)
	case RefreshServerError:
		return d.unknown(grant, RefreshUnknownServerError, nil)
	default:
		class := RefreshUnknownAmbiguous
		if outcome.Ambiguous == AmbiguousTLS {
			class = RefreshUnknownTLS
		}
		if outcome.Ambiguous == AmbiguousWriteFailed {
			class = RefreshUnknownWriteFailed
		}
		return d.unknown(grant, class, nil)
	}
}

func (d *refreshDriver) clear(step RefreshStep) RefreshStep {
	if err := d.store.clearInflight(d.ctx, d.guard); err != nil {
		return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
	}
	return step
}

func (d *refreshDriver) restoreUnknown(resent bool) RefreshStep {
	if err := d.store.restoreUnknown(d.ctx, d.guard, resent); err != nil {
		return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
	}
	state, err := d.store.loadForUpdate(d.ctx)
	if err != nil {
		return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
	}
	if state.Inflight == nil || state.Class == nil {
		return RefreshStep{Kind: RefreshStepStateUnavailable, Reason: "the restored marker is incomplete"}
	}
	return refreshUnknownStep(state)
}

func (d *refreshDriver) unknown(grant string, class RefreshUnknownClass, retryAfter *time.Duration) RefreshStep {
	now := time.Now().UTC()
	if err := d.store.markUnknown(d.ctx, d.guard, class, now, retryAfter); err != nil {
		slog.Warn("a refresh's unknown outcome could not be recorded", "error", err)
	}
	d.auditEvent(CodexEvent{Outcome: AuditAmbiguous, Class: new(class.Label()), Digest8Before: new(grant)})
	read := d.store.Load(d.ctx)
	if read.Kind == RefreshStatePresent {
		state := read.State
		if state.AmbiguousSince == nil {
			state.AmbiguousSince = new(now)
		}
		state.Class = new(class)
		return refreshUnknownStep(state)
	}
	var seconds *uint64
	if retryAfter != nil && *retryAfter >= 0 {
		seconds = new(uint64(*retryAfter / time.Second))
	}
	return RefreshStep{Kind: RefreshStepOutcomeUnknown, Since: now, Class: class, ResendEligibleAt: new(ResendEligibleAt(now, class, seconds))}
}

func (d *refreshDriver) permanent(grant string, class PermanentClass) RefreshStep {
	current, err := d.read()
	if err == nil && current.Kind == ResolvedCredentials {
		kept, ok := current.Credentials.RefreshDigest8()
		if ok && kept != grant {
			step := d.clear(RefreshStep{Kind: RefreshStepRacedExternal})
			d.auditEvent(CodexEvent{Outcome: AuditAdoptedExternal, Digest8Before: new(grant), Digest8After: new(kept)})
			return step
		}
	}
	step := RefreshStep{Kind: RefreshStepNeedsLogin, Reason: "dead"}
	if err := d.store.settleInflight(d.ctx, d.guard, nil, new(grant)); err != nil {
		step = RefreshStep{Kind: RefreshStepStateUnavailable, Reason: err.Error()}
	}
	d.auditEvent(CodexEvent{Outcome: AuditNeedsLogin, Class: new(string(class)), Digest8Before: new(grant)})
	return step
}

func (d *refreshDriver) applied(original *LockedCredentials, grant string, response *RefreshResponse) RefreshStep {
	if response == nil {
		return d.unknown(grant, RefreshUnknownWriteFailed, nil)
	}
	earliest := clampEarliestRefresh(response.EarliestRefreshAt(), time.Now().UTC())
	base := original
	current, err := d.read()
	if err == nil && current.Kind == ResolvedCredentials {
		currentGrant, ok := current.Credentials.RefreshDigest8()
		if ok && currentGrant == grant {
			base = current.Credentials
		}
	}
	merged, merge, err := base.MergeRefresh(response, time.Now().UTC())
	if err != nil {
		slog.Warn("an applied refresh could not be merged", "error", err)
		return d.unknown(grant, RefreshUnknownWriteFailed, nil)
	}
	if merge.IdentityDrift {
		d.note(RefreshNote{Kind: RefreshNoteIdentityDrift})
	}
	if merge.IDTokenUnreadable {
		d.note(RefreshNote{Kind: RefreshNoteIDTokenUnreadable})
	}
	var floor *EarliestRefresh
	if digest, ok := merged.RefreshDigest8(); earliest != nil && ok {
		floor = &EarliestRefresh{GrantDigest8: digest, At: *earliest}
	}
	written := merged.Credentials().Digests()
	for range refreshWriteAttempts {
		result, err := d.namespace.Write(d.ctx, merged)
		if err == nil {
			switch result.Kind {
			case CodexWriteLanded:
				d.auditReceipt(result.Receipt)
				return d.landed(result.Outcome.SavedToPending, floor)
			case CodexWriteChangedSinceRead:
				d.auditReceipt(result.Receipt)
				return d.clear(RefreshStep{Kind: RefreshStepDiscardedExternal})
			case CodexWriteTorn:
				_ = waitRefresh(d.ctx, refreshTornRetry)
			}
		} else {
			slog.Warn("writing an applied refresh failed; retrying", "error", err)
		}
		read, readErr := d.namespace.Read()
		if readErr == nil && read.Kind == ResolvedCredentials && equalRefreshDigests(written, read.Credentials.Credentials().Digests()) {
			digest, ok := read.Credentials.RefreshDigest8()
			var after *string
			if ok {
				after = new(digest)
			}
			d.auditEvent(CodexEvent{Outcome: AuditApplied, Digest8Before: new(grant), Digest8After: after})
			return d.landed(false, floor)
		}
	}
	parked, err := d.namespace.Park(d.ctx, merged)
	if err == nil && parked.Kind == CodexWriteLanded {
		d.auditReceipt(parked.Receipt)
		return d.landed(true, floor)
	}
	if parked.Receipt != nil {
		d.auditReceipt(parked.Receipt)
	}
	if err != nil {
		slog.Warn("an applied refresh could not be written or parked", "error", err)
	}
	return d.unknown(grant, RefreshUnknownWriteFailed, nil)
}

func equalRefreshDigests(a, b *secret.Digests) bool { return a != nil && b != nil && *a == *b }

func (d *refreshDriver) landed(parked bool, earliest *EarliestRefresh) RefreshStep {
	if parked {
		abortRefreshFault("codex_abort_after_pending")
	}
	if err := d.store.settleInflight(d.ctx, d.guard, earliest, nil); err != nil {
		slog.Warn("the refresh marker could not be cleared after a landed write", "error", err)
	}
	evidence := d.namespace.DaemonEvidence(d.ctx)
	if evidence.Kind != DaemonNone {
		d.note(RefreshNote{Kind: RefreshNoteCodexSession, PID: evidence.PID, Evidence: string(evidence.Kind)})
	}
	return RefreshStep{Kind: RefreshStepRefreshed, Parked: parked}
}

func (d *refreshDriver) note(note RefreshNote) { *d.notes = append(*d.notes, note) }
func (d *refreshDriver) audited(err error) {
	if err != nil {
		slog.Warn("the Codex audit log refused an entry for a write that landed", "error", err)
		d.note(RefreshNote{Kind: RefreshNoteAuditLogRefused})
	}
}

func (d *refreshDriver) auditReceipt(receipt *WriteReceipt) {
	if receipt != nil {
		d.audited(AppendCodexReceipt(d.ctx, d.refresh.Paths, receipt))
	}
}

func (d *refreshDriver) auditEvent(event CodexEvent) {
	d.audited(AppendCodexEvent(d.ctx, d.refresh.Paths, d.store.user, d.store.account, event))
}

func refreshUnknownSince(state RefreshState) time.Time {
	if state.AmbiguousSince != nil {
		return *state.AmbiguousSince
	}
	if state.Inflight != nil {
		return state.Inflight.SentAt
	}
	return time.Time{}
}

func refreshUnknownStep(state RefreshState) RefreshStep {
	class := RefreshUnknownInterrupted
	if state.Class != nil {
		class = *state.Class
	}
	since := refreshUnknownSince(state)
	step := RefreshStep{Kind: RefreshStepOutcomeUnknown, Since: since, Class: class}
	if !state.Resent {
		step.ResendEligibleAt = new(ResendEligibleAt(since, class, state.RetryAfter))
	}
	return step
}

func refreshServerFloor(state RefreshState, grant string) *RefreshStep {
	earliest := state.EarliestRefresh
	if earliest != nil && earliest.GrantDigest8 == grant && time.Now().Before(earliest.At) {
		return &RefreshStep{Kind: RefreshStepNotBefore, Until: earliest.At}
	}
	return nil
}

// RecordRetryGet records the usage retry after a sent unauthorized-triggered refresh.
// It sends nothing and requires the same POST capability as the initiating refresh.
func RecordRetryGet(ctx context.Context, permit *PostPermit, owned *OwnedRecord, result RetryGetResult, refresh RefreshContext) error {
	if permit.client() == nil || owned == nil || refresh.Paths == nil {
		return errs.NewConfig("Codex retry recording requires a POST permit and owned namespace")
	}
	guard, err := AcquireCodex(ctx, refresh.Paths, owned, refresh.LockBudget)
	if err != nil {
		return err
	}
	defer func() { _ = guard.Release() }()
	store, err := NewRefreshStateStore(refresh.Paths, owned.User(), owned.Account())
	if err != nil {
		return err
	}
	switch result {
	case RetryGetSucceeded:
		return store.resetFloor(ctx, guard)
	case RetryGetUnauthorized:
		return store.recordDidNotHelp(ctx, guard)
	default:
		return errs.NewConfig("unrecognized Codex retry GET result")
	}
}

// ResetRefreshFloor consumes the user's terminal confirmation without sending a token.
// A refused audit entry is logged but never undoes the durable policy reset.
func ResetRefreshFloor(ctx context.Context, paths *config.Paths, namespace *OwnedNamespace, consent *ResetConsent) error {
	if namespace == nil || !consent.consume() {
		return errs.NewConfig("Codex floor reset requires one confirmed consent")
	}
	user, account := namespace.IDs()
	store, err := NewRefreshStateStore(paths, user, account)
	if err != nil {
		return err
	}
	if err := store.resetFloor(ctx, namespace.Lock()); err != nil {
		return err
	}
	if err := AppendCodexEvent(ctx, paths, user, account, CodexEvent{Outcome: AuditFloorReset}); err != nil {
		slog.Warn("the Codex audit log refused a floor reset entry", "error", err)
	}
	return nil
}
