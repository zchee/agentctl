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
	"fmt"
	"time"
)

// Claude Code makes a lock by mkdir and releases it by rmdir, refreshing
// the directory's modification time every PeerHeartbeat and treating a
// lock older than its staleness window as abandoned. A credential-store
// hold is three such directories in the peer's own nesting: the primary
// refresh lock inside the store directory, the legacy lock beside the
// store's resolved spelling, and the storage-write mutex innermost.
//
// Two properties make holding three of a peer's locks survivable, and
// both are structural: all staleness is resolved before the first
// successful mkdir (a sampling wait inside a hold would outlast the
// peer's own 4000 ms give-up), and the storage-write mutex is taken with
// one non-blocking mkdir, because its owner's retry ladder is a ~7.5 s
// affair that must never enter the hold.

// PeerHeartbeat is how often a live Claude Code holder refreshes a lock
// directory's modification time.
//
// No heartbeat runs on this side: a hold bounded by HoldBudget can never
// reach the heartbeat period, so one could only ever be dead code
// pretending to be a safety net. The number still governs
// StaleSampleInterval's margin, which is why it stays a named constant.
const PeerHeartbeat = 5 * time.Second

// LegacyLockSuffix is the legacy lock's suffix: the peer creates
// `<resolved store dir>.lock` beside the store directory rather than
// inside it.
const LegacyLockSuffix = ".lock"

// ContentionRounds is how many rounds the contention schedule waits for a
// held primary lock before it decides.
const ContentionRounds = 5

// ContentionRoundBase is the fixed part of one contention round.
const ContentionRoundBase = 1 * time.Second

// ContentionRoundJitter is the span of the random part of one contention
// round, so each round waits base plus a uniform draw below this.
const ContentionRoundJitter = 1 * time.Second

// ContentionFloor is the least total wait the contention schedule tops
// itself up to before deciding whether a holder is beating.
const ContentionFloor = 7500 * time.Millisecond

// StaleSampleInterval is how long the break rule waits between its first
// and second samples of a stale lock's modification time.
//
// It must stay greater than 2 × PeerHeartbeat: 12 s is 2.4 heartbeat
// periods, which is the margin that makes a single heartbeat landing
// anywhere in the window fail the modification-time comparison. Never
// reduce it toward 10 s.
const StaleSampleInterval = 12 * time.Second

// ClockSkewTolerance is how far the wall clock and the monotonic clock
// may disagree across the sampling interval before a break is abandoned.
//
// It catches a step in either direction: a backward jump makes the two
// deltas differ by their sum, and a forward jump inflates both ages and
// would otherwise pass undiagnosed.
const ClockSkewTolerance = 1 * time.Second

// HoldBudget is the longest a credential-store hold may last, from the
// first mkdir to the last rmdir.
//
// Derived, not chosen: the peer's scope-expansion helper gives up on its
// own refresh after 4000 ms at the floor, so the budget is 4000 ms minus
// 1000 ms of margin.
const HoldBudget = 3000 * time.Millisecond

// ConfigHoldBudget is the configuration lock's hold budget.
//
// A different derivation from a different peer behaviour: a session
// inside its first 30 s abandons its own acquire ladder after 1500 ms
// and then writes the whole configuration document unlocked, which would
// erase a swap. 1200 ms leaves 300 ms under that floor.
const ConfigHoldBudget = 1200 * time.Millisecond

// MaxRestarts is how many times an EEXIST may send an acquisition back to
// the lock-free probe before it reports the store busy.
const MaxRestarts = 3

// LockProfile is one lock family's timing options.
//
// Two families exist and they do not share options: the credential-store
// locks, and the configuration lock. Keeping them in one type is what
// stops a caller borrowing the wrong staleness window for the wrong lock.
type LockProfile struct {
	// Stale is how old the peer lets a lock get before treating it as
	// abandoned.
	Stale time.Duration
	// Update is the peer's heartbeat period for this family.
	Update time.Duration
	// Retries is how many times a blocked mkdir is retried. Always zero:
	// a retry is a wait, and a wait belongs outside the hold.
	Retries int
	// HoldBudget is the longest a hold of this family may last.
	HoldBudget time.Duration
}

// RefreshProfile is the primary and legacy refresh locks' profile.
var RefreshProfile = LockProfile{
	Stale:      60 * time.Second,
	Update:     PeerHeartbeat,
	Retries:    0,
	HoldBudget: HoldBudget,
}

// StorageWriteProfile is the storage-write mutex's profile, taken with a
// single non-blocking mkdir.
var StorageWriteProfile = LockProfile{
	Stale:      15 * time.Second,
	Update:     PeerHeartbeat,
	Retries:    0,
	HoldBudget: HoldBudget,
}

// ConfigProfile is the configuration lock's profile.
var ConfigProfile = LockProfile{
	Stale:      10 * time.Second,
	Update:     PeerHeartbeat,
	Retries:    0,
	HoldBudget: ConfigHoldBudget,
}

// CompromisedError reports that a lock held by this process had its
// modification time moved under it, so the protocol has already been
// violated and nothing may be written.
type CompromisedError struct {
	// Path is the lock directory that moved.
	Path string
}

// Error implements the error interface.
func (e *CompromisedError) Error() string {
	return fmt.Sprintf("`%s` was modified while this process held it; the lock is compromised", e.Path)
}

// BudgetExceededError reports that a hold outlived its profile's budget,
// which would make the peer's own refresh give up and fail.
type BudgetExceededError struct {
	// Elapsed is how long the hold had lasted when it was noticed.
	Elapsed time.Duration
	// Budget is the profile's budget.
	Budget time.Duration
}

// Error implements the error interface.
func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("the hold reached %d ms, past the %d ms budget", e.Elapsed.Milliseconds(), e.Budget.Milliseconds())
}

// WrongTreeError reports that the store directory is not in the tree the
// caller named, so the hold — and any break inside it — would land
// somewhere the containment rule does not allow.
type WrongTreeError struct {
	// StoreDir is the store directory as the caller spelled it.
	StoreDir string
	// Tree is the tree the caller claimed it was in.
	Tree Tree
}

// Error implements the error interface.
func (e *WrongTreeError) Error() string {
	return fmt.Sprintf("`%s` is not %s, so it will not be locked as one", e.StoreDir, e.Tree.Label())
}

// UnreachableError reports that the way from the permitted anchor down to
// a directory could not be walked without following a symbolic link, or
// that a component of it is missing or is not a directory.
type UnreachableError struct {
	// Path is the directory the walk was heading for.
	Path string
	// Message says what the walk refused, in its own words.
	Message string
}

// Error implements the error interface.
func (e *UnreachableError) Error() string {
	return fmt.Sprintf("`%s` cannot be locked: %s", e.Path, e.Message)
}

// HolderUnreadableError reports that visibility of store-sharing peers is
// unproved, so no stale directory may be removed.
type HolderUnreadableError struct{}

// Error implements the error interface.
func (*HolderUnreadableError) Error() string {
	return "holder unreadable: no stopped peer visible by exact name; peer visibility is unproved"
}

// LockIOError reports that a filesystem operation on a lock failed for a
// reason that is not contention.
type LockIOError struct {
	// Context says what was being attempted.
	Context string
	// Message is the underlying failure.
	Message string
}

// Error implements the error interface.
func (e *LockIOError) Error() string {
	return e.Context + ": " + e.Message
}
