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
	"time"
)

// Tree says which credential tree a lock artefact belongs to.
//
// The one spelling of the distinction: the lock protocol records it, the
// held-lock records carry it, and doctor prints it. The on-disk words are
// fixed by the store format, so a reader and a writer cannot drift apart.
type Tree string

const (
	// TreeOwn is a store under this manager's own namespace root.
	TreeOwn Tree = "agctl"
	// TreeLive is the live Claude Code store. Only the live swap may
	// break a lock here.
	TreeLive Tree = "live"
)

// Label returns the words diagnostics print for this tree.
func (t Tree) Label() string {
	switch t {
	case TreeLive:
		return "the live store"
	default:
		return "the manager's own tree"
	}
}

// Target says which keychain item an event is about, rendered as one
// string so a report line, a filter and a human reading the file all see
// the same token: `live` for the unsuffixed item, `namespace:<sha8>` for
// a namespaced one.
type Target string

// TargetLive is the unsuffixed live keychain item.
const TargetLive Target = "live"

// NamespaceTarget returns the target token for a namespaced item, by the
// eight hex digits of its suffix.
func NamespaceTarget(sha8 string) Target {
	return Target("namespace:" + sha8)
}

// HolderEvidence is what the holder check could see.
//
// Four values, and no value names a pid or claims a store was
// identified — the vocabulary itself is the assertion, so a later edit
// cannot reintroduce attribution through the log. Partial evidence — any
// pid whose state could not be read — is EvidenceNone, never
// EvidenceNoStoppedClaude.
type HolderEvidence string

const (
	// EvidenceStoppedClaudePresent means some same-user `claude` process
	// is stopped or traced.
	EvidenceStoppedClaudePresent HolderEvidence = "stopped_claude_present"
	// EvidenceNoStoppedClaude means every same-user `claude` process was
	// readable and none was stopped.
	EvidenceNoStoppedClaude HolderEvidence = "no_stopped_claude"
	// EvidenceUnreadable means peer visibility is unproved; modification
	// times cannot authorize a removal.
	EvidenceUnreadable HolderEvidence = "unreadable"
	// EvidenceNone means the check could not be made, or could not be
	// made completely.
	EvidenceNone HolderEvidence = "none"
)

// BreakOutcome says whether a stale artefact was removed.
type BreakOutcome string

const (
	// OutcomeBroken means the directory was removed.
	OutcomeBroken BreakOutcome = "broken"
	// OutcomeAbandoned means the rule refused and the directory was left
	// exactly as it was.
	OutcomeAbandoned BreakOutcome = "abandoned"
)

// BreakReason says why a break did not happen — or, for ReasonRetaken,
// what happened to the artefact after one did.
//
// Every member is a reason not to have broken a lock, which is why a
// clean break carries no reason at all. There is deliberately no word
// meaning "it was stale", because staleness is the precondition of the
// whole rule rather than an outcome of it.
type BreakReason string

const (
	// ReasonHeartbeatObserved means a sample's modification time
	// differed from the one before it: somebody is alive in there.
	ReasonHeartbeatObserved BreakReason = "heartbeat_observed"
	// ReasonTooYoung means the artefact was not yet stale.
	ReasonTooYoung BreakReason = "too_young"
	// ReasonVanished means the artefact disappeared between samples.
	ReasonVanished BreakReason = "vanished"
	// ReasonClockJump means the wall clock and the monotonic clock
	// disagreed by more than the tolerance.
	ReasonClockJump BreakReason = "clock_jump"
	// ReasonRetaken means the artefact was back after the removal: a
	// peer re-took it, and there is no second break.
	ReasonRetaken BreakReason = "retaken"
	// ReasonHolderStopped means a same-user `claude` process is stopped,
	// so the break was abandoned whether or not that process is the
	// holder.
	ReasonHolderStopped BreakReason = "holder_stopped"
	// ReasonHolderUnreadable means peer visibility could not be
	// established, so removal is unsupported.
	ReasonHolderUnreadable BreakReason = "holder_unreadable"
)

// LockSample is one stat of a lock directory.
type LockSample struct {
	// At is when the sample was taken.
	At time.Time `json:"at"`
	// MtimeNS is the directory's modification time in nanoseconds since
	// the epoch. Nanoseconds are load-bearing: the comparison between two
	// samples is exact, and a resolution that rounded would silently
	// grant a tolerance the rule forbids.
	MtimeNS int64 `json:"mtime_ns"`
	// AgeMS is how old the directory was at that moment.
	AgeMS uint64 `json:"age_ms"`
}

// BreakDraft is everything the break rule can observe about one break,
// waiting for the two fields only its caller knows.
//
// The rule fills the members it can see; the swap supplies the service
// and target, which it alone knows; and Complete is the only way to get
// from one to the other, so no field can arrive at the log empty because
// a caller forgot it. The draft is returned rather than appended, which
// is what keeps any logging call out of the gap between the last sample
// and the removal.
type BreakDraft struct {
	// Path is the lock directory, as the hold spells it.
	Path string
	// StoreDir is the store directory it guards.
	StoreDir string
	// Tree is which tree that store is in.
	Tree Tree
	// SampleA is the first sample.
	SampleA LockSample
	// SampleB is the sample one interval later, when the rule got that
	// far.
	SampleB *LockSample
	// SampleC is the sample taken immediately before the removal.
	SampleC *LockSample
	// IntervalWallMS is how much wall-clock time passed between A and B.
	IntervalWallMS uint64
	// IntervalMonotonicMS is how much monotonic time passed between the
	// same two samples. The two are compared, which is what catches a
	// clock step in either direction.
	IntervalMonotonicMS uint64
	// Evidence is what the holder check found.
	Evidence HolderEvidence
	// Outcome says whether the directory was removed.
	Outcome BreakOutcome
	// Reason says why not, when it was not — and retaken when it was
	// removed and immediately taken by somebody else. Empty for a clean
	// break.
	Reason BreakReason
}

// Complete fills in the caller's own two fields and hands back the record
// to append to the audit log.
//
// The service is the keychain item the hold was for and the target says
// which item that is; both belong to the caller because the rule is given
// a lock artefact and nothing else.
func (d *BreakDraft) Complete(service string, target Target) *LockBreakRecord {
	return &LockBreakRecord{
		Path:                d.Path,
		StoreDir:            d.StoreDir,
		Tree:                d.Tree,
		Service:             service,
		Target:              target,
		SampleA:             d.SampleA,
		SampleB:             d.SampleB,
		SampleC:             d.SampleC,
		IntervalWallMS:      d.IntervalWallMS,
		IntervalMonotonicMS: d.IntervalMonotonicMS,
		Evidence:            d.Evidence,
		Outcome:             d.Outcome,
		Reason:              d.Reason,
	}
}

// LockBreakRecord is the record of one break decision, as the audit log
// carries it.
//
// Only the lock protocol builds one, and it cannot build one in halves:
// the rule fills everything it can observe and the caller supplies the
// service and target through [BreakDraft.Complete], so no field arrives
// at the log empty because somebody forgot it.
type LockBreakRecord struct {
	// Path is the lock directory itself.
	Path string `json:"path"`
	// StoreDir is the credential store directory it guards.
	StoreDir string `json:"store_dir"`
	// Tree is which tree that store is in.
	Tree Tree `json:"tree"`
	// Service is the keychain service name of the item the store holds.
	Service string `json:"service"`
	// Target says which item the hold was for.
	Target Target `json:"target"`
	// SampleA is the first stat.
	SampleA LockSample `json:"sample_a"`
	// SampleB is the second stat, after the sampling interval. Nil when
	// the rule abandoned before it.
	SampleB *LockSample `json:"sample_b"`
	// SampleC is the third stat, immediately before the removal, with no
	// I/O between.
	SampleC *LockSample `json:"sample_c"`
	// IntervalWallMS is the wall-clock milliseconds between samples A
	// and B.
	IntervalWallMS uint64 `json:"interval_wall_ms"`
	// IntervalMonotonicMS is the monotonic milliseconds between the same
	// two samples. A divergence from IntervalWallMS over a second is a
	// clock step in either direction.
	IntervalMonotonicMS uint64 `json:"interval_monotonic_ms"`
	// Evidence is what the holder check saw.
	Evidence HolderEvidence `json:"holder_evidence"`
	// Outcome says whether the artefact was removed.
	Outcome BreakOutcome `json:"outcome"`
	// Reason says why not, when it was not — and retaken when it was
	// removed and a peer took it back first. Absent for a clean break:
	// every reason is a reason not to have broken a lock, so a break
	// with nothing to explain records no reason rather than inventing a
	// word for success.
	Reason BreakReason `json:"reason,omitzero"`
}
