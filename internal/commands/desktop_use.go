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
	"io"
	"strings"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
	"github.com/zchee/agentctl/internal/runtime/fault"
)

// The public words of a switch that ran past its refusals.
const (
	wordSelected           = "selected"
	wordAlreadySelected    = "already_selected"
	wordLaunchedUnverified = "launched_unverified"
	wordRevertedRelaunched = "reverted_relaunched"
	wordRelaunchFailed     = "relaunch_failed"
	wordJournalNotRecorded = "journal_not_recorded"
	wordExchangeNotRestore = "exchange_not_restored"
	wordRepaired           = "repaired"
	wordRepairedNotRunning = "repaired_not_running"
	wordNothingToRepair    = "nothing_to_repair"
	wordRunUseAgain        = "aborted"
	wordPartial            = "partial"
	wordForgotten          = "forgotten"
	wordNeedsChoice        = "desktop_repair_needs_choice"
	wordDestinationExists  = "desktop_destination_exists"
	wordProfileSymlink     = "desktop_profile_symlink"
	journalOpen            = "open"
)

// targetNone is the identity recorded for a target the app has not
// created yet: the requested account has no stored profile.
const targetNone desktop.ReaderState = "none"

// desktopReport is the JSON document of `use` and `repair`.
type desktopReport struct {
	Version  int               `json:"version"`
	Command  string            `json:"command"`
	Outcome  string            `json:"outcome"`
	Detail   string            `json:"detail,omitzero"`
	OpID     string            `json:"op_id,omitzero"`
	Account  string            `json:"account,omitzero"`
	Identity string            `json:"identity,omitzero"`
	State    string            `json:"state,omitzero"`
	Journal  string            `json:"journal,omitzero"`
	Outgoing *desktop.Outgoing `json:"outgoing,omitzero"`
	Target   *desktop.Target   `json:"target,omitzero"`
	Main     *desktop.Main     `json:"main,omitzero"`
	Layout   []layoutRow       `json:"layout,omitzero"`
}

// layoutRow names one directory an interrupted operation touches.
type layoutRow struct {
	Role    string          `json:"role"`
	Path    string          `json:"path"`
	Present bool            `json:"present"`
	File    *desktop.FileID `json:"file"`
}

// finish prints the report and returns err. A refusal fills the outcome
// with its word, so a JSON consumer always gets one document.
func finish(out io.Writer, asJSON bool, rep *desktopReport, err error) error {
	var refusal *desktopRefusal
	switch {
	case errors.As(err, &refusal):
		if rep.Outcome == "" {
			rep.Outcome = refusal.Word
		}
		if rep.Detail == "" {
			rep.Detail = refusal.Detail
		}
	case err != nil && rep.Outcome == "":
		rep.Outcome, rep.Detail = "error", err.Error()
	}
	if asJSON {
		if werr := writeJSON(out, rep); werr != nil && err == nil {
			return werr
		}
		return err
	}
	var lines []string
	if err == nil {
		lines = append(lines, renderDesktop(rep))
	}
	if rep.State != "" {
		lines = append(lines, "state "+rep.State)
	}
	for _, row := range rep.Layout {
		where := "absent"
		if row.File != nil {
			where = row.File.String()
		}
		lines = append(lines, fmt.Sprintf("  %-8s %s  %s", row.Role, where, row.Path))
	}
	if len(lines) > 0 {
		if terr := tell(out, strings.Join(lines, "\n")); terr != nil && err == nil {
			return terr
		}
	}
	return err
}

// renderDesktop is the one-line summary of a finished `use` or `repair`.
func renderDesktop(rep *desktopReport) string {
	parts := []string{rep.Outcome}
	if rep.Identity != "" {
		parts = append(parts, rep.Identity)
	}
	if rep.Account != "" {
		parts = append(parts, "account "+rep.Account)
	}
	if rep.Main != nil {
		parts = append(parts, fmt.Sprintf("pid %d", rep.Main.PID))
	}
	line := strings.Join(parts, " ")
	if rep.Detail != "" {
		line += ": " + rep.Detail
	}
	return line
}

// observe takes one snapshot and maps a failure onto its refusal.
func (e *desktopEnv) observe(ctx context.Context) (rdesktop.Snapshot, error) {
	snap, err := e.sys.Observer.Snapshot(ctx)
	if err != nil {
		return snap, refuse(desktopObserveFailed, "could not observe the app's processes: %v", err)
	}
	return snap, nil
}

// quitRefusal maps a failed quit step onto its refusal word.
func quitRefusal(err error) error {
	var other *rdesktop.OtherCopyError
	var refused *rdesktop.QuitRefusedError
	var timeout *rdesktop.QuitTimeoutError
	switch {
	case errors.As(err, &other):
		return refuse(desktopOtherCopyRunning, "%v", err)
	case errors.As(err, &refused):
		return refuse(desktopQuitRefused, "%v", err)
	case errors.As(err, &timeout):
		return refuse(desktopQuitTimeout, "%v", err)
	default:
		return refuse(desktopObserveFailed, "%v", err)
	}
}

// observedResult fills a result with what can be seen now: the identity at
// the data path, its (device, inode) and the main the caller observed.
// Nothing in it is predicted.
func (e *desktopEnv) observedResult(in *desktop.Intent, outcome desktop.Outcome, word string, main *rdesktop.Proc, running bool) desktop.Result {
	res := desktop.Result{
		Type: desktop.TypeResult, OpID: in.OpID, Time: desktop.Now(), Command: in.Command,
		RequestedAccount: in.RequestedAccount, SwapAuditID: in.SwapAuditID,
		Outcome: outcome, Word: word, AppRunning: running,
	}
	if id, err := desktop.StatID(e.layout.DefaultData); err == nil {
		rec := desktop.RecordOf(desktop.ReadDesktopIdentity(e.layout.DefaultData))
		res.Installed, res.Observed = &id, &rec
	}
	if main != nil {
		res.Main = &desktop.Main{PID: main.PID, Start: main.StartIdentity()}
	}
	return res
}

// closeIntent appends a result for an operation that stopped before the
// exchange changed anything. When the append fails the intent stays open
// and the report says so; the caller's refusal still decides the exit.
func (e *desktopEnv) closeIntent(ctx context.Context, rep *desktopReport, in *desktop.Intent, word string) {
	running := false
	var main *rdesktop.Proc
	if snap, err := e.sys.Observer.Snapshot(ctx); err == nil {
		running = len(snap.Mains) > 0
		if running {
			main = &snap.Mains[0]
		}
	}
	res := e.observedResult(in, desktop.OutcomeAborted, word, main, running)
	if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
		rep.Journal = journalOpen
		e.log.Warn("desktop result not recorded", "op_id", in.OpID, "error", err.Error())
		return
	}
	rep.Journal = string(desktop.OutcomeAborted)
}

// outgoingName is the stored name the profile leaving the data path gets.
func outgoingName(id desktop.Identity, opID string) string {
	switch id.State {
	case desktop.StateUUID:
		return desktop.AccountName(id.UUID)
	case desktop.StateFresh:
		return desktop.FreshName(opID)
	default:
		return desktop.UnknownName(opID)
	}
}

// RunUse switches the desktop app to the account's profile: it quits the
// app, moves the profile at the data path aside under its own name, puts
// the account's stored profile (or none, for a first sign-in) in its
// place and starts the app again. Every refusal happens before the quit.
func (p DesktopProcess) RunUse(ctx context.Context, globals cli.Globals, opts cli.ClaudeDesktopUseOptions) error {
	rep := &desktopReport{Version: 1, Command: "use"}
	account, err := resolveAccount(ctx, globals, opts.ID)
	if err != nil {
		return finish(p.Out, opts.JSON, rep, err)
	}
	rep.Account = account
	env, err := p.newEnv()
	if err != nil {
		return finish(p.Out, opts.JSON, rep, err)
	}
	err = env.use(ctx, p, rep, account, opts.Yes)
	return finish(p.Out, opts.JSON, rep, err)
}

func (e *desktopEnv) use(ctx context.Context, p DesktopProcess, rep *desktopReport, account string, yes bool) error {
	if err := e.checkBundle(); err != nil {
		return err
	}
	if err := e.checkLayout(); err != nil {
		return err
	}
	guard, err := e.lock(ctx, p.LockWait)
	if err != nil {
		return err
	}
	defer func() { _ = guard.Release() }()

	snap, err := e.observe(ctx)
	if err != nil {
		return err
	}
	if len(snap.OtherCopies) > 0 {
		return quitRefusal(&rdesktop.OtherCopyError{Procs: snap.OtherCopies})
	}
	entries, err := desktop.ReadJournal(e.layout.Journal)
	if err != nil {
		return refuse(desktopJournalFailed, "could not read %s: %v", e.layout.Journal, err)
	}
	if open := desktop.OpenIntents(entries); len(open) > 0 {
		return refuse(desktopIntentOpen, "operation %s recorded its intent and no result; run `agentctl claude desktop repair`", open[0].OpID)
	}
	if err := e.sameDevice(); err != nil {
		return err
	}

	dPresent, err := desktop.Exists(e.layout.DefaultData)
	if err != nil {
		return errs.NewIO("could not check the desktop data path", err)
	}
	defaultID := desktop.ReadDesktopIdentity(e.layout.DefaultData)
	if dPresent && defaultID.State == desktop.StateUnreadable {
		return refuse(desktopProfileUnreadable, "the profile at %s: %s", e.layout.DefaultData, defaultID.Reason)
	}
	targetName := desktop.AccountName(account)
	targetPath := e.layout.Profile(targetName)
	tExists, err := desktop.Exists(targetPath)
	if err != nil {
		return errs.NewIO("could not check the stored profile", err)
	}
	target := desktop.Target{Name: targetName, Identity: desktop.IdentityRecord{State: targetNone}, Source: targetPath, Existed: tExists}
	if tExists {
		tID := desktop.ReadDesktopIdentity(targetPath)
		switch tID.WordFor(account) {
		case desktop.WordUnreadable:
			return refuse(desktopProfileUnreadable, "the stored profile %s: %s", targetName, tID.Reason)
		case desktop.WordMismatch:
			return refuse(desktopProfileMismatch, "the stored profile %s last observed account %s", targetName, tID.UUID)
		}
		target.Identity = desktop.RecordOf(tID)
		rep.Identity = string(tID.WordFor(account))
	} else {
		rep.Identity = string(desktop.WordFresh)
	}

	if dPresent && defaultID.State == desktop.StateUUID && defaultID.UUID == account {
		return e.alreadySelected(rep, snap, account)
	}

	opID, err := desktop.NewOpID()
	if err != nil {
		return errs.NewIO("could not draw an operation id", err)
	}
	rep.OpID = opID
	in := &desktop.Intent{Type: desktop.TypeIntent, OpID: opID, Time: desktop.Now(), Command: "use", RequestedAccount: account, Target: target}
	if tExists {
		id, err := desktop.StatID(targetPath)
		if err != nil {
			return errs.NewIO("could not read the stored profile", err)
		}
		in.Target.File = &id
	} else {
		in.Target.Park = desktop.FreshTargetName(opID)
	}
	if dPresent {
		name := outgoingName(defaultID, opID)
		dest := e.layout.Profile(name)
		if exists, err := desktop.Exists(dest); err != nil {
			return errs.NewIO("could not check "+dest, err)
		} else if exists {
			return refuse(wordDestinationExists, "the profile at the data path would be stored as %s, which already exists; `agentctl claude desktop forget %s` removes the stored copy", name, name)
		}
		id, err := desktop.StatID(e.layout.DefaultData)
		if err != nil {
			return errs.NewIO("could not read the desktop data path", err)
		}
		in.Outgoing = &desktop.Outgoing{Name: name, Identity: desktop.RecordOf(defaultID), Destination: dest, File: id}
	}
	rep.Outgoing, rep.Target = in.Outgoing, &in.Target

	if !yes {
		ok, err := e.prompt.Confirm(ctx, consentQuestion(snap, in))
		if err != nil {
			return err
		}
		if !ok {
			return errs.NewRefused(0, "the desktop switch was not confirmed; nothing was quit or moved")
		}
	}

	if err := e.appendRecord(in, e.fault.Is(fault.DesktopIntentAppendFail)); err != nil {
		return refuse(desktopJournalFailed, "could not record the intent: %v", err)
	}
	crashPoint(e.fault.Is(fault.DesktopAbortAfterIntent))

	quit, err := e.sys.Quit(ctx)
	if err != nil {
		e.closeIntent(ctx, rep, in, quitWord(err))
		return quitRefusal(err)
	}
	crashPoint(e.fault.Is(fault.DesktopAbortAfterQuit))
	before := quit.Before.Mains

	if in.Outgoing != nil {
		if _, err := desktop.Move(e.layout.DefaultData, in.Outgoing.Destination, &in.Outgoing.File); err != nil {
			return e.failedFirstMove(ctx, rep, in, before, err)
		}
		crashPoint(e.fault.Is(fault.DesktopAbortAfterOutgoingRename))
	}
	if tExists {
		if _, err := desktop.Move(targetPath, e.layout.DefaultData, in.Target.File); err != nil {
			e.log.Warn("desktop target rename failed", "op_id", opID, "error", err.Error())
			return e.rollback(ctx, rep, in, before)
		}
		crashPoint(e.fault.Is(fault.DesktopAbortAfterTargetRename))
	}

	launch := e.sys.Launch(ctx, before)
	main, late := launch.Main, false
	if !launch.Ready {
		if snap, err := e.sys.Observer.Snapshot(ctx); err == nil {
			main = rdesktop.NewMain(snap, before)
			late = main != nil
		}
	}
	if !launch.Ready && !late {
		return e.rollback(ctx, rep, in, before)
	}
	word := wordSelected
	if late {
		word = wordLaunchedUnverified
	}
	crashPoint(e.fault.Is(fault.DesktopAbortBeforeResult))
	res := e.observedResult(in, desktop.OutcomeApplied, word, main, true)
	res.Verified = !late
	rep.Main = res.Main
	if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
		rep.Journal = journalOpen
		return refuse(wordJournalNotRecorded, "the switch is done but its result was not recorded (%v); run `agentctl claude desktop repair`", err)
	}
	rep.Outcome, rep.Journal = word, string(desktop.OutcomeApplied)
	return nil
}

// quitWord is the result word recorded when the quit step stops a switch.
func quitWord(err error) string {
	if refusal, ok := errors.AsType[*desktopRefusal](quitRefusal(err)); ok {
		return refusal.Word
	}
	return desktopObserveFailed
}

// alreadySelected records a switch to the account already at the data
// path: nothing is quit or moved, and one noop result says so.
func (e *desktopEnv) alreadySelected(rep *desktopReport, snap rdesktop.Snapshot, account string) error {
	opID, err := desktop.NewOpID()
	if err != nil {
		return errs.NewIO("could not draw an operation id", err)
	}
	in := &desktop.Intent{OpID: opID, Command: "use", RequestedAccount: account}
	var main *rdesktop.Proc
	if len(snap.Mains) > 0 {
		main = &snap.Mains[0]
	}
	res := e.observedResult(in, desktop.OutcomeNoop, wordAlreadySelected, main, main != nil)
	res.Verified = true
	if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
		return refuse(desktopJournalFailed, "could not record the result: %v", err)
	}
	rep.OpID, rep.Outcome, rep.Identity, rep.Journal = opID, wordAlreadySelected, string(desktop.WordMatches), string(desktop.OutcomeNoop)
	rep.Main = res.Main
	return nil
}

// failedFirstMove handles a failed move of the outgoing profile: nothing
// was exchanged, so the app is started again on the same profile and the
// intent is closed as aborted.
func (e *desktopEnv) failedFirstMove(ctx context.Context, rep *desktopReport, in *desktop.Intent, before []rdesktop.Proc, moveErr error) error {
	id, err := desktop.StatID(e.layout.DefaultData)
	if err != nil || id != in.Outgoing.File {
		rep.Outcome, rep.Journal = wordExchangeNotRestore, journalOpen
		return refuse(wordExchangeNotRestore, "the move of %s failed (%v) and the data path no longer holds it; run `agentctl claude desktop repair`", e.layout.DefaultData, moveErr)
	}
	launch := e.sys.Launch(ctx, before)
	word := string(desktop.OutcomeAborted)
	if errors.Is(moveErr, desktop.ErrDestinationExists) {
		word = wordDestinationExists
	}
	res := e.observedResult(in, desktop.OutcomeAborted, word, launch.Main, launch.Ready)
	if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
		rep.Journal = journalOpen
	} else {
		rep.Journal = string(desktop.OutcomeAborted)
	}
	if errors.Is(moveErr, desktop.ErrDestinationExists) {
		return refuse(wordDestinationExists, "%v; nothing was moved and the app was started again", moveErr)
	}
	return errs.NewIO("could not move the profile at the data path; nothing was moved", moveErr)
}

// rollback undoes the exchange after the target did not start: the target
// goes back where it came from (a tree the app created for a new account is
// parked under its own name) and the outgoing profile returns to the data
// path. Then one recovery launch is tried and its observation recorded.
func (e *desktopEnv) rollback(ctx context.Context, rep *desktopReport, in *desktop.Intent, before []rdesktop.Proc) error {
	notRestored := func(err error) error {
		rep.Outcome, rep.Journal = wordExchangeNotRestore, journalOpen
		return refuse(wordExchangeNotRestore, "the app did not start and the exchange could not be undone (%v); run `agentctl claude desktop repair`", err)
	}
	if snap, err := e.sys.Observer.Snapshot(ctx); err != nil {
		return notRestored(err)
	} else if !snap.Quiet() {
		return notRestored(fmt.Errorf("part of the app is still running (%d main, %d family)", len(snap.Mains), len(snap.Family)))
	}
	if e.fault.Is(fault.DesktopRollbackRenameFail) {
		return notRestored(errors.New("rename failed (injected)"))
	}
	if err := e.moveTargetBack(in); err != nil {
		return notRestored(err)
	}
	if in.Outgoing != nil {
		present, err := desktop.Exists(in.Outgoing.Destination)
		if err != nil {
			return notRestored(err)
		}
		if present {
			if _, err := desktop.Move(in.Outgoing.Destination, e.layout.DefaultData, &in.Outgoing.File); err != nil {
				return notRestored(err)
			}
		}
	}
	recovery := e.sys.Launch(ctx, before)
	word := wordRelaunchFailed
	if recovery.Ready {
		word = wordRevertedRelaunched
	}
	crashPoint(e.fault.Is(fault.DesktopAbortBeforeResult))
	res := e.observedResult(in, desktop.OutcomeReverted, word, recovery.Main, recovery.Ready)
	rep.Main = res.Main
	if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
		rep.Outcome, rep.Journal = wordJournalNotRecorded, journalOpen
		return refuse(wordJournalNotRecorded, "the exchange was undone but its result was not recorded (%v); run `agentctl claude desktop repair`", err)
	}
	rep.Outcome, rep.Journal = word, string(desktop.OutcomeReverted)
	return refuse(word, "the app did not start on the requested profile; the previous profile is back at the data path")
}

// moveTargetBack returns whatever is at the data path to the target's
// source: the stored target, or, for an account without one, the tree the
// app may have created, parked under its own name.
func (e *desktopEnv) moveTargetBack(in *desktop.Intent) error {
	present, err := desktop.Exists(e.layout.DefaultData)
	if err != nil || !present {
		return err
	}
	if in.Target.Existed {
		_, err := desktop.Move(e.layout.DefaultData, in.Target.Source, in.Target.File)
		return err
	}
	_, err = desktop.Move(e.layout.DefaultData, e.layout.Profile(in.Target.Park), nil)
	return err
}

// consentQuestion names what will close and what will move.
func consentQuestion(snap rdesktop.Snapshot, in *desktop.Intent) string {
	closing := len(snap.Mains) + len(snap.Family)
	outgoing := "no profile is at the data path"
	if in.Outgoing != nil {
		who := string(in.Outgoing.Identity.State)
		if in.Outgoing.Identity.UUID != "" {
			who = "account " + in.Outgoing.Identity.UUID
		}
		outgoing = fmt.Sprintf("the profile at the data path (%s) moves to %s", who, in.Outgoing.Name)
	}
	incoming := "a new profile is created at the next start (sign in once)"
	if in.Target.Existed {
		incoming = "the stored profile " + in.Target.Name + " moves in"
	}
	return fmt.Sprintf("Quit the Claude desktop app (%d running process(es) close), %s, %s, and start the app for account %s?", closing, outgoing, incoming, in.RequestedAccount)
}
