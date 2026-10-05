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

package cli

// Exit codes for the refusals and outcomes of a `claude use` credential
// swap. Each one gets its own message and its own exit code, so a script
// can act on one without parsing English.
//
// The block starts at 10 because 0, 1 and 2 are taken: 0 is a clean run, 1
// is a fatal failure, 2 is a run whose output holds at least one degraded
// row — and a command-line usage error also exits 2, so a swap code of 2
// would be ambiguous between "the item changed under the hold" and "you
// misspelled a flag". Codes 25, 26 and 28 were assigned once, retired, and
// are never defined again: a script that learned one of them must not see
// it come back meaning something else.
//
// Refusal B — a secure-storage backend is active or of unknown kind — has
// no code here on purpose: it is a warning line and exit 0, because no such
// backend exists in this build.
const (
	// SwapExitRefusedA is refusal A: the lock held for the swap was
	// compromised — its mtime moved underneath the process, or the held
	// mtime could not be read — or acquisition failed for a reason other
	// than the store being unreachable, so nothing may be written.
	SwapExitRefusedA = 10

	// SwapExitRefusedC is refusal C: CLAUDE_CODE_OAUTH_TOKEN holds a
	// non-empty value in the process's own environment, which
	// short-circuits every credential store.
	SwapExitRefusedC = 11

	// SwapExitRefusedD is refusal D: the encoded credential line,
	// including its newline, does not fit the 4032-byte keychain stdin
	// bound.
	SwapExitRefusedD = 12

	// SwapExitRefusedE is refusal E: CLAUDE_SECURESTORAGE_CONFIG_DIR
	// holds a non-empty value, so this shell names a namespace rather
	// than the live store — and the pass was asked for the live one.
	// Reachable only through an undo of a live-target swap, whose target
	// comes from the audit log and can disagree with the environment.
	SwapExitRefusedE = 13

	// SwapExitRefusedF is refusal F: the outgoing credential cannot be
	// adopted safely, so the swap would lose it.
	SwapExitRefusedF = 14

	// SwapExitPrecondition means the inherited
	// CLAUDE_SECURESTORAGE_CONFIG_DIR names no store this registry owns.
	// Not a lettered refusal: it is decided before the swap begins, so
	// the JSON report carries reason "not_owned" and no refusal member.
	SwapExitPrecondition = 15

	// SwapExitBusy means another process holds the store's Claude Code
	// locks and they were not broken.
	SwapExitBusy = 16

	// SwapExitDiscarded means the item changed or became unreadable under
	// the hold, or the remaining hold budget was too short to write, so
	// the refreshed credential was thrown away rather than written over a
	// newer one.
	SwapExitDiscarded = 17

	// SwapExitUnknown means the write timed out and the verifying read
	// did not settle whether it landed. It means "re-run status", not
	// "failed".
	SwapExitUnknown = 18

	// SwapExitWriteFailed means the write ran and failed definitely, for
	// a reason other than a timeout, and the item is demonstrably
	// untouched. The JSON report carries outcome "failed" and no refusal
	// member: an ordinary write failure is not a security signal.
	SwapExitWriteFailed = 19

	// SwapExitCancelled means nobody agreed to the swap: the confirmation
	// was declined, or there was no terminal to ask at and --yes was not
	// given. --json does not imply --yes. The JSON report carries outcome
	// "cancelled" and no refusal member.
	SwapExitCancelled = 20

	// SwapExitNeedsRefresh means the incoming account's credential has
	// expired and its store has migrated into the keychain, so the swap
	// will not refresh it: the refresh could not be saved back, and a
	// spent refresh token would strand the account. The JSON report
	// carries outcome "needs_refresh" and no refusal member.
	SwapExitNeedsRefresh = 21

	// SwapExitAuditRefused means the append-only audit log cannot be
	// written, so a live-store swap is refused rather than performed
	// unrecorded. The JSON report carries outcome "refused" with reason
	// "audit_refused" and no refusal member.
	SwapExitAuditRefused = 22

	// SwapExitLiveUnreachable means the live store could not be resolved:
	// nothing is at the path the environment names, or the symbolic link
	// there dangles. The JSON report carries reason "live_unreachable"
	// and no refusal member.
	SwapExitLiveUnreachable = 23

	// SwapExitLiveItemAbsent means the live keychain item is absent, so
	// the live store has not migrated and its credential is still in a
	// plaintext file no swap may touch. Transient and self-healing; the
	// JSON report carries reason "live_item_absent" and no refusal
	// member.
	SwapExitLiveItemAbsent = 24

	// SwapExitLiveUndoItemChanged means an undo found the live item
	// holding a third account's credential — neither the one the undo
	// would put back nor the one the swap being undone installed. The
	// JSON report carries reason "live_undo_foreign_login" and no refusal
	// member.
	SwapExitLiveUndoItemChanged = 27

	// SwapExitIdentityUnavailable means nothing can say whose credential
	// the live item holds: the profile request did not answer, or the
	// item's access token has expired without an owned audit attribution.
	// Decided before any lock, prompt or write, so nothing is written.
	SwapExitIdentityUnavailable = 29

	// SwapExitRCNotDisconnected means Remote Control could not be
	// disconnected before a live swap: the preflight found an unsupported
	// platform, no TTY to attest at, or a session that stayed connected.
	// Nothing is written.
	SwapExitRCNotDisconnected = 30
)

// SwapExitCodes pairs every swap exit name with its code, for the
// exhaustiveness and uniqueness checks; nothing in the shipped binary reads
// the table as a table.
var SwapExitCodes = [...]struct {
	Name string
	Code int
}{
	{Name: "refused_a", Code: SwapExitRefusedA},
	{Name: "refused_c", Code: SwapExitRefusedC},
	{Name: "refused_d", Code: SwapExitRefusedD},
	{Name: "refused_e", Code: SwapExitRefusedE},
	{Name: "refused_f", Code: SwapExitRefusedF},
	{Name: "precondition", Code: SwapExitPrecondition},
	{Name: "busy", Code: SwapExitBusy},
	{Name: "discarded", Code: SwapExitDiscarded},
	{Name: "unknown", Code: SwapExitUnknown},
	{Name: "write_failed", Code: SwapExitWriteFailed},
	{Name: "cancelled", Code: SwapExitCancelled},
	{Name: "needs_refresh", Code: SwapExitNeedsRefresh},
	{Name: "audit_refused", Code: SwapExitAuditRefused},
	{Name: "live_unreachable", Code: SwapExitLiveUnreachable},
	{Name: "live_item_absent", Code: SwapExitLiveItemAbsent},
	{Name: "live_undo_item_changed", Code: SwapExitLiveUndoItemChanged},
	{Name: "identity_unavailable", Code: SwapExitIdentityUnavailable},
	{Name: "remote_control_not_disconnected", Code: SwapExitRCNotDisconnected},
}
