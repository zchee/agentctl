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

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
)

// RunForget deletes one stored, inactive profile with everything in it,
// including the desktop app's local Code sessions and tasks of that
// profile. It refuses while anything could still need the directory.
func (p DesktopProcess) RunForget(ctx context.Context, _ cli.Globals, opts cli.ClaudeDesktopForgetOptions) error {
	rep := &desktopReport{Version: 1, Command: "forget"}
	env, err := p.newEnv()
	if err != nil {
		return finish(p.Out, false, rep, err)
	}
	return finish(p.Out, false, rep, env.forget(ctx, p, rep, opts.Name, opts.Yes))
}

func (e *desktopEnv) forget(ctx context.Context, p DesktopProcess, rep *desktopReport, name string, yes bool) error {
	if _, err := desktop.ParseProfileName(name); err != nil {
		return errs.NewRefused(0, fmt.Sprintf("`%s` is not a stored desktop profile name; `agentctl claude desktop status` lists them", name))
	}
	if err := e.checkLayout(); err != nil {
		return err
	}
	guard, err := e.lock(ctx, p.LockWait)
	if err != nil {
		return err
	}
	defer func() { _ = guard.Release() }()

	present, err := desktop.Exists(e.layout.Profile(name))
	if err != nil {
		return errs.NewIO("could not check the stored profile", err)
	}
	if !present {
		return errs.NewRefused(0, fmt.Sprintf("no stored desktop profile is named `%s`", name))
	}
	snap, err := e.observe(ctx)
	if err != nil {
		return err
	}
	if n := len(snap.Mains) + len(snap.Family) + len(snap.OtherCopies); n > 0 {
		return refuse(desktopAppRunning, "the desktop app runs (%d process(es)); quit it first", n)
	}
	entries, err := desktop.ReadJournal(e.layout.Journal)
	if err != nil {
		return refuse(desktopJournalFailed, "could not read %s: %v", e.layout.Journal, err)
	}
	if open := desktop.OpenIntents(entries); len(open) > 0 {
		return refuse(desktopIntentOpen, "operation %s is interrupted and may need this profile; run `agentctl claude desktop repair` first", open[0].OpID)
	}
	if name == inPlaceName(entries) {
		return refuse(desktopProfileInPlace, "%s is the profile the last switch put at the data path", name)
	}
	if !yes {
		question := fmt.Sprintf("Delete the stored desktop profile %s, including the local Code sessions and tasks in it? This cannot be undone.", name)
		ok, err := e.prompt.Confirm(ctx, question)
		if err != nil {
			return err
		}
		if !ok {
			return errs.NewRefused(0, "the deletion was not confirmed; nothing was deleted")
		}
	}
	err = desktop.RemoveProfile(e.layout.ProfilesDir, name)
	var partial *desktop.PartialRemoveError
	switch {
	case err == nil:
		rep.Outcome = wordForgotten
		rep.Detail = name
		return nil
	case errors.Is(err, desktop.ErrSymlinkInProfile):
		if errors.As(err, &partial) {
			return refuse(wordProfileSymlink, "%s: %v; the walk stopped with %s remaining", name, partial.Err, partial.RemainingText())
		}
		return refuse(wordProfileSymlink, "%s: %v; nothing was deleted", name, err)
	case errors.As(err, &partial) && partial.Remaining > 0:
		rep.Outcome = wordPartial
		return refuse(wordPartial, "%s: %s remain (%v)", name, partial.RemainingText(), partial.Err)
	default:
		return errs.NewIO("could not delete "+name, err)
	}
}
