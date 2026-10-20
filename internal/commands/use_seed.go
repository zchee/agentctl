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
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
)

// SeedPreHoldBudget bounds every keychain call a seed makes before it takes
// the namespace's lock: the preflight, the listing, the confirming read and
// the live read share it, so a slow keychain costs the launch this much and
// no more.
const SeedPreHoldBudget = 3000 * time.Millisecond

// SeedLockWait bounds the wait for the namespace's peer lock before the
// write. The acquisition would otherwise wait out a held lock on the peer's
// contention schedule, or sample a stale one, for many seconds before the
// session starts.
const SeedLockWait = 500 * time.Millisecond

// SeedFileRemoveWait bounds the wait for the namespace guard and the peer
// lock together before the owned credential file is removed; the default
// namespace lock wait is too long to spend before a launch.
const SeedFileRemoveWait = 500 * time.Millisecond

// seedOutcome is how a seed ended: the `outcome` word of `mcp_seed`.
type seedOutcome string

const (
	seedSeeded    seedOutcome = "seeded"
	seedUnchanged seedOutcome = "unchanged"
	seedOwnState  seedOutcome = "own_state"
	seedSkipped   seedOutcome = "skipped"
	seedDiscarded seedOutcome = "discarded"
	seedFailed    seedOutcome = "failed"
	seedUnknown   seedOutcome = "unknown"
)

// The sentences a seed reports. None of them carries a value.
const (
	seedInterruptedNote = "interrupted; nothing seeded"
	seedBusyNote        = "the namespace is busy; nothing seeded"
	seedLockNote        = "the namespace lock could not be taken; nothing seeded"
	seedChangedNote     = "the namespace's credential file changed under the hold; nothing seeded"
	seedNoFileNote      = "the namespace has no credential file to seed beside; run `agentctl claude login`"
	seedPendingNote     = "an unresolved pending write is parked in that namespace; run `agentctl claude status` to settle it first"
	seedTooLargeNote    = "credential_too_large: the seeded document would be over the keychain update ceiling; nothing seeded"
	seedLiveNote        = "the live keychain item could not be read; nothing seeded"
)

// useSeedReport is the `mcp_seed` member of `use <id> --json`: names,
// counts and words, never a value.
type useSeedReport struct {
	Target           string                       `json:"target"`
	Outcome          seedOutcome                  `json:"outcome"`
	Path             *string                      `json:"path"`
	SeededMembers    []useCarriedMember           `json:"seeded_members"`
	SiblingsReadback *secret.SiblingsReadback     `json:"siblings_readback"`
	Transport        *secret.WriteTransport       `json:"transport"`
	OwnedFile        *secret.OwnedFileDisposition `json:"owned_file"`
	Note             *string                      `json:"note"`
	// silent marks a skip the stderr line says nothing about: the
	// environment skip and a keychain that is not unlocked.
	silent bool
	// warning is the sentence that says why the owned file was kept after
	// a seed; it is also the last part of Note.
	warning string
}

// seedInput is what one seed works on: the record `use <id>` resolved, the
// store it lives in and the keychain reader every call goes through.
type seedInput struct {
	paths  *config.Paths
	record *config.AccountRecord
	env    *claude.EnvView
	reader secret.Reader
	// id is the id `use` was given, named in the kept-file warnings.
	id string
	// clock is the lock acquisitions' clock; nil is the system clock.
	clock secret.Clock
}

// lockClock returns the clock the peer-lock acquisitions run on.
func (in *seedInput) lockClock() secret.Clock {
	if in.clock == nil {
		return secret.SystemClock()
	}
	return in.clock
}

// seedNamespaceSiblings creates the namespace's keychain item from its owned
// credential file and the members the live item keeps beside its account
// object, so the first isolated session finds the user's MCP logins. It
// writes only while the item is absent and the file holds no member of its
// own, and never blocks the launch: every failure is an outcome in the
// report.
func (p SessionProcess) seedNamespaceSiblings(ctx context.Context, in seedInput) *useSeedReport {
	sha8 := in.record.Kind.Owned.ExportSHA8
	service := claude.LiveService + "-" + sha8
	report := &useSeedReport{Target: service, Outcome: seedSkipped, SeededMembers: []useCarriedMember{}}
	end := func(outcome seedOutcome, note string) *useSeedReport {
		report.Outcome = outcome
		if note != "" {
			report.Note = &note
		}
		return report
	}
	// The live source would be another namespace's item, so there is
	// nothing that could be called the live MCP state.
	if in.env.SecureStorageDir != nil && *in.env.SecureStorageDir != "" {
		report.silent = true
		return end(seedSkipped, "a secure-storage override is set in agentctl's own environment; nothing seeded")
	}

	preCtx, cancelPre := context.WithTimeout(ctx, SeedPreHoldBudget)
	defer cancelPre()
	// Before the hold a failure is a skip, and its cause is read from the
	// contexts rather than the error: a signal and the phase deadline both
	// surface as whatever the running call returned.
	skip := func(note string) *useSeedReport {
		switch {
		case ctx.Err() != nil:
			note = seedInterruptedNote
		case preCtx.Err() != nil:
			note = fmt.Sprintf("the keychain did not answer within the seeding deadline (%d ms); nothing seeded", SeedPreHoldBudget.Milliseconds())
		}
		return end(seedSkipped, note)
	}
	if status := in.reader.Preflight(preCtx); status.State != secret.KeychainStateUnlocked {
		if ctx.Err() == nil && preCtx.Err() == nil {
			report.silent = true
		}
		return skip("the keychain is " + status.State.String() + "; nothing seeded")
	}
	listing, err := in.reader.ListServices(preCtx, claude.LiveService)
	if err != nil {
		return skip("the keychain items could not be listed; nothing seeded")
	}
	target, refusal := migratedRefreshItem(in.paths, in.record, service, listing)
	if target == nil {
		return skip(refusal)
	}
	present := func() *useSeedReport {
		report.Path = new("present")
		return end(seedUnchanged, "")
	}
	if seedTargetExists(seedListed(listing, service), nil) {
		return present()
	}
	early, err := useReadKeychain(preCtx, in.reader, service)
	defer early.Wipe()
	if err != nil {
		return skip("the namespace item could not be read; nothing seeded")
	}
	if seedTargetExists(false, early) {
		return present()
	}
	report.Path = new("absent")

	source, err := useReadKeychain(preCtx, in.reader, claude.ServiceName(in.env))
	defer source.Wipe()
	if err != nil || source == nil {
		return skip(seedLiveNote)
	}
	if len(source.Siblings) == 0 {
		return end(seedUnchanged, "")
	}

	nsDir := target.nsDir
	fileBefore, docBefore, err := seedReadOwned(in.paths, nsDir)
	defer fileBefore.Wipe()
	if err != nil || fileBefore == nil {
		return end(seedSkipped, seedNoFileNote)
	}
	if seedPendingParked(nsDir) {
		return end(seedSkipped, seedPendingNote)
	}
	if len(fileBefore.Siblings) > 0 {
		return end(seedOwnState, seedOwnStateNote(nsDir, fileBefore))
	}
	digests, err := fileBefore.Digests()
	if err != nil {
		return end(seedSkipped, "the owned credential file could not be fingerprinted; nothing seeded")
	}
	toDigest8, ok := secret.Digest8(digests.AccessSHA256)
	if !ok {
		return end(seedSkipped, "the owned credential file has no usable digest; nothing seeded")
	}
	// The dry document: the file's account object and every live member,
	// admitted against the update ceiling before any lock is taken.
	fileBefore.CarrySiblingsFrom(source)
	useObserveParsed(fileBefore)
	if _, err := fileBefore.ToKeychainUpdate(target.account, service); err != nil {
		if errors.Is(err, secret.ErrCredentialTooLarge) {
			return end(seedSkipped, seedTooLargeNote)
		}
		return end(seedSkipped, "the keychain update could not be built; nothing seeded")
	}
	injected := fault.Active()
	injected.PausePoint(fault.BeforeSeedHold)
	cancelPre()

	dir, err := secret.OpenNamespaceDir(in.paths, nsDir)
	if err != nil {
		return end(seedSkipped, seedLockNote)
	}
	_ = unix.Close(dir)
	seams := secret.RealSeams(in.lockClock())
	// The cap is the acquisition's own context, cancelled as soon as the
	// call returns; the hold keeps no context, so a returned hold stays.
	lockCtx, cancelLock := context.WithTimeout(ctx, SeedLockWait)
	acquisition, err := secret.AcquirePeerLocks(lockCtx, secret.LockSubject{StoreDir: nsDir, Tree: secret.TreeOwn}, in.paths, nil, seams)
	cancelLock()
	seedAuditBreak(ctx, in.paths, acquisition, err, service, sha8)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return end(seedSkipped, seedInterruptedNote)
		case seedLockTimedOut(err):
			return end(seedSkipped, seedBusyNote)
		default:
			return end(seedSkipped, seedLockNote)
		}
	}
	if acquisition.Busy() {
		return end(seedSkipped, seedBusyNote)
	}
	hold := acquisition.Held
	defer hold.Release()

	// From here every way out records the attempt, after the owned file's
	// fate is known.
	audit := secret.SeedEvent{Target: secret.NamespaceTarget(sha8), ToDigest8: toDigest8, Outcome: secret.WriteDiscarded}
	defer func() { appendRefreshAudit(ctx, in.paths, &audit) }()
	discard := func(note string) *useSeedReport {
		if ctx.Err() != nil {
			note = seedInterruptedNote
		}
		return end(seedDiscarded, note)
	}
	if err := hold.DriftCheck(); err != nil {
		return discard("the namespace lock agentctl holds is compromised; nothing seeded")
	}
	readCtx, cancelRead := context.WithTimeout(ctx, secret.KeychainVerifyTimeout)
	held, err := useReadKeychain(readCtx, in.reader, service)
	cancelRead()
	defer held.Wipe()
	if err != nil {
		return discard("the namespace item could not be re-read; nothing seeded")
	}
	if seedTargetExists(false, held) {
		return discard("Claude Code created the namespace item meanwhile; nothing seeded")
	}
	fileHeld, docHeld, err := seedReadOwned(in.paths, nsDir)
	defer fileHeld.Wipe()
	if err != nil || fileHeld == nil || seedPendingParked(nsDir) {
		return discard(seedChangedNote)
	}
	if len(fileHeld.Siblings) > 0 {
		audit.Outcome = secret.WriteOwnState
		return end(seedOwnState, seedOwnStateNote(nsDir, fileHeld))
	}
	if docHeld != docBefore {
		return discard(seedChangedNote)
	}
	// docHeld was fixed by the read above, so the carry, which replaces
	// the credential's members, cannot change what the removal compares.
	fileHeld.CarrySiblingsFrom(source)
	useObserveParsed(fileHeld)
	// A member another process changes from here until the write commits is
	// overwritten and cannot be detected: the accepted residual race.
	injected.PausePoint(fault.SeedAfterHeldRead)
	update, err := fileHeld.ToKeychainUpdate(target.account, service)
	if err != nil {
		if errors.Is(err, secret.ErrCredentialTooLarge) {
			return discard("credential_too_large: the owned file grew under the hold; nothing seeded")
		}
		return discard("the keychain update could not be built; nothing seeded")
	}
	carried, err := fileHeld.SiblingSummary()
	if err != nil {
		return discard("the members to seed could not be described; nothing seeded")
	}
	after, err := fileHeld.Digests()
	if err != nil {
		return discard("the owned credential file could not be fingerprinted under the hold; nothing seeded")
	}
	if err := hold.DriftCheck(); err != nil {
		return discard("the namespace lock agentctl holds is compromised; nothing seeded")
	}
	if err := hold.WriteAdmission(); err != nil || hold.HoldElapsed()+secret.KeychainWriteTimeout+secret.KeychainWriteWaitDelay > secret.HoldBudget {
		return discard("the hold ran out of budget before the write; nothing seeded")
	}
	if ctx.Err() != nil {
		return discard(seedInterruptedNote)
	}

	writeErr := secret.NewKeychainWriter(p.Signals).Write(ctx, service, update)
	hold.Release()
	audit.Signal = secret.KeychainWriteSignal(writeErr)
	if errors.Is(writeErr, secret.ErrKeychainNotStarted) {
		return discard(useNotStartedNote(writeErr))
	}
	transport := secret.TransportArgv
	report.Transport, audit.Transport = &transport, transport
	report.SeededMembers, audit.Seeded = useCarried(carried), useCarriedNames(carried)
	kept := secret.OwnedFileKept
	report.OwnedFile, audit.OwnedFile = &kept, kept
	if writeErr != nil && !errors.Is(writeErr, secret.ErrKeychainOutcomeUnknown) {
		audit.Outcome = secret.WriteFailed
		return end(seedFailed, "the keychain write failed ("+seedErrorClass(writeErr)+"); nothing seeded")
	}
	injected.PausePoint(fault.SeedAfterWrite)

	// Readback is outside the hold: a peer winning this gap makes the
	// result unknown, and the owned file stays.
	verified, verifyErr := useReadKeychain(ctx, in.reader, service)
	defer verified.Wipe()
	applied := false
	if verifyErr == nil && verified != nil {
		digests, err := verified.Digests()
		applied = err == nil && digests == after
	}
	readback, changed := useSiblingsReadback(carried, verified, verifyErr)
	report.SiblingsReadback, audit.SiblingsReadback = &readback, readback
	notes := []string{}
	if readback == secret.SiblingsDiffer {
		notes = append(notes, useSiblingsMismatchNote(changed))
	}
	audit.Outcome, report.Outcome = secret.WriteUnknown, seedUnknown
	if applied {
		audit.Outcome, report.Outcome = secret.WriteApplied, seedSeeded
	} else {
		notes = append([]string{"the created namespace item could not be confirmed; re-run `agentctl claude status`"}, notes...)
	}

	// The owned file goes only when the item demonstrably holds everything
	// written: an unconfirmed item may leave the file the namespace's only
	// good copy.
	if report.Outcome == seedSeeded && readback == secret.SiblingsEqual {
		disposition, warning := seedRemoveOwnedFile(ctx, in, nsDir, service, sha8, docHeld)
		report.OwnedFile, audit.OwnedFile = &disposition, disposition
		if warning != "" {
			report.warning = warning
			notes = append(notes, warning)
		}
	}
	if len(notes) != 0 {
		report.Note = new(strings.Join(notes, ". "))
	}
	return report
}

// seedRemoveOwnedFile removes the namespace's owned credential file once the
// created item is confirmed, so the item is the namespace's only credential
// as after Claude Code's own migration. It holds the lock agentctl's file
// writers take and then the one Claude Code's credential writes take, and
// removes the file only while it is still byte for byte the file the item's
// account was taken from (docHeld). Every other case keeps the file and
// returns the sentence saying why.
func seedRemoveOwnedFile(ctx context.Context, in seedInput, nsDir, service, sha8 string, docHeld [sha256.Size]byte) (secret.OwnedFileDisposition, string) {
	file := filepath.Join(nsDir, secret.CredentialsFile)
	kept := func(why string) (secret.OwnedFileDisposition, string) {
		return secret.OwnedFileKept, fmt.Sprintf("the owned credential file `%s` %s; `agentctl claude use --live %s` reads this file until it is removed", file, why, in.id)
	}
	interrupted := func() (secret.OwnedFileDisposition, string) {
		return kept("was kept: the run was interrupted before it was removed")
	}
	lockFailed := func(err error) (secret.OwnedFileDisposition, string) {
		switch {
		case ctx.Err() != nil:
			return interrupted()
		case errors.Is(err, secret.ErrLockBusy), seedLockTimedOut(err):
			return kept("was kept: its namespace is busy")
		default:
			return kept("was kept: its namespace lock could not be taken (" + seedErrorClass(err) + ")")
		}
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	removeBy := time.Now().Add(SeedFileRemoveWait)
	guard, err := secret.Acquire(ctx, in.paths.LocksDir(), filepath.Base(in.paths.LockPath(in.record.AccountUUID, in.record.OrganizationUUID)), removeBy)
	if err != nil {
		return lockFailed(err)
	}
	defer func() { _ = guard.Release() }()
	injected := fault.Active()
	seams := secret.RealSeams(in.lockClock())
	lockCtx, cancelLock := context.WithDeadline(ctx, removeBy)
	acquisition, err := secret.AcquirePeerLocks(lockCtx, secret.LockSubject{StoreDir: nsDir, Tree: secret.TreeOwn}, in.paths, nil, seams)
	cancelLock()
	seedAuditBreak(ctx, in.paths, acquisition, err, service, sha8)
	if err != nil {
		return lockFailed(err)
	}
	if acquisition.Busy() {
		return kept("was kept: its namespace is busy")
	}
	hold := acquisition.Held
	defer hold.Release()
	if err := hold.DriftCheck(); err != nil {
		return kept("was kept: its namespace lock could not be taken (" + seedErrorClass(err) + ")")
	}
	injected.PausePoint(fault.SeedBeforeFileRemove)
	fileFinal, docFinal, err := seedReadOwned(in.paths, nsDir)
	defer fileFinal.Wipe()
	if err != nil || fileFinal == nil || docFinal != docHeld {
		return kept("was kept: it changed after the seed")
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	removed, err := secret.RemoveCredentialsFile(in.paths, nsDir)
	if err != nil {
		return kept("could not be removed (" + seedErrorClass(err) + ")")
	}
	if !removed {
		return kept("was kept: it changed after the seed")
	}
	return secret.OwnedFileRemoved, ""
}

// seedAuditBreak appends the one lock break an acquisition may have made,
// whatever it returned, as the namespace refresh audits one.
func seedAuditBreak(ctx context.Context, paths *config.Paths, acquisition *secret.Acquisition, err error, service, sha8 string) {
	var draft *secret.BreakDraft
	if acquisition != nil {
		draft = acquisition.BreakRecord
	}
	if failure, ok := errors.AsType[*secret.AcquireError](err); ok {
		draft = failure.BreakRecord
	}
	if draft != nil {
		appendRefreshAudit(ctx, paths, draft.Complete(service, secret.NamespaceTarget(sha8)))
	}
}

// seedLockTimedOut reports an acquisition ended by its own deadline. The
// caller has already ruled out a cancelled run, so a cancellation here is
// the deadline: the contention wait reports both as a cancellation.
func seedLockTimedOut(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// seedTargetExists reports that the namespace already has its keychain
// item, by the listing or by a read. Seeding never reads an existing item
// for a merge and never writes it.
func seedTargetExists(listed bool, read *claude.Credentials) bool {
	return listed || read != nil
}

// seedListed reports whether the listing carries service.
func seedListed(listing []secret.ServiceEntry, service string) bool {
	return slices.ContainsFunc(listing, func(entry secret.ServiceEntry) bool { return entry.Service == service })
}

// seedReadOwned reads the owned credential file the strict way discovery
// does, with the sha256 of the whole document as read.
func seedReadOwned(paths *config.Paths, nsDir string) (*claude.Credentials, [sha256.Size]byte, error) {
	credentials, digest, err := claude.ReadOwnedCredentials(paths, nsDir)
	useObserveParsed(credentials)
	return credentials, digest, err
}

// seedPendingParked reports a parked pending credential, which may be newer
// than the file about to become the item's account credential. Anything
// but a clean absence counts as parked.
func seedPendingParked(nsDir string) bool {
	_, err := os.Lstat(filepath.Join(nsDir, secret.PendingFile))
	return !errors.Is(err, fs.ErrNotExist)
}

// seedOwnStateNote names the members an owned file holds of its own, never
// a value.
func seedOwnStateNote(nsDir string, file *claude.Credentials) string {
	names := "its members beside the account object"
	if summary, err := file.SiblingSummary(); err == nil {
		quoted := useCarriedNames(summary)
		for i, name := range quoted {
			quoted[i] = "`" + name + "`"
		}
		names = strings.Join(quoted, ", ")
	}
	return fmt.Sprintf("the namespace's credential file `%s` holds MCP state of its own (%s); it was left as it is and nothing was seeded", filepath.Join(nsDir, secret.CredentialsFile), names)
}

// seedErrorClass names a keychain failure by its class, or any other error
// by its message, which carries paths and reasons but no credential.
func seedErrorClass(err error) string {
	if keychain, ok := errors.AsType[*secret.KeychainError](err); ok {
		return string(keychain.Class)
	}
	return err.Error()
}

// prose is the one stderr line a seed prints before the launch, or "" when
// it has nothing to say.
func (r *useSeedReport) prose() string {
	switch {
	case r.Outcome == seedUnchanged, r.silent:
		return ""
	case r.Outcome == seedSeeded:
		members := make([]string, 0, len(r.SeededMembers))
		for _, member := range r.SeededMembers {
			switch {
			case member.Entries == nil:
				members = append(members, member.Name)
			case *member.Entries == 1:
				members = append(members, member.Name+" (1 entry)")
			default:
				members = append(members, fmt.Sprintf("%s (%d entries)", member.Name, *member.Entries))
			}
		}
		last := "removed the owned credential file"
		switch {
		case r.warning != "":
			last = r.warning
		case r.OwnedFile != nil && *r.OwnedFile == secret.OwnedFileKept:
			last = "kept the owned credential file"
			if r.Note != nil {
				last += ": " + *r.Note
			}
		}
		return fmt.Sprintf("seeded %s into %s; %s", strings.Join(members, ", "), r.Target, last)
	}
	note := ""
	if r.Note != nil {
		note = *r.Note
	}
	return "MCP credentials not seeded: " + note
}
