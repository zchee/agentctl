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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

// newFakeClockAt returns a fakeClock whose wall clock starts at wall,
// for a test that samples real files.
func newFakeClockAt(wall time.Time) *fakeClock {
	return &fakeClock{wall: wall}
}

// mtimeStep is one scripted answer of the fake filesystem's stat.
type mtimeStep struct {
	at      time.Time
	present bool
}

// fakeLockFS scripts the three directory operations and records their
// sequence, so a test can assert what happened between two samples —
// something no inspection of the filesystem afterwards can check.
type fakeLockFS struct {
	mu sync.Mutex
	// ops is every operation in order, as "op name".
	ops []string
	// mtimes maps a slot name to its scripted stat answers, consumed
	// one per call with the last repeating.
	mtimes map[string][]mtimeStep
	// rmdirErr scripts a removal failure per slot name.
	rmdirErr map[string]error
	// removed is every name that was removed.
	removed []string
}

func newFakeLockFS() *fakeLockFS {
	return &fakeLockFS{mtimes: map[string][]mtimeStep{}, rmdirErr: map[string]error{}}
}

func (f *fakeLockFS) script(name string, steps ...mtimeStep) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mtimes[name] = steps
}

func (f *fakeLockFS) Mkdir(at LockSlot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "mkdir "+at.Name)
	return nil
}

func (f *fakeLockFS) Rmdir(at LockSlot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "rmdir "+at.Name)
	if err := f.rmdirErr[at.Name]; err != nil {
		return err
	}
	f.removed = append(f.removed, at.Name)
	// A removed artefact answers absent from here on.
	f.mtimes[at.Name] = []mtimeStep{{}}
	return nil
}

func (f *fakeLockFS) Mtime(at LockSlot) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "mtime "+at.Name)
	steps := f.mtimes[at.Name]
	if len(steps) == 0 {
		return time.Time{}, false
	}
	step := steps[0]
	if len(steps) > 1 {
		f.mtimes[at.Name] = steps[1:]
	}
	return step.at, step.present
}

// fakeHolders scripts the holder check and counts how often it was
// asked.
type fakeHolders struct {
	evidence HolderEvidence
	pids     []int32
	asked    int
}

func (h *fakeHolders) StoppedClaudePresent(context.Context) HolderEvidence {
	h.asked++
	return h.evidence
}

func (h *fakeHolders) StoppedPIDs(context.Context) []int32 {
	return h.pids
}

// staleSeams is a seam set over the fakes, with no-stopped-holder
// evidence unless a test scripts otherwise.
func staleSeams(fs LockFS, clock Clock, holders HolderSightings) *Seams {
	return &Seams{FS: fs, Holders: holders, Clock: clock}
}

// aSubject is the hold the break rule is run for in these tests.
var aSubject = LockSubject{StoreDir: "/store/acct/org", Tree: TreeOwn}

// theSlot is the artefact the fakes operate on; the descriptor is
// never dereferenced by a fake.
var theSlot = LockSlot{Dir: -1, Name: ".oauth_refresh.lock", Shown: "/store/acct/org/.oauth_refresh.lock"}

func TestADeadHoldersLockIsBrokenWithThreeAgreeingSamples(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	fs := newFakeLockFS()
	old := clock.Wall().Add(-2 * time.Minute)
	fs.script(theSlot.Name, mtimeStep{at: old, present: true})
	holders := &fakeHolders{evidence: EvidenceNoStoppedClaude}

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, holders))

	if got.Decision != DecisionBroken {
		t.Fatalf("Decision = %v, want broken; reason %q message %q", got.Decision, got.Reason, got.FailureMessage)
	}
	record := got.Record
	if record == nil {
		t.Fatalf("a break must carry its record")
	}
	if record.Outcome != OutcomeBroken || record.Reason != "" {
		t.Errorf("record = %q/%q, want broken with no reason: every reason is a reason not to have broken", record.Outcome, record.Reason)
	}
	if record.SampleB == nil || record.SampleC == nil {
		t.Fatalf("all three samples must be recorded")
	}
	if record.Evidence != EvidenceNoStoppedClaude {
		t.Errorf("Evidence = %q", record.Evidence)
	}
	if record.IntervalWallMS != uint64(StaleSampleInterval.Milliseconds()) || record.IntervalMonotonicMS != uint64(StaleSampleInterval.Milliseconds()) {
		t.Errorf("intervals = %d/%d ms, want the sampling interval on both clocks", record.IntervalWallMS, record.IntervalMonotonicMS)
	}
	if len(fs.removed) != 1 {
		t.Fatalf("removed = %v, want exactly one removal", fs.removed)
	}
	// No I/O of any kind between sample C and the removal: the last two
	// operations are that stat and that removal, adjacent.
	if tail := fs.ops[len(fs.ops)-2:]; !slices.Equal(tail, []string{"mtime " + theSlot.Name, "rmdir " + theSlot.Name}) {
		t.Errorf("operations between sample C and the removal: %v", tail)
	}
	// And the one sleep is the sampling interval, taken while holding
	// nothing.
	if !slices.Equal(clock.sleeps, []time.Duration{StaleSampleInterval}) {
		t.Errorf("sleeps = %v, want one sampling interval", clock.sleeps)
	}
}

func TestAHeartbeatAnywhereInsideTheWindowAbandonsTheBreak(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		beat time.Duration
	}{
		"success: a full heartbeat abandons the break":   {beat: 5 * time.Second},
		"success: a one-nanosecond move abandons it too": {beat: time.Nanosecond},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := newFakeClock()
			fs := newFakeLockFS()
			old := clock.Wall().Add(-2 * time.Minute)
			fs.script(theSlot.Name,
				mtimeStep{at: old, present: true},
				mtimeStep{at: old.Add(tt.beat), present: true})

			got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

			if got.Decision != DecisionAbandoned || got.Reason != ReasonHeartbeatObserved {
				t.Fatalf("got %v/%q, want abandoned for an observed heartbeat", got.Decision, got.Reason)
			}
			if len(fs.removed) != 0 {
				t.Errorf("nothing may be removed: %v", fs.removed)
			}
			if got.Record == nil || got.Record.SampleB == nil {
				t.Errorf("the abandonment must record both samples")
			}
		})
	}
}

func TestAHeartbeatBetweenSampleBAndSampleCAbandonsAtSampleC(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	fs := newFakeLockFS()
	old := clock.Wall().Add(-2 * time.Minute)
	fs.script(theSlot.Name,
		mtimeStep{at: old, present: true},
		mtimeStep{at: old, present: true},
		mtimeStep{at: old.Add(time.Nanosecond), present: true})

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

	if got.Decision != DecisionAbandoned || got.Reason != ReasonHeartbeatObserved {
		t.Fatalf("got %v/%q, want abandoned at sample C", got.Decision, got.Reason)
	}
	if got.Record == nil || got.Record.SampleC == nil {
		t.Fatalf("the third sample must be recorded")
	}
	if len(fs.removed) != 0 {
		t.Errorf("nothing may be removed: %v", fs.removed)
	}
}

func TestALockYoungerThanItsProfileIsTooYoung(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	fs := newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-30 * time.Second), present: true})
	holders := &fakeHolders{evidence: EvidenceNoStoppedClaude}

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, holders))

	if got.Decision != DecisionAbandoned || got.Reason != ReasonTooYoung {
		t.Fatalf("got %v/%q, want too young", got.Decision, got.Reason)
	}
	if holders.asked != 0 {
		t.Errorf("a too-young lock must not cost a process sweep")
	}
	if len(clock.sleeps) != 0 {
		t.Errorf("a too-young lock must not cost a sampling wait: %v", clock.sleeps)
	}
}

func TestEachProfileIsJudgedByItsOwnStalenessWindow(t *testing.T) {
	t.Parallel()

	// 30 s old: too young for the 60 s refresh window, stale for the
	// 15 s storage-write window.
	clock := newFakeClock()
	fs := newFakeLockFS()
	old := clock.Wall().Add(-30 * time.Second)
	fs.script(theSlot.Name, mtimeStep{at: old, present: true})

	byRefresh := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))
	if byRefresh.Reason != ReasonTooYoung {
		t.Errorf("the refresh profile at 30s = %q, want too young", byRefresh.Reason)
	}

	fs = newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: old, present: true})
	byStorage := ResolveStale(t.Context(), aSubject, theSlot, &StorageWriteProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))
	if byStorage.Decision != DecisionBroken {
		t.Errorf("the storage-write profile at 30s = %v/%q, want broken", byStorage.Decision, byStorage.Reason)
	}
}

func TestAStoppedSameUserClaudeAbandonsTheBreakBeforeAnyWait(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	fs := newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-2 * time.Minute), present: true})

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceStoppedClaudePresent}))

	if got.Decision != DecisionAbandoned || got.Reason != ReasonHolderStopped {
		t.Fatalf("got %v/%q, want abandoned for a stopped holder", got.Decision, got.Reason)
	}
	if len(clock.sleeps) != 0 {
		t.Errorf("the abandonment must come before the sampling wait: %v", clock.sleeps)
	}
	if got.Record == nil || got.Record.Evidence != EvidenceStoppedClaudePresent {
		t.Errorf("the record must carry the evidence")
	}
}

func TestUnprovablePeerVisibilityRefusesRemoval(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	fs := newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-2 * time.Minute), present: true})

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceUnreadable}))

	if got.Decision != DecisionAbandoned || got.Reason != ReasonHolderUnreadable {
		t.Fatalf("got %v/%q, want abandoned for unprovable visibility", got.Decision, got.Reason)
	}
	if len(fs.removed) != 0 {
		t.Errorf("modification times cannot authorize removal here: %v", fs.removed)
	}
}

func TestUnavailableEvidenceContinuesOnModificationTimesAlone(t *testing.T) {
	t.Parallel()

	// "Could not check" is not "no stopped holder" — but it is not a
	// refusal either: the rule continues and rests on the samples.
	clock := newFakeClock()
	fs := newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-2 * time.Minute), present: true})

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNone}))

	if got.Decision != DecisionBroken {
		t.Fatalf("got %v/%q, want broken on the samples alone", got.Decision, got.Reason)
	}
	if got.Record.Evidence != EvidenceNone {
		t.Errorf("Evidence = %q, want none", got.Record.Evidence)
	}
}

func TestAClockStepInEitherDirectionAbandonsTheBreak(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		wall time.Duration
	}{
		"success: a forward wall step abandons the break":             {wall: StaleSampleInterval + 2*time.Second},
		"success: a backward wall step abandons the break":            {wall: StaleSampleInterval - 2*time.Second},
		"success: a wall clock that ran backwards abandons the break": {wall: -3 * time.Second},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := newFakeClock()
			clock.onSleep = func(c *fakeClock, slept time.Duration) {
				// The monotonic clock cannot step; the wall clock just
				// did.
				c.advance(tt.wall-slept, 0)
				c.advance(0, slept)
			}
			fs := newFakeLockFS()
			fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-2 * time.Minute), present: true})

			got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

			if got.Decision != DecisionAbandoned || got.Reason != ReasonClockJump {
				t.Fatalf("got %v/%q, want abandoned for a clock jump", got.Decision, got.Reason)
			}
			if len(fs.removed) != 0 {
				t.Errorf("nothing may be removed under a stepped clock: %v", fs.removed)
			}
		})
	}
}

func TestAClockWithinToleranceDoesNotAbandonTheBreak(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	clock.onSleep = func(c *fakeClock, slept time.Duration) {
		// Half a second of disagreement: inside the tolerance.
		c.advance(slept+500*time.Millisecond, slept)
	}
	fs := newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-2 * time.Minute), present: true})

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

	if got.Decision != DecisionBroken {
		t.Fatalf("got %v/%q, want broken: half a second is inside the tolerance", got.Decision, got.Reason)
	}
}

func TestALockThatVanishesAtAnySampleRecordsNothing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		steps []mtimeStep
	}{
		"success: vanished at sample A": {steps: []mtimeStep{{}}},
		"success: vanished at sample B": {steps: []mtimeStep{{at: time.Unix(1, 0), present: true}, {}}},
		"success: vanished at sample C": {steps: []mtimeStep{{at: time.Unix(1, 0), present: true}, {at: time.Unix(1, 0), present: true}, {}}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := newFakeClock()
			fs := newFakeLockFS()
			fs.script(theSlot.Name, tt.steps...)

			got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

			if got.Decision != DecisionAbandoned || got.Reason != ReasonVanished {
				t.Fatalf("got %v/%q, want vanished", got.Decision, got.Reason)
			}
			if got.Record != nil {
				t.Errorf("nothing was there and nothing was done, so nothing may be recorded: %+v", got.Record)
			}
		})
	}
}

func TestACancelledSamplingWaitDecidesNothingAndRecordsNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	clock := newFakeClock()
	clock.onSleep = func(*fakeClock, time.Duration) { cancel() }
	fs := newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-2 * time.Minute), present: true})

	got := ResolveStale(ctx, aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

	if got.Decision != DecisionCancelled {
		t.Fatalf("got %v, want cancelled", got.Decision)
	}
	if got.Record != nil {
		t.Errorf("a cancelled wait sampled nothing further and removed nothing: %+v", got.Record)
	}
	if len(fs.removed) != 0 {
		t.Errorf("nothing may be removed: %v", fs.removed)
	}
}

func TestARemovalFailureIsReportedAndNothingIsClaimed(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	fs := newFakeLockFS()
	fs.script(theSlot.Name, mtimeStep{at: clock.Wall().Add(-2 * time.Minute), present: true})
	fs.rmdirErr[theSlot.Name] = &LockIOError{Context: "could not remove", Message: "it is not empty"}

	got := ResolveStale(t.Context(), aSubject, theSlot, &RefreshProfile, staleSeams(fs, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

	if got.Decision != DecisionFailed || got.FailureMessage == "" {
		t.Fatalf("got %v/%q, want a named failure", got.Decision, got.FailureMessage)
	}
	if got.Record != nil {
		t.Errorf("a failed removal removed nothing, so it records nothing")
	}
}

func TestTheInjectedResumeReachesTheWindowSampleCCloses(t *testing.T) {
	t.Parallel()

	// Real filesystem: the fault touches the real directory between
	// sample B and sample C, which is exactly the wedged-holder resume
	// the third sample exists to catch.
	dir := t.TempDir()
	lock := filepath.Join(dir, RefreshLockName)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatalf("Chtimes() = %v", err)
	}
	clock := newFakeClockAt(time.Now())
	seams := staleSeams(RealFS{}, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude})
	seams.Fault = func(name string) bool { return name == FaultLockResumeAfterSampleB }
	at := LockSlot{Dir: openTestDir(t, dir), Name: RefreshLockName, Shown: lock}

	got := ResolveStale(t.Context(), LockSubject{StoreDir: dir, Tree: TreeOwn}, at, &RefreshProfile, seams)

	if got.Decision != DecisionAbandoned || got.Reason != ReasonHeartbeatObserved {
		t.Fatalf("got %v/%q, want the resume caught at sample C", got.Decision, got.Reason)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the resumed holder's lock must survive: %v", err)
	}
}

func TestThePublicRuleRunsAgainstTheRealClockAndFilesystem(t *testing.T) {
	t.Parallel()

	// A vanished artefact decides before the sampling wait, so the real
	// 12-second clock is never slept on.
	dir := t.TempDir()
	at := LockSlot{Dir: openTestDir(t, dir), Name: RefreshLockName, Shown: filepath.Join(dir, RefreshLockName)}

	got := ResolveStale(t.Context(), LockSubject{StoreDir: dir, Tree: TreeOwn}, at, &RefreshProfile, RealSeams(SystemClock()))

	if got.Decision != DecisionAbandoned || got.Reason != ReasonVanished {
		t.Fatalf("got %v/%q, want vanished", got.Decision, got.Reason)
	}
}

func TestTheRealHolderCheckNeverClaimsASweepItDidNotFinish(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		sweep []proc.Process
		err   error
		want  HolderEvidence
	}{
		"success: a stopped process is reported":             {sweep: []proc.Process{{PID: 7, Holder: proc.HolderStopped}}, want: EvidenceStoppedClaudePresent},
		"success: an all-alive sweep is a clean answer":      {sweep: []proc.Process{{PID: 7, Holder: proc.HolderAlive}}, want: EvidenceNoStoppedClaude},
		"success: an empty finished sweep is a clean answer": {want: EvidenceNoStoppedClaude},
		"error: a failed sweep claims nothing":               {err: errors.New("process table unreadable"), want: EvidenceNone},
		"error: an unsupported platform refuses removal":     {err: &proc.UnsupportedPlatformError{GOOS: "plan9"}, want: EvidenceUnreadable},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := holderEvidenceFromSweep(tt.sweep, tt.err); got != tt.want {
				t.Errorf("holderEvidenceFromSweep() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTheTwoSampleVariantOnlyClearsAnUnchangedModificationTime(t *testing.T) {
	t.Parallel()

	old := time.Unix(1_700_000_000, 0)
	tests := map[string]struct {
		steps  []mtimeStep
		cancel bool
		want   bool
	}{
		"success: an unchanged time across the interval reads unheld": {steps: []mtimeStep{{at: old, present: true}, {at: old, present: true}}, want: false},
		"success: a rewritten artefact reads held":                    {steps: []mtimeStep{{at: old, present: true}, {at: old.Add(time.Nanosecond), present: true}}, want: true},
		"success: an artefact missing at the first sample reads held": {steps: []mtimeStep{{}}, want: true},
		"success: an artefact gone at the second sample reads held":   {steps: []mtimeStep{{at: old, present: true}, {}}, want: true},
		"success: a cancelled wait proves nothing and reads held":     {steps: []mtimeStep{{at: old, present: true}, {at: old, present: true}}, cancel: true, want: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			clock := newFakeClock()
			if tt.cancel {
				cancellable, cancel := context.WithCancel(ctx)
				clock.onSleep = func(*fakeClock, time.Duration) { cancel() }
				ctx = cancellable
			}
			fs := newFakeLockFS()
			fs.script(theSlot.Name, tt.steps...)

			if got := SampleHolderAcrossInterval(ctx, theSlot, StaleSampleInterval, clock, fs); got != tt.want {
				t.Errorf("SampleHolderAcrossInterval() = %t, want %t", got, tt.want)
			}
		})
	}
}

// peerLockHolderEnv tells a re-executed copy of this test binary to act
// as a lock holder instead of asserting anything: the directory it
// serves commands for.
const peerLockHolderEnv = "AGENTCTL_TEST_PEER_LOCK_HOLDER_DIR"

// lockHolderChild is a genuinely separate process holding a peer lock:
// the re-executed test binary running [TestLockHolderProtocol]'s serve
// half, driven over its standard input.
type lockHolderChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	t      *testing.T
	exited bool
}

// spawnLockHolder re-executes the test binary as a holder for the
// given lock directory.
func spawnLockHolder(t *testing.T, lockDir string) *lockHolderChild {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestLockHolderProtocol$", "-test.count=1")
	cmd.Env = append(os.Environ(), peerLockHolderEnv+"="+lockDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() = %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() = %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	child := &lockHolderChild{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout), t: t}
	t.Cleanup(func() {
		if !child.exited {
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	})
	return child
}

// order sends one command and waits for its acknowledgement.
func (c *lockHolderChild) order(command string) {
	c.t.Helper()
	if _, err := fmt.Fprintln(c.stdin, command); err != nil {
		c.t.Fatalf("ordering %q: %v", command, err)
	}
	line, err := c.out.ReadString('\n')
	if err != nil {
		c.t.Fatalf("waiting for %q: %v", command, err)
	}
	if line != "ok\n" {
		c.t.Fatalf("the holder answered %q to %q", line, command)
	}
}

// exit ends the holder and waits for the process to be collected, so
// its id provably names nothing.
func (c *lockHolderChild) exit() {
	c.t.Helper()
	if _, err := fmt.Fprintln(c.stdin, "exit"); err != nil {
		c.t.Fatalf("ordering the exit: %v", err)
	}
	_ = c.stdin.Close()
	if err := c.cmd.Wait(); err != nil {
		c.t.Fatalf("the holder should exit cleanly: %v", err)
	}
	c.exited = true
}

// serveLockHolder is the child half: it performs each ordered
// operation on the lock directory and acknowledges it.
func serveLockHolder(lockDir string) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "mkdir":
			if err := os.Mkdir(lockDir, 0o700); err != nil {
				fmt.Fprintf(os.Stderr, "holder: mkdir: %v\n", err)
				os.Exit(1)
			}
		case "touch":
			now := time.Now()
			if err := os.Chtimes(lockDir, now, now); err != nil {
				fmt.Fprintf(os.Stderr, "holder: touch: %v\n", err)
				os.Exit(1)
			}
		case "rmdir":
			if err := os.Remove(lockDir); err != nil {
				fmt.Fprintf(os.Stderr, "holder: rmdir: %v\n", err)
				os.Exit(1)
			}
		case "exit":
			return
		default:
			fmt.Fprintf(os.Stderr, "holder: unknown order %q\n", scanner.Text())
			os.Exit(1)
		}
		fmt.Println("ok")
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "holder: read order: %v\n", err)
		os.Exit(1)
	}
}

// TestLockHolderProtocol is two tests in one binary: re-executed with
// the holder environment set it serves lock operations for its parent,
// and run normally it proves the protocol itself works end to end.
func TestLockHolderProtocol(t *testing.T) {
	if dir := os.Getenv(peerLockHolderEnv); dir != "" {
		serveLockHolder(dir)
		return
	}
	t.Parallel()

	lock := filepath.Join(t.TempDir(), RefreshLockName)
	holder := spawnLockHolder(t, lock)
	holder.order("mkdir")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the holder's mkdir must land: %v", err)
	}
	before, err := os.Stat(lock)
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	holder.order("touch")
	after, err := os.Stat(lock)
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Errorf("the holder's touch must move the modification time: %v then %v", before.ModTime(), after.ModTime())
	}
	holder.order("rmdir")
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("the holder's rmdir must land: %v", err)
	}
	holder.exit()
}

func TestALiveHoldersLockIsNeverBroken(t *testing.T) {
	t.Parallel()

	// A genuinely separate process holds the lock and heartbeats it in
	// the middle of the sampling window; the fake clock only replaces
	// the 12-second wait, every stat is real.
	dir := t.TempDir()
	lock := filepath.Join(dir, RefreshLockName)
	holder := spawnLockHolder(t, lock)
	holder.order("mkdir")
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatalf("Chtimes() = %v", err)
	}

	clock := newFakeClockAt(time.Now())
	clock.onSleep = func(c *fakeClock, slept time.Duration) {
		holder.order("touch")
		c.advance(slept, slept)
	}
	at := LockSlot{Dir: openTestDir(t, dir), Name: RefreshLockName, Shown: lock}

	got := ResolveStale(t.Context(), LockSubject{StoreDir: dir, Tree: TreeOwn}, at, &RefreshProfile, staleSeams(RealFS{}, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

	if got.Decision != DecisionAbandoned || got.Reason != ReasonHeartbeatObserved {
		t.Fatalf("got %v/%q, want the live holder's heartbeat observed", got.Decision, got.Reason)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the live holder's lock must survive: %v", err)
	}
	holder.order("rmdir")
	holder.exit()
}

func TestADeadHoldersLockIsBrokenOnTheRealFilesystem(t *testing.T) {
	t.Parallel()

	// The holder made the directory and is gone: three agreeing real
	// stats later the directory is removed, exactly once.
	dir := t.TempDir()
	lock := filepath.Join(dir, RefreshLockName)
	holder := spawnLockHolder(t, lock)
	holder.order("mkdir")
	holder.exit()
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatalf("Chtimes() = %v", err)
	}

	clock := newFakeClockAt(time.Now())
	at := LockSlot{Dir: openTestDir(t, dir), Name: RefreshLockName, Shown: lock}

	got := ResolveStale(t.Context(), LockSubject{StoreDir: dir, Tree: TreeOwn}, at, &RefreshProfile, staleSeams(RealFS{}, clock, &fakeHolders{evidence: EvidenceNoStoppedClaude}))

	if got.Decision != DecisionBroken {
		t.Fatalf("got %v/%q, want broken", got.Decision, got.Reason)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("the dead holder's lock must be gone: %v", err)
	}
	if got.Record == nil || got.Record.Outcome != OutcomeBroken {
		t.Errorf("the break must be recorded")
	}
}
