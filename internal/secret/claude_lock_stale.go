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
	"log/slog"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/runtime/proc"
)

// ClaudeProcessName is the exact kernel command name the holder check
// sweeps for.
const ClaudeProcessName = "claude"

// The injected fault names the testing build's fault seam may carry.
// Production code never reads an environment variable here: the hook is
// injected through [Seams].
const (
	// FaultLockStale makes a fresh lock eligible for the break rule.
	FaultLockStale = fault.LockStale
	// FaultLockContended makes the primary lock's first mkdir report a
	// holder.
	FaultLockContended = fault.LockContended
	// FaultLockResumeAfterSampleB touches the sampled lock in exactly
	// the window the third sample exists to close.
	FaultLockResumeAfterSampleB = fault.LockResumeAfterSampleB
	// FaultSwapLockLeak makes a release leave the directories and the
	// record behind, the way a crashed hold would.
	FaultSwapLockLeak = fault.SwapLockLeak
)

// HolderSightings is whether any same-user `claude` process is stopped.
type HolderSightings interface {
	// StoppedClaudePresent answers the one question the break rule
	// asks, at the moment the rule asks it.
	StoppedClaudePresent(ctx context.Context) HolderEvidence
	// StoppedPIDs returns the stopped process ids, for the terminal's
	// busy message only — never for a record. A bare busy is a dead end
	// for a user whose suspended pane is blocking every break, so the
	// terminal names the pids and disclaims attribution in the same
	// sentence.
	StoppedPIDs(ctx context.Context) []int32
}

// ProcHolders is the real holder check: the kernel's process table,
// in-process, no child process and no argument list.
type ProcHolders struct{}

// StoppedClaudePresent implements [HolderSightings].
func (ProcHolders) StoppedClaudePresent(ctx context.Context) HolderEvidence {
	return holderEvidenceFromSweep(proc.SameUserNamed(ctx, ClaudeProcessName))
}

// StoppedPIDs implements [HolderSightings].
func (ProcHolders) StoppedPIDs(ctx context.Context) []int32 {
	sweep, err := proc.SameUserNamed(ctx, ClaudeProcessName)
	if err != nil {
		return nil
	}
	var stopped []int32
	for _, p := range sweep {
		if p.Holder == proc.HolderStopped {
			stopped = append(stopped, p.PID)
		}
	}
	return stopped
}

// holderEvidenceFromSweep converts a process sweep into recovery
// evidence. A sweep that could not be finished is [EvidenceNone] — "I do
// not know" — never [EvidenceNoStoppedClaude]: a shorter list licenses
// nothing. A platform with no process observation at all is
// [EvidenceUnreadable], which refuses removal outright.
func holderEvidenceFromSweep(sweep []proc.Process, err error) HolderEvidence {
	if err != nil {
		if _, ok := errors.AsType[*proc.UnsupportedPlatformError](err); ok {
			return EvidenceUnreadable
		}
		return EvidenceNone
	}
	for _, p := range sweep {
		if p.Holder == proc.HolderStopped {
			return EvidenceStoppedClaudePresent
		}
	}
	return EvidenceNoStoppedClaude
}

// CleanupRegistry is where a hold registers its emergency release, so a
// signal exit still gives the peer's locks back. Injected rather than
// imported: the signal runtime owns the real registry and wires it in.
type CleanupRegistry interface {
	// Register adds a release to run on an emergency exit and returns
	// its withdrawal, which reports whether the release was still
	// registered — false means the emergency path already ran it.
	Register(release func()) (unregister func() bool)
}

// Seams is everything the lock protocol talks to that a test — or a
// later runtime — replaces.
type Seams struct {
	// FS is the three directory operations.
	FS LockFS
	// Holders is the holder check, asked its question at the moment the
	// rule asks it rather than in advance.
	Holders HolderSightings
	// Clock is both clocks, the sleeper and the jitter draw.
	Clock Clock
	// Cleanup is the emergency-release registry. RealSeams installs the
	// process-wide registry; isolated callers may supply their own.
	Cleanup CleanupRegistry
	// Fault is the injected fault hook, or nil for none.
	Fault func(name string) bool
	// Pause is an optional named pause hook for the caller. Acquisition
	// itself has no pause points.
	Pause func(name string)
}

// RealSeams returns the production seams over the given clock, including
// the process-wide emergency cleanup registry.
func RealSeams(clock Clock) *Seams {
	return &Seams{FS: RealFS{}, Holders: ProcHolders{}, Clock: clock, Cleanup: processLockCleanup{}}
}

type processLockCleanup struct{}

func (processLockCleanup) Register(release func()) func() bool {
	token := cleanup.Register(release)
	return func() bool { return cleanup.Unregister(token) }
}

// fault reports whether the named fault is injected.
func (s *Seams) fault(name string) bool {
	return s.Fault != nil && s.Fault(name)
}

// BreakDecision is what the break rule decided.
type BreakDecision int

const (
	// DecisionBroken means the directory was removed.
	DecisionBroken BreakDecision = iota
	// DecisionAbandoned means it was left alone, for the attempt's
	// reason.
	DecisionAbandoned
	// DecisionCancelled means the sampling wait was cancelled. Nothing
	// was sampled further and nothing was removed, so there is nothing
	// to record.
	DecisionCancelled
	// DecisionFailed means a filesystem operation failed. Nothing was
	// removed.
	DecisionFailed
)

// BreakAttempt is a break attempt's decision and the draft record for
// it.
type BreakAttempt struct {
	// Decision says what happened.
	Decision BreakDecision
	// Reason says why the break was abandoned, for
	// [DecisionAbandoned].
	Reason BreakReason
	// FailureMessage says what failed, for [DecisionFailed].
	FailureMessage string
	// Record is the draft to complete and append, or nil when there is
	// nothing to record: a lock that vanished, a cancelled wait, or a
	// failed operation.
	Record *BreakDraft
}

// ResolveStale decides whether one lock directory may be removed, and
// removes it.
//
// The only code in the module that removes a directory it did not
// create, so the predicate is written out rather than left to a reader,
// and it runs only with nothing held. All conditions must hold in order
// and any failure abandons the break:
//
//  1. The first stat succeeds — sample A — and the lock is at least
//     profile.Stale old.
//  2. Holder evidence: any same-user `claude` in a stopped state
//     abandons the break; unprovable peer visibility refuses it.
//     Evidence that could not be gathered continues on modification
//     times alone — it is never read as "no stopped holder".
//  3. Wait [StaleSampleInterval], cancellably.
//  4. Sample B: the stat succeeds and the modification time equals A's
//     exactly — nanoseconds, never a tolerance.
//  5. The lock is still at least profile.Stale old, and the wall and
//     monotonic clocks agree to within [ClockSkewTolerance]. The clock
//     check comes before the age check because a wall clock that
//     stepped backwards makes the second age smaller: an age test
//     placed first would call a clock jump "too young", the wrong
//     diagnosis.
//  6. Sample C, immediately before the removal, with no I/O of any
//     kind in between — no log line, no record append.
//
// Then the removal. Whether the lock is recreated afterwards is the
// acquisition's business; a recreation there is "retaken" and ends the
// acquisition. Known and accepted: a heartbeat landing just after
// sample C is unobservable to any sampling rule; the window is two
// system calls wide.
func ResolveStale(ctx context.Context, subject LockSubject, at LockSlot, profile *LockProfile, seams *Seams) BreakAttempt {
	clock := seams.Clock

	// --- Sample A ---
	wallA := clock.Wall()
	monoA := clock.Monotonic()
	mtimeA, present := seams.FS.Mtime(at)
	if !present {
		return vanishedBreak()
	}
	ageA := elapsedSince(wallA, mtimeA)

	record := &BreakDraft{
		Path:     at.Shown,
		StoreDir: subject.StoreDir,
		Tree:     subject.Tree,
		SampleA:  buildSample(wallA, mtimeA, ageA),
		Evidence: EvidenceNone,
		Outcome:  OutcomeAbandoned,
	}

	if ageA < profile.Stale {
		return abandonedBreak(record, ReasonTooYoung)
	}

	// --- Holder evidence ---
	record.Evidence = seams.Holders.StoppedClaudePresent(ctx)
	if record.Evidence == EvidenceUnreadable {
		return abandonedBreak(record, ReasonHolderUnreadable)
	}
	if record.Evidence == EvidenceStoppedClaudePresent {
		return abandonedBreak(record, ReasonHolderStopped)
	}

	// --- The sampling wait ---
	if err := clock.Sleep(ctx, StaleSampleInterval); err != nil {
		return BreakAttempt{Decision: DecisionCancelled}
	}

	// --- Sample B ---
	wallB := clock.Wall()
	monoB := clock.Monotonic()
	mtimeB, present := seams.FS.Mtime(at)
	if !present {
		return vanishedBreak()
	}
	ageB := elapsedSince(wallB, mtimeB)
	sampleB := buildSample(wallB, mtimeB, ageB)
	record.SampleB = &sampleB

	wallDelta, forward := signedDelta(wallA, wallB)
	monoDelta := max(monoB-monoA, 0)
	record.IntervalWallMS = uint64(wallDelta.Milliseconds())
	record.IntervalMonotonicMS = uint64(monoDelta.Milliseconds())

	if !mtimeB.Equal(mtimeA) {
		return abandonedBreak(record, ReasonHeartbeatObserved)
	}
	if clockSkew(wallDelta, forward, monoDelta) > ClockSkewTolerance {
		return abandonedBreak(record, ReasonClockJump)
	}
	if ageB < profile.Stale {
		return abandonedBreak(record, ReasonTooYoung)
	}

	// The injected resume: a holder that was wedged and comes back in
	// exactly the window sample C exists to close.
	if seams.fault(FaultLockResumeAfterSampleB) {
		touchSlot(at)
	}

	// --- Sample C, then the removal, with nothing in between ---
	wallC := clock.Wall()
	mtimeC, present := seams.FS.Mtime(at)
	if !present {
		return vanishedBreak()
	}
	if !mtimeC.Equal(mtimeB) {
		sampleC := buildSample(wallC, mtimeC, elapsedSince(wallC, mtimeC))
		record.SampleC = &sampleC
		return abandonedBreak(record, ReasonHeartbeatObserved)
	}
	removal := seams.FS.Rmdir(at)

	// Everything below is after the decision. Nothing above allocates,
	// logs or appends between sample C and the removal.
	sampleC := buildSample(wallC, mtimeC, elapsedSince(wallC, mtimeC))
	record.SampleC = &sampleC
	switch {
	case removal == nil:
		record.Outcome = OutcomeBroken
		record.Reason = ""
		slog.Info("removed a stale Claude Code lock",
			slog.String("path", at.Shown),
			slog.String("holder_evidence", string(record.Evidence)))
		return BreakAttempt{Decision: DecisionBroken, Record: record}
	case errors.Is(removal, ErrLockGone):
		return vanishedBreak()
	default:
		return BreakAttempt{Decision: DecisionFailed, FailureMessage: removal.Error()}
	}
}

// SampleHolderAcrossInterval is the two-sample variant of the stale
// proof, for a consumer that reports and removes through its own
// permit: two stats one interval apart, and only an unchanged
// modification time across the whole interval reads as "nothing is
// holding this". A changed time, a vanished artefact, an unreadable
// stat and a cancelled wait all read as held, because none of them
// proves absence.
func SampleHolderAcrossInterval(ctx context.Context, at LockSlot, interval time.Duration, clock Clock, fs LockFS) bool {
	first, present := fs.Mtime(at)
	if !present {
		return true
	}
	if err := clock.Sleep(ctx, interval); err != nil {
		return true
	}
	second, present := fs.Mtime(at)
	if !present {
		return true
	}
	return !second.Equal(first)
}

// vanishedBreak is a lock that disappeared: nothing was there and
// nothing was done, so there is nothing to record.
func vanishedBreak() BreakAttempt {
	return BreakAttempt{Decision: DecisionAbandoned, Reason: ReasonVanished}
}

// abandonedBreak stamps an abandoned decision onto the draft.
func abandonedBreak(record *BreakDraft, reason BreakReason) BreakAttempt {
	record.Outcome = OutcomeAbandoned
	record.Reason = reason
	return BreakAttempt{Decision: DecisionAbandoned, Reason: reason, Record: record}
}

// clockSkew is how far apart the two clocks' views of the interval
// are. A backward wall-clock step makes the distance the sum of the two
// deltas rather than their difference, which is why the direction is
// carried rather than the magnitude alone.
func clockSkew(wallDelta time.Duration, forward bool, monoDelta time.Duration) time.Duration {
	if !forward {
		return wallDelta + monoDelta
	}
	if wallDelta > monoDelta {
		return wallDelta - monoDelta
	}
	return monoDelta - wallDelta
}

// signedDelta is the wall-clock interval and whether it ran forwards.
func signedDelta(from, to time.Time) (time.Duration, bool) {
	delta := to.Sub(from)
	if delta < 0 {
		return -delta, false
	}
	return delta, true
}

// elapsedSince is how long before now a modification time was,
// saturating at zero for a clock that has gone backwards.
func elapsedSince(now, mtime time.Time) time.Duration {
	return max(now.Sub(mtime), 0)
}

// buildSample builds one record sample.
func buildSample(at, mtime time.Time, age time.Duration) LockSample {
	return LockSample{At: at, MtimeNS: mtime.UnixNano(), AgeMS: uint64(age.Milliseconds())}
}

// touchSlot sets a lock's modification time to now, for the injected
// resume fault, without following a link at the name.
func touchSlot(at LockSlot) {
	now := unix.NsecToTimespec(time.Now().UnixNano())
	_ = unix.UtimesNanoAt(at.Dir, at.Name, []unix.Timespec{now, now}, unix.AT_SYMLINK_NOFOLLOW)
}
