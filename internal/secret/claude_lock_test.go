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
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestPeerLockConstants(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		got  any
		want any
	}{
		"success: the peer heartbeat is 5s":         {got: PeerHeartbeat, want: 5 * time.Second},
		"success: five contention rounds":           {got: ContentionRounds, want: 5},
		"success: a round's fixed part is 1s":       {got: ContentionRoundBase, want: 1 * time.Second},
		"success: a round's jitter span is 1s":      {got: ContentionRoundJitter, want: 1 * time.Second},
		"success: the contention floor is 7.5s":     {got: ContentionFloor, want: 7500 * time.Millisecond},
		"success: the stale sample interval is 12s": {got: StaleSampleInterval, want: 12 * time.Second},
		"success: the clock skew tolerance is 1s":   {got: ClockSkewTolerance, want: 1 * time.Second},
		"success: the hold budget is 3s":            {got: HoldBudget, want: 3000 * time.Millisecond},
		"success: the config hold budget is 1.2s":   {got: ConfigHoldBudget, want: 1200 * time.Millisecond},
		"success: three restarts":                   {got: MaxRestarts, want: 3},
		"success: the legacy lock suffix is .lock":  {got: LegacyLockSuffix, want: ".lock"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := gocmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("constant mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStaleProfiles(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		profile LockProfile
		stale   time.Duration
		budget  time.Duration
	}{
		"success: refresh and legacy locks go stale at 60s":  {profile: RefreshProfile, stale: 60 * time.Second, budget: HoldBudget},
		"success: the storage-write mutex goes stale at 15s": {profile: StorageWriteProfile, stale: 15 * time.Second, budget: HoldBudget},
		"success: the configuration lock goes stale at 10s":  {profile: ConfigProfile, stale: 10 * time.Second, budget: ConfigHoldBudget},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.profile.Stale != tt.stale {
				t.Errorf("Stale = %v, want %v", tt.profile.Stale, tt.stale)
			}
			if tt.profile.Update != PeerHeartbeat {
				t.Errorf("Update = %v, want the peer heartbeat %v", tt.profile.Update, PeerHeartbeat)
			}
			if tt.profile.Retries != 0 {
				t.Errorf("Retries = %d: a retry is a wait, and a wait belongs outside the hold", tt.profile.Retries)
			}
			if tt.profile.HoldBudget != tt.budget {
				t.Errorf("HoldBudget = %v, want %v", tt.profile.HoldBudget, tt.budget)
			}
		})
	}
}

func TestTheSamplingIntervalStaysClearOfThePeersHeartbeat(t *testing.T) {
	t.Parallel()

	// A single peer heartbeat anywhere in the window must fail the
	// modification-time comparison, so the window has to span more than
	// two heartbeat periods.
	if StaleSampleInterval <= 2*PeerHeartbeat {
		t.Errorf("StaleSampleInterval = %v, must stay greater than 2 × %v", StaleSampleInterval, PeerHeartbeat)
	}
}

func TestEveryBudgetStaysUnderTheFloorItIsDerivedFrom(t *testing.T) {
	t.Parallel()

	// The peer's own refresh gives up after 4000 ms at the floor, so the
	// hold must end before that.
	if HoldBudget >= 4000*time.Millisecond {
		t.Errorf("HoldBudget = %v, must stay under the peer's 4000 ms give-up", HoldBudget)
	}
	// A session inside its first 30 s abandons its configuration-lock
	// ladder after 1500 ms and then writes the whole document unlocked.
	if ConfigHoldBudget >= 1500*time.Millisecond {
		t.Errorf("ConfigHoldBudget = %v, must stay under the peer's 1500 ms give-up", ConfigHoldBudget)
	}
	if ConfigProfile.HoldBudget != ConfigHoldBudget {
		t.Errorf("ConfigProfile.HoldBudget = %v, want %v", ConfigProfile.HoldBudget, ConfigHoldBudget)
	}
}

func TestTheHolderEvidenceVocabulary(t *testing.T) {
	t.Parallel()

	// The vocabulary itself is the assertion: no value names a pid or
	// claims a store was identified, so a later edit cannot reintroduce
	// attribution through the log.
	want := []HolderEvidence{
		EvidenceStoppedClaudePresent,
		EvidenceNoStoppedClaude,
		EvidenceUnreadable,
		EvidenceNone,
	}
	spelled := []string{"stopped_claude_present", "no_stopped_claude", "unreadable", "none"}
	for i, evidence := range want {
		if string(evidence) != spelled[i] {
			t.Errorf("evidence %d = %q, want %q", i, evidence, spelled[i])
		}
	}
}

func TestTheOutcomeAndReasonVocabularies(t *testing.T) {
	t.Parallel()

	if OutcomeBroken != "broken" || OutcomeAbandoned != "abandoned" {
		t.Errorf("outcomes = %q, %q; want broken, abandoned", OutcomeBroken, OutcomeAbandoned)
	}
	reasons := map[BreakReason]string{
		ReasonHeartbeatObserved: "heartbeat_observed",
		ReasonTooYoung:          "too_young",
		ReasonVanished:          "vanished",
		ReasonClockJump:         "clock_jump",
		ReasonRetaken:           "retaken",
		ReasonHolderStopped:     "holder_stopped",
		ReasonHolderUnreadable:  "holder_unreadable",
	}
	if len(reasons) != 7 {
		t.Fatalf("there are exactly seven reasons, found %d", len(reasons))
	}
	for reason, spelling := range reasons {
		if string(reason) != spelling {
			t.Errorf("reason %q, want %q", reason, spelling)
		}
	}
}

func TestTreeSpellingsAndTargets(t *testing.T) {
	t.Parallel()

	if TreeOwn != "agctl" || TreeLive != "live" {
		t.Errorf("trees = %q, %q; the on-disk words are fixed by the store format", TreeOwn, TreeLive)
	}
	if TreeOwn.Label() == "" || TreeLive.Label() == "" || TreeOwn.Label() == TreeLive.Label() {
		t.Errorf("tree labels must be distinct words: %q vs %q", TreeOwn.Label(), TreeLive.Label())
	}
	if got := NamespaceTarget("0a1b2c3d"); got != Target("namespace:0a1b2c3d") {
		t.Errorf("NamespaceTarget() = %q, want namespace:0a1b2c3d", got)
	}
	if TargetLive != "live" {
		t.Errorf("TargetLive = %q, want live", TargetLive)
	}
}

func TestBreakDraftCompleteCarriesEveryObservedField(t *testing.T) {
	t.Parallel()

	sampleB := LockSample{MtimeNS: 2, AgeMS: 61_000}
	draft := &BreakDraft{
		Path:                "/store/.oauth_refresh.lock",
		StoreDir:            "/store",
		Tree:                TreeOwn,
		SampleA:             LockSample{MtimeNS: 1, AgeMS: 60_500},
		SampleB:             &sampleB,
		IntervalWallMS:      12_000,
		IntervalMonotonicMS: 12_001,
		Evidence:            EvidenceNoStoppedClaude,
		Outcome:             OutcomeAbandoned,
		Reason:              ReasonHeartbeatObserved,
	}

	record := draft.Complete("Claude Code-credentials", TargetLive)

	want := &LockBreakRecord{
		Path:                "/store/.oauth_refresh.lock",
		StoreDir:            "/store",
		Tree:                TreeOwn,
		Service:             "Claude Code-credentials",
		Target:              TargetLive,
		SampleA:             LockSample{MtimeNS: 1, AgeMS: 60_500},
		SampleB:             &sampleB,
		IntervalWallMS:      12_000,
		IntervalMonotonicMS: 12_001,
		Evidence:            EvidenceNoStoppedClaude,
		Outcome:             OutcomeAbandoned,
		Reason:              ReasonHeartbeatObserved,
	}
	if diff := gocmp.Diff(want, record); diff != "" {
		t.Errorf("Complete() mismatch (-want +got):\n%s", diff)
	}
}

func TestSystemClockJitterStaysInsideItsSpan(t *testing.T) {
	t.Parallel()

	clock := SystemClock()
	for range 1000 {
		draw := clock.Jitter(time.Second)
		if draw < 0 || draw >= time.Second {
			t.Fatalf("Jitter(1s) = %v, want a draw in [0s, 1s)", draw)
		}
	}
	if draw := clock.Jitter(0); draw != 0 {
		t.Errorf("Jitter(0) = %v, want 0", draw)
	}
	if draw := clock.Jitter(-time.Second); draw != 0 {
		t.Errorf("Jitter(-1s) = %v, want 0", draw)
	}
}

func TestSystemClockSleepIsCancellable(t *testing.T) {
	t.Parallel()

	clock := SystemClock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	err := clock.Sleep(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep() under a cancelled context = %v, want %v", err, context.Canceled)
	}
	if waited := time.Since(start); waited >= 5*time.Second {
		t.Errorf("a cancelled sleep must end at once, waited %v", waited)
	}

	if err := clock.Sleep(t.Context(), time.Millisecond); err != nil {
		t.Errorf("Sleep(1ms) = %v, want success", err)
	}
}

func TestSystemClockWallReadingsSubtractAsWallTime(t *testing.T) {
	t.Parallel()

	clock := SystemClock()
	first := clock.Wall()
	// A reading carrying a monotonic component would make later
	// arithmetic reflect the monotonic clock, which is exactly what the
	// wall half of the skew comparison must not do.
	if got, stripped := first, first.Round(0); !got.Equal(stripped) || got != stripped {
		t.Errorf("Wall() = %v carries a monotonic reading", got)
	}
	before := clock.Monotonic()
	after := clock.Monotonic()
	if after < before {
		t.Errorf("Monotonic() went backwards: %v then %v", before, after)
	}
}
