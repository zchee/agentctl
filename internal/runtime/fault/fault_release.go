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

//go:build !agentctl_testing

package fault

// Empty names prevent untagged callers from retaining fault strings in
// release artifacts; Active never activates any of them.
const (
	// BeforeInvalidGrantReread is inert in a release build.
	BeforeInvalidGrantReread = ""
	// BeforeMigratedReread is inert in a release build.
	BeforeMigratedReread = ""
	// BeforeMigratedWrite is inert in a release build.
	BeforeMigratedWrite = ""
	// BeforeRefreshRecheck is inert in a release build.
	BeforeRefreshRecheck = ""
	// BeforeSwapWrite is inert in a release build.
	BeforeSwapWrite = ""
	// CodexAbortAfterMarker is inert in a release build.
	CodexAbortAfterMarker = ""
	// CodexAbortAfterPending is inert in a release build.
	CodexAbortAfterPending = ""
	// CodexAfterPostSnapshot is inert in a release build.
	CodexAfterPostSnapshot = ""
	// CodexBeforePostSnapshot is inert in a release build.
	CodexBeforePostSnapshot = ""
	// CodexErrorAfterRename is inert in a release build.
	CodexErrorAfterRename = ""
	// CodexLoginAfterWrite is inert in a release build.
	CodexLoginAfterWrite = ""
	// CodexLoginBeforeInstall is inert in a release build.
	CodexLoginBeforeInstall = ""
	// CodexRefreshStateBeforeRename is inert in a release build.
	CodexRefreshStateBeforeRename = ""
	// CodexRefreshStateDirSync is inert in a release build.
	CodexRefreshStateDirSync = ""
	// CodexRefreshStateFileSync is inert in a release build.
	CodexRefreshStateFileSync = ""
	// CodexRefreshStateRename is inert in a release build.
	CodexRefreshStateRename = ""
	// CodexRefreshStateWrite is inert in a release build.
	CodexRefreshStateWrite = ""
	// LockContended is inert in a release build.
	LockContended = ""
	// LockResumeAfterSampleB is inert in a release build.
	LockResumeAfterSampleB = ""
	// LockStale is inert in a release build.
	LockStale = ""
	// SwapLockLeak is inert in a release build.
	SwapLockLeak = ""
	// SwapNamespaceAcquired is inert in a release build.
	SwapNamespaceAcquired = ""
	// SwapPauseInLocks is inert in a release build.
	SwapPauseInLocks = ""
	// SwapWriteFail is inert in a release build.
	SwapWriteFail = ""
	// BeforeRename is inert in a release build.
	BeforeRename = ""
	// RenameFail is inert in a release build.
	RenameFail = ""
	// CodexBeforeRename is inert in a release build.
	CodexBeforeRename = ""
	// CodexInstallRenameFail is inert in a release build.
	CodexInstallRenameFail = ""
	// CodexRenameFail is inert in a release build.
	CodexRenameFail = ""
)

// Active is the release factory: nothing is ever injected, and no
// environment variable is read, so a release binary carries neither the
// fault names nor a way to wait at one.
func Active() Fault {
	return None()
}

// PausePoint is inert in a release build: every pause compiles to an
// immediate return.
func (f Fault) PausePoint(name string) {}

// WaitIf is inert in a release build.
func (f Fault) WaitIf(name string) {}
