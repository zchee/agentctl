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

package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
)

type useWritePhase struct {
	paths            *config.Paths
	env              *claude.EnvView
	subject          *useSubject
	log              *os.File
	before           *claude.Digests
	after            claude.Digests
	fromDigest8      *string
	toDigest8        string
	adoptedTo        *string
	staged           *secret.StagedAdoption
	direction        secret.WriteDirection
	shadowingStore   bool
	restoredAdopted  string
	incomingIdentity *secret.IncomingIdentity
}

// useWrite takes peer locks only after all network and consent work is finished.
func useWrite(ctx context.Context, input useWritePhase, line *secret.KeychainStdinLine) *useReport {
	report := &useReport{service: input.subject.service, fromDigest8: input.fromDigest8, toDigest8: new(input.toDigest8), adoptedTo: input.adoptedTo, lock: useLockReport{BudgetMS: new(uint64(secret.HoldBudget.Milliseconds()))}}
	if input.staged != nil {
		defer input.staged.Discard()
	}
	injected := fault.Active()
	seams := secret.RealSeams(secret.SystemClock())
	seams.Fault = injected.Is
	var liveEnv *secret.LiveStoreEnv
	if input.subject.tree == secret.TreeLive {
		liveEnv = &secret.LiveStoreEnv{SecureStorageEnvName: claude.SecureStorageEnv, NamedStoreDir: claude.LiveStoreDir(input.env)}
		if input.env.SecureStorageDir != nil {
			liveEnv.SecureStorageDir = *input.env.SecureStorageDir
		}
	}
	acquisition, err := secret.AcquirePeerLocks(ctx, secret.LockSubject{StoreDir: input.subject.storeDir, Tree: input.subject.tree}, input.paths, liveEnv, seams)
	var draft *secret.BreakDraft
	if acquisition != nil {
		draft = acquisition.BreakRecord
	}
	if failure, ok := errors.AsType[*secret.AcquireError](err); ok {
		draft = failure.BreakRecord
	}
	// A failed acquisition may already have removed a stale lock.
	if draft != nil {
		record := draft.Complete(input.subject.service, input.subject.audit)
		var reason *secret.BreakReason
		if record.Reason != "" {
			reason = &record.Reason
		}
		report.lock.Break = &useBreakReport{Broke: record.Outcome == secret.OutcomeBroken, Outcome: record.Outcome, Reason: reason, HolderEvidence: record.Evidence}
		useAppendAudit(ctx, input.paths, input.log, record)
	}
	end := func(kind claude.SwapOutcomeKind, refusal claude.SwapRefusalKind, note string) *useReport {
		report.outcome = claude.SwapOutcome{Kind: kind, Refusal: claude.SwapRefusal{Kind: refusal}}
		report.note = &note
		return report
	}
	if err != nil {
		if _, unreachable := errors.AsType[*secret.UnreachableError](err); unreachable && input.subject.tree == secret.TreeLive {
			return end(claude.SwapRefused, claude.SwapLiveUnreachable, "the live store could not be resolved: "+err.Error())
		}
		return end(claude.SwapRefused, claude.SwapCompromisedHold, "the Claude Code locks could not be taken: "+err.Error())
	}
	if acquisition.Busy() {
		return end(claude.SwapBusy, 0, claude.SwapBusyNote(acquisition.HolderAlive, acquisition.StoppedPIDs))
	}
	hold := acquisition.Held
	defer hold.Release()
	heldEnd := func(kind claude.SwapOutcomeKind, refusal claude.SwapRefusalKind, note string) *useReport {
		report.lock.HoldMS = new(uint64(max(hold.HoldElapsed().Milliseconds(), 0)))
		return end(kind, refusal, note)
	}
	if err := hold.DriftCheck(); err != nil {
		return heldEnd(claude.SwapRefused, claude.SwapCompromisedHold, "the lock agentctl holds is compromised: "+err.Error())
	}
	reader := secret.NewReader()
	readCtx, cancel := context.WithTimeout(ctx, secret.KeychainVerifyTimeout)
	current, err := useReadKeychain(readCtx, reader, input.subject.service)
	cancel()
	if err != nil {
		if errors.Is(err, secret.ErrKeychainLocked) {
			return heldEnd(claude.SwapDiscarded, 0, "the keychain locked before the item could be re-read under the hold")
		}
		return heldEnd(claude.SwapDiscarded, 0, "the item could not be re-read under the hold ("+err.Error()+")")
	}
	var observed *claude.Digests
	if current != nil {
		digest, err := current.Digests()
		if err != nil {
			return heldEnd(claude.SwapDiscarded, 0, "the item could not be fingerprinted under the hold")
		}
		observed = &digest
	}
	if (observed == nil) != (input.before == nil) || observed != nil && *observed != *input.before {
		return heldEnd(claude.SwapDiscarded, 0, "the item changed under the hold, so the swap was thrown away rather than written over a newer credential")
	}
	injected.WaitIf("swap_pause_in_locks")
	if err := hold.DriftCheck(); err != nil {
		return heldEnd(claude.SwapRefused, claude.SwapCompromisedHold, "the lock agentctl holds is compromised: "+err.Error())
	}
	if hold.HoldElapsed()+secret.KeychainWriteTimeout > secret.HoldBudget {
		return heldEnd(claude.SwapDiscarded, 0, "the hold ran out of budget before the write")
	}
	if err := hold.WriteAdmission(); err != nil {
		return heldEnd(claude.SwapRefused, claude.SwapCompromisedHold, "the lock agentctl holds is compromised: "+err.Error())
	}
	var writeErr error
	if injected.Is("swap_write_fail") {
		writeErr = errors.New("the write was refused by the test fault switch")
	} else {
		writeErr = secret.NewKeychainWriter().Write(ctx, input.subject.service, line)
	}
	elapsed := hold.HoldElapsed()
	report.lock.HoldMS = new(uint64(max(elapsed.Milliseconds(), 0)))
	hold.Release()
	if elapsed > secret.HoldBudget {
		slog.WarnContext(ctx, "the credential-store hold outlasted its budget", "hold_ms", elapsed.Milliseconds(), "budget_ms", secret.HoldBudget.Milliseconds())
	}
	audit := secret.WriteEvent{Target: input.subject.audit, FromDigest8: input.fromDigest8, ToDigest8: input.toDigest8, Direction: input.direction, IncomingIdentity: input.incomingIdentity}
	if writeErr != nil && !errors.Is(writeErr, secret.ErrKeychainTimeout) {
		audit.Outcome = secret.WriteFailed
		report.auditID = useAppendAudit(ctx, input.paths, input.log, &audit)
		recovery := "The outgoing credential is still recoverable with `agentctl claude use --undo`"
		if input.direction == secret.DirectionUndo {
			recovery = "The credential this rollback was restoring is untouched in the adopted copy; nothing was lost and the rollback can be run again"
			if input.subject.tree == secret.TreeLive {
				recovery = "The credential this rollback was restoring is untouched where the swap being undone filed it, inside agentctl's own namespaces; nothing was lost and the rollback can be run again"
			}
		}
		return end(claude.SwapFailed, 0, "the credential could not be stored: "+writeErr.Error()+". "+recovery)
	}
	// Readback is outside the hold: a peer winning this gap makes the result unknown.
	readCtx, cancel = context.WithTimeout(ctx, secret.KeychainVerifyTimeout)
	verified, verifyErr := useReadKeychain(readCtx, reader, input.subject.service)
	cancel()
	applied := false
	if verifyErr == nil && verified != nil {
		digests, err := verified.Digests()
		applied = err == nil && digests == input.after
	}
	audit.Outcome = secret.WriteUnknown
	report.outcome.Kind = claude.SwapUnknown
	if applied {
		audit.Outcome = secret.WriteApplied
		report.outcome.Kind = claude.SwapApplied
	}
	report.auditID = useAppendAudit(ctx, input.paths, input.log, &audit)
	if input.staged != nil {
		if applied {
			if _, err := secret.CommitStaged(input.paths, input.staged); err != nil {
				report.note = new("the swap applied, but the credential it displaced could not be parked in the adopted copy (" + err.Error() + "); it is still in its own namespace store")
			} else {
				report.adoptedTo = new(secret.AdoptedFile)
			}
		} else {
			report.note = new("the write could not be confirmed; re-run `agentctl claude status`. The credential this rollback was restoring is untouched in the adopted copy, and the one it displaced was deliberately not parked there — so this reversal cannot itself be undone; that credential was not parked; its own store may still hold a usable copy")
		}
	}
	// Never remove live plaintext, or a namespace's possible sole copy on unknown.
	if input.shadowingStore && input.subject.tree == secret.TreeOwn {
		sentence := ""
		if applied {
			if _, err := secret.RemoveCredentialsFile(input.paths, input.subject.storeDir); err != nil {
				sentence = fmt.Sprintf("the swap applied, but `%s` still holds the credential it displaced and Claude Code reads that file whenever the keychain is unavailable; remove it by hand (%v)", secret.CredentialsFile, err)
			}
		} else {
			sentence = fmt.Sprintf("the write could not be confirmed, so `%s` was kept and still holds the credential this swap displaced. The item may already hold the incoming one: run `agentctl claude status` to see which, and if it does, remove that file by hand — Claude Code reads it whenever the keychain is unavailable. `agentctl claude doctor` reports it until then", secret.CredentialsFile)
		}
		useJoinNote(report, sentence)
	}
	if applied && input.restoredAdopted != "" {
		if err := useRemoveDuplicateAdopted(input.paths, input.restoredAdopted, input.after); err != nil {
			useJoinNote(report, fmt.Sprintf("the swap applied, but the duplicate adopted copy `%s` could not be removed (%v)", filepath.Join(input.restoredAdopted, secret.AdoptedFile), err))
		}
	}
	if report.note == nil && !applied {
		report.note = new("the write could not be confirmed; re-run `agentctl claude status`")
	}
	return report
}

func useReadKeychain(ctx context.Context, reader secret.Reader, service string) (*claude.Credentials, error) {
	sealed, err := reader.Read(ctx, service)
	if errors.Is(err, secret.ErrItemNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var credentials *claude.Credentials
	err = sealed.WithPlaintext(func(blob []byte) error {
		var parseErr error
		credentials, parseErr = claude.ParseBlob(blob)
		return parseErr
	})
	return credentials, err
}

func useRemoveDuplicateAdopted(paths *config.Paths, dir string, restored claude.Digests) error {
	for _, read := range []func(string) (secret.ReadOutcome, error){secret.ReadCredentials, secret.ReadAdopted} {
		item, err := read(dir)
		if err != nil || !item.Present {
			return nil
		}
		credentials, err := claude.ParseBlob(item.Bytes)
		memguard.WipeBytes(item.Bytes)
		if err != nil {
			return nil
		}
		digests, err := credentials.Digests()
		if err != nil || digests != restored {
			return nil
		}
	}
	_, err := secret.RemoveAdoptedFile(paths, dir)
	return err
}

func useJoinNote(report *useReport, sentence string) {
	if sentence == "" {
		return
	}
	if report.note == nil {
		report.note = &sentence
		return
	}
	report.note = new(*report.note + ". " + sentence)
}
