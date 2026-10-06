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

//go:build agentctl_testing

package fault

import (
	"os"
	"strings"
	"time"
)

// Fault names live only in tagged builds so callers cannot retain
// their strings in release artifacts.
const (
	// BeforeInvalidGrantReread pauses before rereading a rejected grant.
	BeforeInvalidGrantReread = "before_invalid_grant_reread"
	// BeforeMigratedReread pauses before rereading a migrated credential.
	BeforeMigratedReread = "before_migrated_reread"
	// BeforeMigratedWrite pauses before writing a migrated credential.
	BeforeMigratedWrite = "before_migrated_write"
	// BeforeRefreshRecheck pauses before checking a refreshed file's identity.
	BeforeRefreshRecheck = "before_refresh_recheck"
	// BeforeSwapWrite waits before entering a swap's write phase.
	BeforeSwapWrite = "before_swap_write"
	// CodexAbortAfterMarker aborts after persisting the refresh marker.
	CodexAbortAfterMarker = "codex_abort_after_marker"
	// CodexAbortAfterPending aborts after parking a refreshed credential.
	CodexAbortAfterPending = "codex_abort_after_pending"
	// CodexAfterPostSnapshot pauses after recording the refresh marker.
	CodexAfterPostSnapshot = "codex_after_post_snapshot"
	// CodexBeforePostSnapshot pauses before taking the refresh snapshot.
	CodexBeforePostSnapshot = "codex_before_post_snapshot"
	// CodexErrorAfterRename fails after replacing the refreshed credential.
	CodexErrorAfterRename = "codex_error_after_rename"
	// CodexLoginAfterWrite stops a login before its registry update.
	CodexLoginAfterWrite = "codex_login_after_write"
	// CodexLoginBeforeInstall pauses before installing a verified login.
	CodexLoginBeforeInstall = "codex_login_before_install"
	// CodexRefreshStateBeforeRename pauses before replacing refresh state.
	CodexRefreshStateBeforeRename = "codex_refresh_state_before_rename"
	// CodexRefreshStateDirSync fails the refresh-state directory flush.
	CodexRefreshStateDirSync = "codex_refresh_state_dir_sync"
	// CodexRefreshStateFileSync fails the refresh-state file flush.
	CodexRefreshStateFileSync = "codex_refresh_state_file_sync"
	// CodexRefreshStateRename fails the refresh-state rename.
	CodexRefreshStateRename = "codex_refresh_state_rename"
	// CodexRefreshStateWrite fails the refresh-state temporary write.
	CodexRefreshStateWrite = "codex_refresh_state_write"
	// LockContended makes the primary lock's first mkdir report a holder.
	LockContended = "lock_contended"
	// LockResumeAfterSampleB touches the lock between the last two samples.
	LockResumeAfterSampleB = "lock_resume_after_sample_b"
	// LockStale makes a fresh lock eligible for the break rule.
	LockStale = "lock_stale"
	// SwapLockLeak leaves the lock directories and record after release.
	SwapLockLeak = "swap_lock_leak"
	// SwapNamespaceAcquired pauses after acquiring a swap namespace lock.
	SwapNamespaceAcquired = "swap_namespace_acquired"
	// SwapPauseInLocks waits while holding the swap's peer locks.
	SwapPauseInLocks = "swap_pause_in_locks"
	// SwapWriteFail fails the swap's keychain write.
	SwapWriteFail = "swap_write_fail"
	// BeforeRename pauses before replacing a credential file.
	BeforeRename = "before_rename"
	// RenameFail forces a credential file rename to fail.
	RenameFail = "rename_fail"
	// CodexBeforeRename pauses before replacing a Codex credential file.
	CodexBeforeRename = "codex_before_rename"
	// CodexInstallRenameFail forces a Codex login install rename to fail.
	CodexInstallRenameFail = "codex_install_rename_fail"
	// CodexRenameFail forces a Codex refreshed credential rename to fail.
	CodexRenameFail = "codex_rename_fail"
)

// The seam variables a tagged build reads its fault set through. Neither
// name exists in a release build.
const (
	// faultEnv carries the active fault names, comma separated.
	faultEnv = "AGENTCTL_FAULT"
	// faultResumeEnv names the file whose appearance releases a pause.
	faultResumeEnv = "AGENTCTL_FAULT_RESUME"
)

const (
	// pauseBudget is how long a pause waits before giving up on its
	// resume file, so a test that crashes without writing one cannot
	// wedge a run.
	pauseBudget = 10 * time.Second
	// pausePollInterval is how often a pause re-checks for its resume
	// file.
	pausePollInterval = 20 * time.Millisecond
)

// Active is the tagged factory: it reads the fault set from the
// environment. Names are separated by commas; surrounding whitespace is
// trimmed and empty entries are dropped, so "rename_fail, hold_lock" and
// "rename_fail,hold_lock" mean the same thing. An unset or empty variable
// yields the same value as None.
func Active() Fault {
	return FromList(os.Getenv(faultEnv))
}

// FromList parses a comma-separated fault list.
func FromList(raw string) Fault {
	names := make(map[string]struct{})
	for name := range strings.SplitSeq(raw, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			names[name] = struct{}{}
		}
	}
	if len(names) == 0 {
		return None()
	}
	return Fault{names: names}
}

// PausePoint blocks at a named pause point when "pause_" plus name is
// active.
//
// The wait ends when the resume file exists, or after the pause budget.
// It exists so a test can interleave with a window that is otherwise a
// few microseconds wide — the moment between an exchange returning and
// the new credentials being renamed into place.
//
// Observable: before it waits, an active pause creates a ".reached"
// marker next to the resume file the test already owns. A test that acts
// inside the window can therefore wait for proof that the run is actually
// held here, rather than inferring it from some other side effect —
// without the marker, deleting a pause leaves every test that relies on
// it green while the window it guarded closes by timing alone. Additive:
// a caller that never looks for the marker is unaffected.
func (f Fault) PausePoint(name string) {
	active := "pause_" + name
	if f.Is(active) {
		if resume := os.Getenv(faultResumeEnv); resume != "" {
			// Best effort: a marker that cannot be written makes the
			// waiting test fail loudly on its missing marker, which is
			// the right outcome; it must not stop the pause itself.
			_ = os.WriteFile(resume+".reached", nil, 0o600)
		}
	}
	f.WaitIf(active)
}

// WaitIf blocks while name — the whole fault name, not a pause stem — is
// active.
//
// PausePoint is this function with the pause prefix applied, and is what
// almost every waiting injection should use: the prefix is what makes a
// fault list readable as "these ones stop, those ones break". This is the
// escape hatch for a declared name that does not carry it. The wait ends
// when the resume file exists, or after the pause budget, so a test that
// dies without writing one cannot wedge a run.
func (f Fault) WaitIf(name string) {
	if !f.Is(name) {
		return
	}
	resume := os.Getenv(faultResumeEnv)
	start := time.Now()
	for {
		if resume != "" {
			if _, err := os.Stat(resume); err == nil {
				return
			}
		}
		if time.Since(start) >= pauseBudget {
			return
		}
		time.Sleep(pausePollInterval)
	}
}
