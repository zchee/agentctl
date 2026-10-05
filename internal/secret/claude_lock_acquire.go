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
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/proc"
)

// The exit codes the lock protocol's refusals map onto. The command
// layer's exit table names the same numbers; they are spelled here too
// because this package sits below it and a refusal must carry its code
// wherever it is built.
const (
	// refusedExitCode is the compromised-hold refusal and every other
	// acquisition failure that is not an unreachable store.
	refusedExitCode = 10
	// busyExitCode means the peer's locks are held and were not broken.
	busyExitCode = 16
	// discardedExitCode means a credential had to be thrown away under
	// the hold — here, because the write window had already closed.
	discardedExitCode = 17
)

// WriteWindowClosedError reports that too little of the hold budget
// remains for a write to start: a write takes up to the configuration
// hold budget, and starting one that cannot finish inside the hold
// budget would hand the peer a lock past its own give-up.
type WriteWindowClosedError struct {
	// Elapsed is how long the hold had lasted.
	Elapsed time.Duration
	// Budget is the hold budget the write had to fit inside.
	Budget time.Duration
}

// Error implements the error interface.
func (e *WriteWindowClosedError) Error() string {
	return fmt.Sprintf("the hold is %d ms old and a write takes up to %d ms more, past the %d ms budget",
		e.Elapsed.Milliseconds(), ConfigHoldBudget.Milliseconds(), e.Budget.Milliseconds())
}

// AcquireError is an acquisition failure, carrying the one break it may
// already have performed.
//
// The break rule removes a peer's lock directory at its third sample
// and hands back a draft record; every way out of the acquisition after
// that moment owes that draft to the audit log. The draft is carried
// rather than dropped so no failure path can remove another process's
// lock with nothing durable to say so.
type AcquireError struct {
	// Err is why the acquisition failed.
	Err error
	// BreakRecord is the single break this acquisition completed before
	// failing, if any. The caller owes it to the audit log before it
	// maps the error to anything.
	BreakRecord *BreakDraft
}

// Error implements the error interface.
func (e *AcquireError) Error() string {
	return e.Err.Error()
}

// Unwrap exposes the underlying failure for errors.Is and errors.As.
func (e *AcquireError) Unwrap() error {
	return e.Err
}

// Acquisition is what an acquisition came back with: the hold, or a
// busy answer, and the one break it may have performed either way.
//
// The break record travels back to the caller instead of being appended
// here, so that no code between the rule's last sample and its removal
// can grow an I/O call; the caller appends it through the audit log.
type Acquisition struct {
	// Held is the live hold, or nil when the store is busy.
	Held *HeldLocks
	// HolderAlive says whether the primary lock's modification time
	// moved during the contention schedule, for a busy answer.
	HolderAlive bool
	// StoppedPIDs is the stopped `claude` process ids, for the terminal
	// message only — never recorded.
	StoppedPIDs []int32
	// BreakRecord is the single break this acquisition attempted, if
	// any.
	BreakRecord *BreakDraft
}

// Busy reports that the locks were not taken.
func (a *Acquisition) Busy() bool {
	return a.Held == nil
}

// heldOne is one held directory and the modification time it had when
// it was created. An unreadable reading never reaches a finished hold:
// the only entry carrying one is pushed on the way out of a failed
// take, purely so the directory it names is released with the rest.
type heldOne struct {
	artefact lockArtefact
	mtime    time.Time
	readable bool
}

// HeldLocks is a live hold of the three credential-store locks.
//
// Released in reverse order by [HeldLocks.Release], and — when a
// cleanup registry was injected — by the emergency path a signal exit
// runs, with the same reverse-order removals through the same
// descriptors.
type HeldLocks struct {
	anchor     *LockAnchor
	held       []heldOne
	recordPath string
	firstMkdir time.Duration
	holdBudget time.Duration
	clock      Clock
	fs         LockFS
	unregister func() bool
	leak       bool
	released   bool
}

// StoreDir returns the store directory this hold is about.
func (h *HeldLocks) StoreDir() string {
	return h.anchor.StoreDir()
}

// Tree returns which tree the hold is in.
func (h *HeldLocks) Tree() Tree {
	return h.anchor.Tree()
}

// RecordPath returns the held-lock record's path, so diagnostics and
// tests can name it.
func (h *HeldLocks) RecordPath() string {
	return h.recordPath
}

// Paths returns the directories held, in acquisition order.
func (h *HeldLocks) Paths() []string {
	paths := make([]string, 0, len(h.held))
	for _, one := range h.held {
		paths = append(paths, one.artefact.path)
	}
	return paths
}

// Slot returns the slot for one held artefact by its recorded path, so
// a caller can sample a lock it holds; false when the hold does not
// carry that path.
func (h *HeldLocks) Slot(path string) (LockSlot, bool) {
	for i := range h.held {
		if h.held[i].artefact.path == path {
			return h.anchor.slot(&h.held[i].artefact), true
		}
	}
	return LockSlot{}, false
}

// HoldElapsed returns how long the hold has lasted, from the first
// mkdir, measured on the same clock that stamped it — mixing an
// injected clock with the real one would make this the one number a
// test could not check.
func (h *HeldLocks) HoldElapsed() time.Duration {
	return max(h.clock.Monotonic()-h.firstMkdir, 0)
}

// DriftCheck is the last check before a write: one stat of each held
// directory, immediately before the caller commits.
//
// Any modification time differing from the value recorded at its mkdir
// means a third party has touched a lock this process holds — the
// protocol has been violated and nothing may be written. An unreadable
// modification time on either side is the same refusal, not an
// equality: an unreadable reading is not evidence that nothing moved.
// The budget is checked in the same place, because this is the last
// moment at which abandoning still costs nothing.
func (h *HeldLocks) DriftCheck() error {
	for i := range h.held {
		one := &h.held[i]
		now, readable := h.fs.Mtime(h.anchor.slot(&one.artefact))
		if !one.readable || !readable || !now.Equal(one.mtime) {
			return &CompromisedError{Path: one.artefact.path}
		}
	}
	elapsed := h.HoldElapsed()
	if elapsed > h.holdBudget {
		return &BudgetExceededError{Elapsed: elapsed, Budget: h.holdBudget}
	}
	return nil
}

// WriteAdmission is the decision a writer asks before starting: a
// write may begin only while what remains of the hold budget still
// covers a whole configuration-lock hold, so a write that starts can
// also finish inside the budget.
func (h *HeldLocks) WriteAdmission() error {
	elapsed := h.HoldElapsed()
	if elapsed+ConfigHoldBudget > h.holdBudget {
		return &WriteWindowClosedError{Elapsed: elapsed, Budget: h.holdBudget}
	}
	return nil
}

// Release gives everything back in reverse order and clears the
// record. Safe to call twice. A hold acquired under the injected leak
// fault releases nothing, leaving the directories and the record
// exactly as a crashed hold would.
func (h *HeldLocks) Release() {
	if h.released {
		return
	}
	h.released = true
	if h.leak {
		return
	}
	// Once emergency cleanup takes the registration, it also owns the
	// descriptors. Removing again could delete locks a peer has since taken.
	if h.unregister != nil && !h.unregister() {
		return
	}
	for i := len(h.held) - 1; i >= 0; i-- {
		_ = h.fs.Rmdir(h.anchor.slot(&h.held[i].artefact))
	}
	_ = os.Remove(h.recordPath)
	h.anchor.Close()
}

// AcquirePeerLocks takes all three credential-store locks, or reports the store
// busy.
//
// In order, and each wait with nothing held:
//
//  1. With nothing held, stat all three; for any that exists and is
//     stale by its own profile, run the full break rule — including
//     its sampling wait. At most one break per acquisition, so the
//     protocol cannot loop against a peer that recreates a lock.
//  2. With nothing held, wait out a primary that is present and not
//     stale on the peer's own contention schedule. If it is released
//     during the schedule the acquisition proceeds; if it is still
//     there, the store is busy.
//  3. Write the held-lock record before the first mkdir.
//  4. Mkdir the three in the peer's nesting. A holder found at any
//     position releases everything already taken, clears the record,
//     and restarts the stale checks — never a wait or a sampling window
//     while holding anything. At most [MaxRestarts] restarts.
//
// Every failure is an [*AcquireError] whose break record, if any, the
// caller owes to the audit log before mapping the error to anything.
func AcquirePeerLocks(ctx context.Context, subject LockSubject, paths *config.Paths, live *LiveStoreEnv, seams *Seams) (*Acquisition, error) {
	anchor, err := OpenLockAnchor(subject, paths, live)
	if err != nil {
		// Decided before the break rule could have run at all, so there
		// is no draft in existence to lose.
		return nil, &AcquireError{Err: err}
	}
	return AcquirePeerLocksWith(ctx, anchor, paths, seams)
}

// AcquirePeerLocksWith is [AcquirePeerLocks] over an already-opened anchor. It owns the
// anchor: every return path either hands it to the hold or gives its
// descriptors back.
func AcquirePeerLocksWith(ctx context.Context, anchor *LockAnchor, paths *config.Paths, seams *Seams) (*Acquisition, error) {
	plans := peerLockPlan(anchor)
	subject := LockSubject{StoreDir: anchor.StoreDir(), Tree: anchor.Tree()}
	var breakRecord *BreakDraft
	var broken *lockArtefact
	restarts := 0

	busy := func(holderAlive bool, stoppedPIDs []int32) (*Acquisition, error) {
		anchor.Close()
		return &Acquisition{HolderAlive: holderAlive, StoppedPIDs: stoppedPIDs, BreakRecord: breakRecord}, nil
	}
	fail := func(err error) (*Acquisition, error) {
		anchor.Close()
		return nil, &AcquireError{Err: err, BreakRecord: breakRecord}
	}

	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}

		// Resolve staleness with nothing held.
		contend := false
		for i := range plans {
			lock := &plans[i]
			at := anchor.slot(&lock.artefact)
			mtime, present := seams.FS.Mtime(at)
			if !present {
				continue
			}
			if elapsedSince(seams.Clock.Wall(), mtime) < lock.profile.Stale && !seams.fault(FaultLockStale) {
				continue
			}
			if breakRecord != nil {
				// One break per acquisition. A second stale lock is
				// reported busy rather than broken.
				return busy(false, nil)
			}

			outcome := ResolveStale(ctx, subject, at, &lock.profile, seams)
			breakRecord = outcome.Record

			switch outcome.Decision {
			case DecisionBroken:
				artefact := lock.artefact
				broken = &artefact
			case DecisionAbandoned:
				switch outcome.Reason {
				case ReasonVanished:
					// Gone on its own: carry on to the next artefact.
				case ReasonHolderUnreadable:
					return fail(&HolderUnreadableError{})
				case ReasonHolderStopped:
					return busy(false, seams.Holders.StoppedPIDs(ctx))
				default:
					// Something is beating, or the clocks disagree.
					// Fall through to the contention schedule, which is
					// what decides whether the holder is alive.
					contend = true
				}
			case DecisionCancelled:
				return fail(context.Canceled)
			case DecisionFailed:
				return fail(&LockIOError{
					Context: fmt.Sprintf("could not resolve `%s`", lock.artefact.path),
					Message: outcome.FailureMessage,
				})
			}
			if contend {
				break
			}
		}

		// Wait out a live primary with nothing held.
		primary := &plans[0]

		// A lock that was broken and is already back is a retake, and it
		// ends the acquisition: at most one break, so there is no second
		// one to attempt, and waiting out the new holder would be
		// waiting out a lock this process itself just freed.
		if broken != nil {
			if _, present := seams.FS.Mtime(anchor.slot(broken)); present {
				if breakRecord != nil {
					breakRecord.Reason = ReasonRetaken
				}
				return busy(true, nil)
			}
		}

		if first, present := seams.FS.Mtime(anchor.slot(&primary.artefact)); present {
			switch held, holderAlive := contentionWait(ctx, anchor.slot(&primary.artefact), first, seams); held {
			case contentionReleased:
				// Freed during the schedule: the acquisition proceeds.
			case contentionStillHeld:
				return busy(holderAlive, nil)
			case contentionCancelled:
				return fail(context.Canceled)
			}
		}

		// Write the record before the first mkdir.
		//
		// A failure here carries the draft out: a break may already have
		// happened this iteration, and discarding its only durable
		// evidence on demand is exactly what the carried record exists
		// to prevent.
		record := &HeldLockRecord{
			WriterPID:       uint32(os.Getpid()),
			WriterStartTime: selfStartIdentity(ctx),
			Tree:            anchor.Tree(),
			StoreDir:        anchor.StoreDir(),
			Paths:           planPaths(&plans),
			TakenAt:         rfc3339UTC(seams.Clock.Wall()),
		}
		recordPath, err := writeHeldLockRecord(ctx, paths, record, seams.Clock)
		if err != nil {
			return fail(err)
		}

		// Take the three locks in the peer's nesting.
		//
		// The hold is timed from here, before the first mkdir: the
		// peer's give-up floor starts running the moment the primary
		// exists, so measuring from the end would understate exactly
		// the term the budget is derived from.
		firstMkdir := seams.Clock.Monotonic()
		held, takeFailure := takeAll(anchor, &plans, seams)
		if takeFailure == nil {
			hold := &HeldLocks{
				anchor:     anchor,
				held:       held,
				recordPath: recordPath,
				firstMkdir: firstMkdir,
				holdBudget: primary.profile.HoldBudget,
				clock:      seams.Clock,
				fs:         seams.FS,
				leak:       seams.fault(FaultSwapLockLeak),
			}
			registerEmergencyRelease(hold, seams)
			return &Acquisition{Held: hold, BreakRecord: breakRecord}, nil
		}

		releaseAll(anchor, held, seams)
		_ = os.Remove(recordPath)

		if errors.Is(takeFailure.err, ErrLockExists) {
			// A lock recreated between this process's own removal and
			// its own mkdir is a retake, and it ends the acquisition: at
			// most one break, so there is no second one to attempt.
			if broken != nil && *broken == takeFailure.artefact {
				if breakRecord != nil {
					breakRecord.Reason = ReasonRetaken
				}
				return busy(true, nil)
			}
			restarts++
			if restarts > MaxRestarts {
				return busy(false, nil)
			}
			continue
		}
		return fail(&LockIOError{
			// "take", not "create": this arm is also reached by a
			// directory that was created and could not then be stated,
			// and "could not create" would contradict its own message.
			Context: fmt.Sprintf("could not take `%s`", takeFailure.artefact.path),
			Message: takeFailure.message,
		})
	}
}

// planPaths lists the plan's artefact paths in acquisition order.
func planPaths(plans *[3]lockPlan) []string {
	paths := make([]string, 0, len(plans))
	for i := range plans {
		paths = append(paths, plans[i].artefact.path)
	}
	return paths
}

// takeFailure is why the three mkdirs did not all succeed: the artefact
// that stopped them, and — wrapped in err — whether somebody holds it
// or it could not be taken at all.
type takeFailure struct {
	artefact lockArtefact
	err      error
	message  string
}

// takeAll mkdirs the three in order, recording each one's modification
// time. Every attempt is a single non-blocking mkdir — including the
// storage-write mutex, whose own retry ladder must never enter the
// hold.
//
// A directory whose modification time cannot be read immediately after
// its own mkdir is a failure, not a hold: the drift check is the only
// thing that tells this process a third party touched a lock it holds,
// and without a baseline there is nothing to compare against, so the
// lock would be held with that check switched off. The directory
// exists by then, so it joins the held list on the way out and is
// released in reverse order with everything before it.
func takeAll(anchor *LockAnchor, plans *[3]lockPlan, seams *Seams) ([]heldOne, *takeFailure) {
	held := make([]heldOne, 0, len(plans))
	for position := range plans {
		lock := &plans[position]
		at := anchor.slot(&lock.artefact)
		var err error
		if position == 0 && seams.fault(FaultLockContended) {
			err = ErrLockExists
		} else {
			err = seams.FS.Mkdir(at)
		}
		switch {
		case err == nil:
			mtime, readable := seams.FS.Mtime(at)
			if !readable {
				held = append(held, heldOne{artefact: lock.artefact})
				return held, &takeFailure{
					artefact: lock.artefact,
					err:      ErrLockGone,
					message: "it was created, but its modification time could not be read, " +
						"so a third party touching it could not be detected",
				}
			}
			held = append(held, heldOne{artefact: lock.artefact, mtime: mtime, readable: true})
		case errors.Is(err, ErrLockExists):
			return held, &takeFailure{artefact: lock.artefact, err: ErrLockExists, message: "somebody else holds it"}
		case errors.Is(err, ErrLockGone):
			return held, &takeFailure{artefact: lock.artefact, err: ErrLockGone, message: "the parent directory is not there"}
		default:
			return held, &takeFailure{artefact: lock.artefact, err: err, message: err.Error()}
		}
	}
	return held, nil
}

// releaseAll gives back what was taken, in reverse order.
func releaseAll(anchor *LockAnchor, held []heldOne, seams *Seams) {
	for i := len(held) - 1; i >= 0; i-- {
		_ = seams.FS.Rmdir(anchor.slot(&held[i].artefact))
	}
}

// registerEmergencyRelease registers the reverse-order release with the
// injected cleanup registry, when one is wired: the release lands in
// the directories the mkdirs landed in, because an emergency path
// re-resolving a path is an emergency path that can be redirected.
func registerEmergencyRelease(hold *HeldLocks, seams *Seams) {
	if seams.Cleanup == nil {
		return
	}
	anchor, fs := hold.anchor, hold.fs
	artefacts := make([]lockArtefact, 0, len(hold.held))
	for _, v := range slices.Backward(hold.held) {
		artefacts = append(artefacts, v.artefact)
	}
	record := hold.recordPath
	hold.unregister = seams.Cleanup.Register(func() {
		for i := range artefacts {
			_ = fs.Rmdir(anchor.slot(&artefacts[i]))
		}
		_ = os.Remove(record)
		anchor.Close()
	})
}

// contention is what the schedule concluded about a primary lock
// somebody holds.
type contention int

const (
	// contentionReleased means it went away; the acquisition may
	// proceed.
	contentionReleased contention = iota
	// contentionStillHeld means it is still there.
	contentionStillHeld
	// contentionCancelled means the wait was cancelled.
	contentionCancelled
)

// contentionWait waits on the peer's own contention schedule, holding
// nothing: five rounds of one second plus up to a second of jitter, a
// second sample, and — if less than the floor has passed — a top-up to
// the floor and one more sample. The holder-alive answer is "the
// modification time moved", which is exactly how a live session
// decides the same question. A lock that disappears at any sample ends
// the wait immediately: that is a live session releasing the lock, and
// the acquisition should carry on rather than report a refusal.
func contentionWait(ctx context.Context, at LockSlot, first time.Time, seams *Seams) (contention, bool) {
	var waited time.Duration
	for range ContentionRounds {
		nap := ContentionRoundBase + seams.Clock.Jitter(ContentionRoundJitter)
		if err := seams.Clock.Sleep(ctx, nap); err != nil {
			return contentionCancelled, false
		}
		waited += nap
		if _, present := seams.FS.Mtime(at); !present {
			return contentionReleased, false
		}
	}

	second, present := seams.FS.Mtime(at)
	if !present {
		return contentionReleased, false
	}
	if !second.Equal(first) {
		return contentionStillHeld, true
	}
	if waited >= ContentionFloor {
		return contentionStillHeld, false
	}

	if err := seams.Clock.Sleep(ctx, ContentionFloor-waited); err != nil {
		return contentionCancelled, false
	}
	third, present := seams.FS.Mtime(at)
	if !present {
		return contentionReleased, false
	}
	return contentionStillHeld, !third.Equal(first)
}

// selfStartIdentity returns this process's start identity, or nil when
// it cannot be read — recorded as unknown rather than invented, because
// a reader treats only a readable mismatch as evidence of a recycled
// process id.
func selfStartIdentity(ctx context.Context) *string {
	current, err := proc.Lookup(ctx, os.Getpid())
	if err != nil {
		return nil
	}
	identity := current.StartIdentity()
	if identity == "" {
		return nil
	}
	return &identity
}

// BusyRefusal maps a busy store onto the exit vocabulary: the locks
// are held and were not broken.
func BusyRefusal(holderAlive bool, stoppedPIDs []int32) error {
	reason := "the store's Claude Code locks are held by another process"
	if holderAlive {
		reason = "a Claude Code session is actively holding the store's locks"
	}
	if len(stoppedPIDs) > 0 {
		reason = fmt.Sprintf("%s; stopped claude process(es) %v may or may not be the holder", reason, stoppedPIDs)
	}
	return errs.NewRefused(busyExitCode, reason)
}

// LockRefusal maps a lock failure onto the exit vocabulary.
//
// A compromised hold — drifted or unreadable held modification time, or
// a hold past its budget — is the lettered refusal; an unreachable
// store is fatal configuration; a closed write window discards; a
// cancellation passes through for the signal exit; and every other
// acquisition failure carries the refusal code with its own words.
func LockRefusal(err error) error {
	var (
		compromised *CompromisedError
		budget      *BudgetExceededError
		window      *WriteWindowClosedError
		unreachable *UnreachableError
	)
	switch {
	case err == nil:
		return nil
	case errors.As(err, &compromised), errors.As(err, &budget):
		return errs.NewRefusedLetter(refusedExitCode, "A")
	case errors.As(err, &window):
		return errs.NewRefused(discardedExitCode, err.Error())
	case errors.As(err, &unreachable):
		return errs.NewConfig(err.Error())
	case errors.Is(err, context.Canceled):
		return err
	default:
		return errs.NewRefused(refusedExitCode, err.Error())
	}
}
