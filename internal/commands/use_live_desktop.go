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
	"slices"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
)

// The swap forms recorded as the command of a desktop switch that follows
// a swap.
const (
	useDesktopForward = "use --live --desktop"
	useDesktopUndo    = "use --undo --desktop"
)

// useDesktopBudget bounds the desktop switch that follows a swap: the quit
// wait, the exchange, the launch wait and a recovery launch fit inside it.
const useDesktopBudget = 60 * time.Second

// useDesktop is the desktop switch's part of a swap report. It is present
// only when `--desktop` was given, and then always. A null outcome means
// the switch did not run, and detail says why; otherwise outcome is one of
// the words `agentctl claude desktop use` reports.
type useDesktop struct {
	Outcome *string `json:"outcome"`
	// Before is what the profile at the data path read as when the switch
	// looked at it, or null when no profile was there or the switch
	// stopped before it looked.
	Before *desktop.IdentityRecord `json:"before"`
	// After is what the result row observed at the data path, or null when
	// no result row was written.
	After *desktop.IdentityRecord `json:"after"`
	// Identity is the target's identity word: matches, fresh or unknown.
	Identity *string `json:"identity"`
	Journal  string  `json:"journal,omitzero"`
	OpID     string  `json:"op_id,omitzero"`
	Detail   string  `json:"detail,omitzero"`
	// leftover names a profile the switch being undone stored under a
	// name of its own drawing, which the undo does not bring back; it is
	// reported as a warning even when the undo finished.
	leftover string
}

// useDesktopFollow names the switch a swap asks for.
type useDesktopFollow struct {
	// account is the account the swap installed, whose desktop profile
	// the switch selects.
	account string
	// command is the swap form recorded in the desktop journal.
	command string
	// undone is the audit id of the swap an undo reverses, or empty for a
	// forward swap.
	undone string
}

// nothingToUndo reports an undo whose swap has no switch to reverse: none
// was recorded for it, or the one recorded did not install anything.
type nothingToUndo struct{ reason string }

func (e *nothingToUndo) Error() string { return e.reason }

// desktopFor returns the desktop process the follow-up drives. Its report
// writer is the swap's error stream, so nothing the switch prints can mix
// into the swap's one JSON document; the switch prints nothing itself.
func (p SessionProcess) desktopFor() DesktopProcess {
	dp := p.Desktop
	if dp.System == nil {
		dp.System = rdesktop.Default
	}
	if dp.LockWait == 0 {
		dp.LockWait = time.Second
	}
	dp.Out, dp.Err = p.Err, p.Err
	return dp
}

// followUpDesktop runs after the swap released every lock, after the
// Remote Control follow-up and before the report is printed. It switches
// the desktop app only after an applied swap. It never changes the swap's
// outcome or exit status: anything but a finished switch adds a warning.
func (p SessionProcess) followUpDesktop(ctx context.Context, opts cli.ClaudeUseOptions, report *useReport, follow useDesktopFollow) {
	if !opts.Desktop {
		return
	}
	if report.outcome.Kind != claude.SwapApplied {
		report.desktop = &useDesktop{Detail: "the swap did not apply, so the desktop app was left alone"}
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, useDesktopBudget)
	defer cancel()
	result := p.switchDesktop(ctx, report, follow)
	report.desktop = result
	slog.DebugContext(ctx, "desktop follow-up", slog.Any("outcome", result.Outcome), slog.Int64("elapsed_ms", time.Since(started).Milliseconds()))
	if warning := useDesktopWarning(result); warning != "" {
		report.warnings = append(report.warnings, warning)
		if err := tell(p.Err, "warning: "+warning); err != nil {
			slog.ErrorContext(ctx, "the desktop warning could not be written", slog.Any("error", err))
		}
	}
}

// switchDesktop runs the switch through the same path as `desktop use`,
// with the consent already given by the swap's question or `--yes`.
func (p SessionProcess) switchDesktop(ctx context.Context, report *useReport, follow useDesktopFollow) *useDesktop {
	if !desktop.ValidUUID(follow.account) {
		return &useDesktop{Detail: fmt.Sprintf("the swapped-in account `%s` has no account UUID the desktop app could record", usePrintable(follow.account))}
	}
	account := desktop.AccountName(follow.account)
	if err := ctx.Err(); err != nil {
		return &useDesktop{Detail: "the swap was interrupted before the desktop step, so nothing was quit or moved"}
	}
	dp := p.desktopFor()
	rep := &desktopReport{Version: 1, Command: "use", Account: account}
	env, err := dp.newEnv()
	if err == nil {
		env.command = follow.command
		if report.store != nil {
			env.store = *report.store
		}
		if report.auditID != nil {
			env.swapAuditID = *report.auditID
		}
		var leftover string
		if follow.undone != "" {
			env.guard = desktopUndoGuard(follow.undone, account, env.layout.DefaultData, &leftover)
		}
		err = env.use(ctx, dp, rep, account, true)
		out := useDesktopOf(rep, env, err)
		if leftover != "" {
			out.leftover = fmt.Sprintf("the profile the switch being undone moved aside is still stored as %s and was not put back, because it was never tied to an account; `agentctl claude desktop forget %s` removes it", leftover, leftover)
			out.Detail = joinDetail(out.Detail, out.leftover)
		}
		return out
	}
	return useDesktopOf(rep, env, err)
}

// joinDetail appends a second sentence to a detail that may be empty.
func joinDetail(detail, more string) string {
	if detail == "" {
		return more
	}
	return detail + "; " + more
}

// useDesktopOf turns what the switch did into the report's object.
func useDesktopOf(rep *desktopReport, env *desktopEnv, err error) *useDesktop {
	if nothing, ok := errors.AsType[*nothingToUndo](err); ok {
		return &useDesktop{Detail: nothing.reason}
	}
	if err != nil && rep.OpID == "" && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return &useDesktop{Detail: "the swap was interrupted before the desktop step, so nothing was quit or moved"}
	}
	settleOutcome(rep, err)
	out := &useDesktop{Outcome: &rep.Outcome, Journal: rep.Journal, OpID: rep.OpID, Detail: rep.Detail}
	if rep.Identity != "" {
		out.Identity = &rep.Identity
	}
	if env != nil {
		out.Before = env.before
		if env.recorded != nil {
			out.After = env.recorded.Observed
		}
	}
	return out
}

// useDesktopSucceeded lists the outcomes after which the app runs on the
// requested profile; every other outcome is reported as a warning.
var useDesktopSucceeded = []string{wordSelected, wordAlreadySelected, wordLaunchedUnverified}

// useDesktopWarning is the warning a switch adds, or empty when it
// finished.
func useDesktopWarning(d *useDesktop) string {
	if d.Outcome == nil {
		return "the desktop app was not switched: " + d.Detail
	}
	if slices.Contains(useDesktopSucceeded, *d.Outcome) {
		return d.leftover
	}
	warning := "the desktop app switch ended " + *d.Outcome
	if d.Detail != "" {
		warning += ": " + d.Detail
	}
	return warning
}

// renderUseDesktop is the swap report's text line for the switch.
func renderUseDesktop(d *useDesktop) string {
	if d.Outcome == nil {
		return "desktop: not switched: " + d.Detail
	}
	parts := []string{"desktop:", *d.Outcome}
	if d.Identity != nil {
		parts = append(parts, *d.Identity)
	}
	if d.OpID != "" {
		parts = append(parts, "operation "+d.OpID)
	}
	return strings.Join(parts, " ")
}

// desktopUndoGuard decides, under the desktop lock, whether the undo of the
// swap with audit id undone may switch the app back. Only a switch recorded
// for that swap as applied is reversed, and only while the data path still
// holds the directory it installed, signed in as the account it observed,
// as the account it switched to (its first sign-in finished since), or not
// signed in at all. Anything else is a profile someone else put there.
//
// When the undo may run and the switch being undone had moved a profile
// aside under a name it drew (fresh-… or unknown-…), leftover receives that
// name: the undo selects the account's own profile and cannot know that
// directory was the one to bring back, so the caller says it stays stored.
func desktopUndoGuard(undone, account, dataPath string, leftover *string) func([]desktop.Entry) error {
	return func(entries []desktop.Entry) error {
		var res *desktop.Result
		for _, entry := range slices.Backward(entries) {
			if entry.Result != nil && entry.Result.SwapAuditID == undone {
				res = entry.Result
				break
			}
		}
		if res == nil {
			return &nothingToUndo{reason: "no desktop switch was recorded for the swap being undone, so nothing was quit or moved"}
		}
		if res.Outcome != desktop.OutcomeApplied {
			return &nothingToUndo{reason: fmt.Sprintf("the desktop switch recorded for the swap being undone (operation %s) ended %s, so there is nothing to put back; nothing was quit or moved", res.OpID, res.Word)}
		}
		if res.Installed == nil {
			return refuse(desktopProfileMismatch, "the desktop switch recorded for the swap being undone (operation %s) found no profile at the data path when it finished, so the profile there now cannot be tied to it; nothing was quit or moved, and `agentctl claude desktop use %s` switches the app back", res.OpID, account)
		}
		found, err := desktop.StatID(dataPath)
		if err != nil {
			return refuse(desktopProfileMismatch, "the desktop switch recorded for the swap being undone (operation %s) installed %s, and the data path now holds no profile that can be read (%v); nothing was quit or moved", res.OpID, res.Installed, err)
		}
		if found != *res.Installed {
			return refuse(desktopProfileMismatch, "the profile at the data path (%s) is not the one the desktop switch for the swap being undone installed (%s, operation %s); nothing was quit or moved", found, res.Installed, res.OpID)
		}
		current := desktop.RecordOf(desktop.ReadDesktopIdentity(dataPath))
		switch {
		case res.Observed != nil && current == *res.Observed:
		case current.State == desktop.StateUUID && current.UUID == res.RequestedAccount:
		case current.State == desktop.StateFresh:
		default:
			return refuse(desktopProfileMismatch, "the profile at the data path now reads as %s, which is neither what the desktop switch for the swap being undone observed, nor the account it switched to, nor a profile no one has signed in to; nothing was quit or moved", useIdentityText(current))
		}
		if in := desktop.IntentFor(entries, res.OpID); in != nil && in.Outgoing != nil && !desktop.ValidUUID(in.Outgoing.Name) {
			*leftover = in.Outgoing.Name
		}
		return nil
	}
}

// useIdentityText names an identity record for a message.
func useIdentityText(id desktop.IdentityRecord) string {
	if id.UUID != "" {
		return "account " + id.UUID
	}
	return string(id.State)
}

// desktopConsentClause is the part of the swap's question that names what
// the desktop switch will do. It is composed from a read-only look taken
// before the swap, because the swap asks its question before it writes
// anything. The switch itself looks again under the desktop lock and
// decides from that second look, so a change between the two (an app
// started or quit, a profile signed in) is acted on as it then is, and the
// switch still refuses anything the question did not cover. Nothing here
// creates the desktop store or takes its lock.
func (p SessionProcess) desktopConsentClause(ctx context.Context, account string, undo bool) string {
	refused := func(why string) string {
		return fmt.Sprintf(". The desktop app switch will be refused (%s); the swap is not affected", why)
	}
	if !desktop.ValidUUID(account) {
		return refused("the account has no account UUID the desktop app could record")
	}
	account = desktop.AccountName(account)
	env, err := p.desktopFor().newEnv()
	if err != nil {
		return refused(useDesktopReason(err))
	}
	if err := env.checkBundle(); err != nil {
		return refused(useDesktopReason(err))
	}
	snap, err := env.observe(ctx)
	if err != nil {
		return refused(useDesktopReason(err))
	}
	in := &desktop.Intent{RequestedAccount: account}
	if present, err := desktop.Exists(env.layout.DefaultData); err != nil {
		return refused("the desktop data path could not be read: " + err.Error())
	} else if present {
		id := desktop.ReadDesktopIdentity(env.layout.DefaultData)
		if id.State == desktop.StateUnreadable {
			return refused(fmt.Sprintf("the profile at the data path cannot be read: %s", id.Reason))
		}
		if id.State == desktop.StateUUID && id.UUID == account {
			return ". The desktop app already runs on that account's profile, so the desktop switch quits and moves nothing"
		}
		in.Outgoing = &desktop.Outgoing{Name: outgoingName(id, "<operation id>"), Identity: desktop.RecordOf(id)}
	}
	name := desktop.AccountName(account)
	stored, err := desktop.Exists(env.layout.Profile(name))
	if err != nil {
		return refused("the stored profile could not be read: " + err.Error())
	}
	in.Target = desktop.Target{Name: name, Existed: stored}
	clause := ". Then " + switchPlan(snap, in)
	if undo {
		clause += ", if the desktop switch recorded for the swap being undone is still in place"
	}
	return clause + "; the switch looks at the app again before it moves anything, and a switch that does not finish leaves the swap as it is"
}

// useDesktopReason is the reason a refusal gives, without its word.
func useDesktopReason(err error) string {
	if refusal, ok := errors.AsType[*desktopRefusal](err); ok {
		return refusal.Word + ": " + refusal.Detail
	}
	return err.Error()
}
