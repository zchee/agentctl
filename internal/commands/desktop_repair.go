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
	"fmt"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
	"github.com/zchee/agentctl/internal/runtime/fault"
)

// The layouts an interrupted operation can leave, read from its intent's
// (device, inode) pairs and the directories as they are now.
const (
	stateNeverStarted      = "never-started"
	stateOutgoingMoved     = "outgoing-moved"
	stateTargetInstalled   = "target-installed"
	stateUnexpectedDefault = "unexpected-default"
	stateNoIntent          = "no-intent"
	stateInconsistent      = "inconsistent"
)

// repairAction is what the person asked repair to do.
type repairAction int

const (
	actionNone repairAction = iota
	actionComplete
	actionRevert
)

// dirState is one directory as observed now.
type dirState struct {
	path    string
	present bool
	id      desktop.FileID
}

func observeDir(path string) (dirState, error) {
	st := dirState{path: path}
	present, err := desktop.Exists(path)
	if err != nil || !present {
		return st, err
	}
	id, err := desktop.StatID(path)
	if err != nil {
		return st, err
	}
	st.present, st.id = true, id
	return st, nil
}

func (d dirState) carries(id *desktop.FileID) bool {
	return d.present && id != nil && d.id == *id
}

func (d dirState) row(role string) layoutRow {
	row := layoutRow{Role: role, Path: d.path, Present: d.present}
	if d.present {
		id := d.id
		row.File = &id
	}
	return row
}

// repairLayout is the observed layout of one open intent.
type repairLayout struct {
	data, dest, src dirState
	rows            []layoutRow
}

// observeLayout reads the three directories the intent names.
func (e *desktopEnv) observeLayout(in *desktop.Intent) (repairLayout, error) {
	var l repairLayout
	var err error
	if l.data, err = observeDir(e.layout.DefaultData); err != nil {
		return l, err
	}
	l.rows = append(l.rows, l.data.row("data"))
	if in.Outgoing != nil {
		if l.dest, err = observeDir(in.Outgoing.Destination); err != nil {
			return l, err
		}
		l.rows = append(l.rows, l.dest.row("outgoing"))
	}
	if in.Target.Existed {
		if l.src, err = observeDir(in.Target.Source); err != nil {
			return l, err
		}
		l.rows = append(l.rows, l.src.row("target"))
	}
	return l, nil
}

// classify names the state of the layout. The target of an account without
// a stored profile has no recorded pair, so it is recognised at the data
// path by what that profile observed: nothing yet, or the requested
// account. A stored target is recognised only by its pair, so a profile the
// app created at the data path after a crash is never taken for it.
func (e *desktopEnv) classify(in *desktop.Intent, l repairLayout) string {
	var outgoing *desktop.FileID
	if in.Outgoing != nil {
		outgoing = &in.Outgoing.File
	}
	oAtData := l.data.carries(outgoing)
	oAtDest := in.Outgoing != nil && l.dest.carries(outgoing)
	destAbsent := in.Outgoing == nil || !l.dest.present
	tAtSrc := !in.Target.Existed || l.src.carries(in.Target.File)
	srcAbsent := !in.Target.Existed || !l.src.present
	tAtData := false
	if in.Target.Existed {
		tAtData = l.data.carries(in.Target.File)
	} else if l.data.present && !oAtData {
		w := desktop.ReadDesktopIdentity(e.layout.DefaultData).WordFor(in.RequestedAccount)
		tAtData = w == desktop.WordFresh || w == desktop.WordMatches
	}
	destOK := in.Outgoing == nil || oAtDest
	switch {
	case in.Outgoing != nil && oAtData && destAbsent:
		return stateNeverStarted
	case in.Outgoing == nil && !l.data.present && tAtSrc:
		return stateNeverStarted
	case in.Outgoing != nil && !l.data.present && oAtDest && tAtSrc:
		return stateOutgoingMoved
	case tAtData && destOK && srcAbsent:
		return stateTargetInstalled
	case l.data.present && !oAtData && !tAtData && destOK && tAtSrc:
		return stateUnexpectedDefault
	default:
		return stateInconsistent
	}
}

// RunRepair finishes or undoes an interrupted switch, as the state of its
// directories and the chosen action decide. Every move proves where the
// directory landed; a layout that fits no state is refused untouched.
func (p DesktopProcess) RunRepair(ctx context.Context, _ cli.Globals, opts cli.ClaudeDesktopRepairOptions) error {
	rep := &desktopReport{Version: 1, Command: "repair"}
	if opts.Complete && opts.Revert {
		return errs.NewRefused(0, "--complete and --revert cannot be combined")
	}
	action := actionNone
	switch {
	case opts.Complete:
		action = actionComplete
	case opts.Revert:
		action = actionRevert
	}
	env, err := p.newEnv()
	if err != nil {
		return finish(p.Out, false, rep, err)
	}
	return finish(p.Out, false, rep, env.repair(ctx, p, rep, action, opts.Yes))
}

func (e *desktopEnv) repair(ctx context.Context, p DesktopProcess, rep *desktopReport, action repairAction, yes bool) error {
	if err := e.checkLayout(); err != nil {
		return err
	}
	guard, err := e.lock(ctx, p.LockWait)
	if err != nil {
		return err
	}
	defer func() { _ = guard.Release() }()

	entries, err := desktop.ReadJournal(e.layout.Journal)
	if err != nil {
		return refuse(desktopJournalFailed, "could not read %s: %v", e.layout.Journal, err)
	}
	open := desktop.OpenIntents(entries)
	if len(open) == 0 {
		rep.State, rep.Outcome = stateNoIntent, wordNothingToRepair
		return nil
	}
	if len(open) > 1 {
		rep.State = stateInconsistent
		return refuse(desktopRepairInconsistent, "%d operations are open at once; nothing was moved", len(open))
	}
	in := open[0]
	rep.OpID, rep.Account, rep.Outgoing, rep.Target = in.OpID, in.RequestedAccount, in.Outgoing, &in.Target
	layout, err := e.observeLayout(in)
	if err != nil {
		return errs.NewIO("could not read the interrupted operation's directories", err)
	}
	rep.Layout = layout.rows
	rep.State = e.classify(in, layout)

	switch rep.State {
	case stateInconsistent:
		return refuse(desktopRepairInconsistent, "the directories fit no state of operation %s; nothing was moved", in.OpID)
	case stateNeverStarted:
		res := e.observedResult(in, desktop.OutcomeAborted, wordRunUseAgain, nil, false)
		if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
			rep.Journal = journalOpen
			return refuse(wordJournalNotRecorded, "could not record the result: %v", err)
		}
		rep.Outcome, rep.Journal, rep.Detail = wordRunUseAgain, string(desktop.OutcomeAborted), "the switch never moved anything; run `agentctl claude desktop use` again"
		return nil
	case stateOutgoingMoved, stateUnexpectedDefault:
		if action == actionNone {
			return refuse(wordNeedsChoice, "operation %s stopped at %s; pass --complete to finish it or --revert to undo it", in.OpID, rep.State)
		}
	}
	if !yes && (action != actionNone || rep.State != stateTargetInstalled) {
		ok, err := e.prompt.Confirm(ctx, repairQuestion(rep.State, action, in))
		if err != nil {
			return err
		}
		if !ok {
			return errs.NewRefused(0, "the repair was not confirmed; nothing was moved")
		}
	}

	snap, err := e.observe(ctx)
	if err != nil {
		return err
	}
	if len(snap.OtherCopies) > 0 {
		return quitRefusal(&rdesktop.OtherCopyError{Procs: snap.OtherCopies})
	}
	before := snap.Mains
	outcome := desktop.OutcomeApplied
	if action == actionRevert {
		outcome = desktop.OutcomeReverted
	}

	switch {
	case rep.State == stateTargetInstalled && action != actionRevert:
		if len(snap.Mains) > 0 {
			return e.recordRepair(rep, in, outcome, &snap.Mains[0])
		}
		return e.repairLaunch(ctx, rep, in, outcome, before)
	case rep.State == stateTargetInstalled:
		if before, err = e.quitForRepair(ctx); err != nil {
			return err
		}
		if err := e.moveTargetBack(in); err != nil {
			return e.moveFailed(rep, err)
		}
		if err := e.returnOutgoing(in); err != nil {
			return e.moveFailed(rep, err)
		}
	case rep.State == stateUnexpectedDefault:
		if before, err = e.quitForRepair(ctx); err != nil {
			return err
		}
		if _, err := desktop.Move(e.layout.DefaultData, e.layout.Profile(desktop.StrayName(in.OpID)), &layout.data.id); err != nil {
			return e.moveFailed(rep, err)
		}
		fallthrough
	default:
		if err := e.finishOutgoingMoved(in, action); err != nil {
			return e.moveFailed(rep, err)
		}
	}
	return e.repairLaunch(ctx, rep, in, outcome, before)
}

// finishOutgoingMoved puts the chosen profile at the empty data path: the
// stored target for --complete (none for an account without one, which the
// app creates), the outgoing profile for --revert.
func (e *desktopEnv) finishOutgoingMoved(in *desktop.Intent, action repairAction) error {
	if action == actionRevert {
		return e.returnOutgoing(in)
	}
	if !in.Target.Existed {
		return nil
	}
	_, err := desktop.Move(in.Target.Source, e.layout.DefaultData, in.Target.File)
	return err
}

// returnOutgoing moves the outgoing profile back to the data path.
func (e *desktopEnv) returnOutgoing(in *desktop.Intent) error {
	if in.Outgoing == nil {
		return nil
	}
	_, err := desktop.Move(in.Outgoing.Destination, e.layout.DefaultData, &in.Outgoing.File)
	return err
}

// quitForRepair quits the app before repair moves a directory the app may
// hold, and returns the mains that ran before.
func (e *desktopEnv) quitForRepair(ctx context.Context) ([]rdesktop.Proc, error) {
	quit, err := e.sys.Quit(ctx)
	if err != nil {
		return nil, quitRefusal(err)
	}
	return quit.Before.Mains, nil
}

// moveFailed reports a move inside repair that did not land: the intent
// stays open and the layout is reported again.
func (e *desktopEnv) moveFailed(rep *desktopReport, err error) error {
	rep.Journal = journalOpen
	return refuse(desktopRepairInconsistent, "a move failed (%v); the intent stays open, run `agentctl claude desktop repair` again to see the layout", err)
}

// repairLaunch starts the app after the repair's moves and records the
// cell's outcome. Repair never rolls back: the moves were the recovery.
func (e *desktopEnv) repairLaunch(ctx context.Context, rep *desktopReport, in *desktop.Intent, outcome desktop.Outcome, before []rdesktop.Proc) error {
	launch := e.sys.Launch(ctx, before)
	crashPoint(e.fault.Is(fault.DesktopAbortBeforeResult))
	word := wordRepaired
	if !launch.Ready {
		word = wordRepairedNotRunning
	}
	res := e.observedResult(in, outcome, word, launch.Main, launch.Ready)
	res.Verified = launch.Ready
	rep.Main = res.Main
	if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
		rep.Outcome, rep.Journal = wordJournalNotRecorded, journalOpen
		return refuse(wordJournalNotRecorded, "the repair moved the profiles but its result was not recorded (%v); run `agentctl claude desktop repair` again", err)
	}
	rep.Outcome, rep.Journal = word, string(outcome)
	if outcome == desktop.OutcomeApplied {
		rep.Identity = observedWord(res, in)
	}
	if !launch.Ready {
		return refuse(wordRepairedNotRunning, "the profiles are in place (%s) but the app did not start", outcome)
	}
	return nil
}

// recordRepair records the cell's outcome when an instance already runs
// on the installed target.
func (e *desktopEnv) recordRepair(rep *desktopReport, in *desktop.Intent, outcome desktop.Outcome, main *rdesktop.Proc) error {
	res := e.observedResult(in, outcome, wordRepaired, main, true)
	res.Verified = true
	rep.Main = res.Main
	if err := e.appendRecord(res, e.fault.Is(fault.DesktopResultAppendFail)); err != nil {
		rep.Outcome, rep.Journal = wordJournalNotRecorded, journalOpen
		return refuse(wordJournalNotRecorded, "could not record the result: %v", err)
	}
	rep.Outcome, rep.Journal, rep.Identity = wordRepaired, string(outcome), observedWord(res, in)
	return nil
}

// observedWord is the identity word of what the result observed at the data
// path, relative to the requested account.
func observedWord(res desktop.Result, in *desktop.Intent) string {
	if res.Observed == nil {
		return string(desktop.WordFresh)
	}
	switch res.Observed.State {
	case desktop.StateUUID:
		if res.Observed.UUID == in.RequestedAccount {
			return string(desktop.WordMatches)
		}
		return string(desktop.WordMismatch)
	default:
		return string(res.Observed.State)
	}
}

// repairQuestion names what repair is about to do.
func repairQuestion(state string, action repairAction, in *desktop.Intent) string {
	verb := map[repairAction]string{actionNone: "finish", actionComplete: "finish", actionRevert: "undo"}[action]
	return fmt.Sprintf("Operation %s stopped at %s. Quit the Claude desktop app if it runs, %s the switch to account %s by moving the profiles, and start the app?", in.OpID, state, verb, in.RequestedAccount)
}
