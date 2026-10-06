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

package codex

import (
	"fmt"
	"time"

	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/errs"
	provider "github.com/zchee/agentctl/internal/provider/codex"
)

func reportResend(prompt commands.Prompter, shown string, report provider.RefreshReport) error {
	for _, note := range report.Notes {
		if err := prompt.Tell("note: " + refreshNoteLine(note.Kind)); err != nil {
			return err
		}
	}
	step := report.Step
	var line string
	var result error
	switch step.Kind {
	case provider.RefreshStepRefreshed:
		if step.Parked {
			line = shown + ": the token host answered with a new grant; it is parked as pending and the next pass puts it in place"
		} else {
			line = shown + ": the token host answered with a new grant, and it is stored"
		}
	case provider.RefreshStepAdopted:
		line = shown + ": nothing was sent — " + refreshAdoptedLine(step.Reason)
	case provider.RefreshStepResendRefused:
		return errs.NewRefused(0, shown+": "+refreshResendRefusal(shown, step))
	case provider.RefreshStepBusy:
		return errs.NewRefused(0, fmt.Sprintf("another agentctl holds `%s`'s namespace; nothing was sent. Try again when it has finished", shown))
	case provider.RefreshStepDisabled:
		return errs.NewRefused(0, fmt.Sprintf("`%s` is set to `never` refresh; `agentctl codex accounts set %s --refresh auto` is what re-arms it", shown, shown))
	case provider.RefreshStepSessionDetected:
		return errs.NewRefused(0, fmt.Sprintf("a Codex process (%d) is using `%s`'s namespace; agentctl refreshes a grant only when nothing else has it open", step.PID, shown))
	case provider.RefreshStepStateUnavailable:
		return errs.NewRefused(0, fmt.Sprintf("`%s`'s refresh marker could not be read or written, so nothing was sent: %s", shown, step.Reason))
	case provider.RefreshStepNotBefore:
		return errs.NewRefused(0, fmt.Sprintf("the token host asked for no refresh of `%s`'s grant before %s", shown, localRefreshTime(step.Until)))
	case provider.RefreshStepUnauthorizedFloor:
		return errs.NewRefused(0, fmt.Sprintf("`%s` is inside its 401 refresh floor until %s; nothing was sent", shown, localRefreshTime(step.Until)))
	case provider.RefreshStepUnauthorizedTerminal:
		return errs.NewRefused(0, fmt.Sprintf("three sent refreshes did not help `%s`; `agentctl codex accounts refresh %s --reset-floor` lifts that, and `agentctl codex login` replaces the grant", shown, shown))
	case provider.RefreshStepStale:
		return errs.NewRefused(0, fmt.Sprintf("nothing was sent for `%s`: %s", shown, refreshStaleLine(step)))
	case provider.RefreshStepOutcomeUnknown:
		line = fmt.Sprintf("%s: the re-send was answered with `%s`, so its outcome is still unknown (since %s). Its one re-send is spent; `agentctl codex login` replaces the grant", shown, step.Class.Label(), localRefreshTime(step.Since))
		result = errs.NewPartial(1)
	case provider.RefreshStepNeedsLogin:
		line = shown + ": " + refreshNeedsLoginLine(step.Reason)
		result = errs.NewPartial(1)
	case provider.RefreshStepRacedExternal:
		line = shown + ": the token host called the grant that was sent dead while another writer's newer one was in the file; the newer grant was kept"
		result = errs.NewPartial(1)
	case provider.RefreshStepDiscardedExternal:
		line = shown + ": another writer's grant was in the file, so the response was discarded"
		result = errs.NewPartial(1)
	case provider.RefreshStepFailed:
		return errs.NewRefused(0, fmt.Sprintf("`%s`'s re-send did not run: %s", shown, step.Reason))
	default:
		return errs.NewConfig("the refresh driver returned an unrecognized state")
	}
	if err := prompt.Tell(line); err != nil {
		return err
	}
	return result
}

func refreshResendRefusal(shown string, step provider.RefreshStep) string {
	switch step.Reason {
	case "not_unknown":
		return "its last refresh outcome is not unknown, so there is nothing to re-send. agentctl re-sends a refresh token only when it does not know what its own last send did"
	case "already_resent":
		return "its one re-send is already spent. agentctl sends a grant at most twice — once on its own and once because you asked — so `agentctl codex login` is what remains"
	case "too_early":
		return fmt.Sprintf("agentctl waits an hour after a send whose outcome it does not know, in case an answer is still on its way; `%s` becomes eligible at %s", shown, localRefreshTime(step.Until))
	case "stray_tmp":
		return fmt.Sprintf("a staged temporary is lying in `%s`'s namespace and may hold a grant the token host already rotated. `agentctl codex doctor` names the file; nothing was sent until it is dealt with", shown)
	default:
		return "the refresh driver did not authorize a re-send"
	}
}

func refreshAdoptedLine(reason string) string {
	switch reason {
	case "fresh":
		return "the stored access token is not due for a refresh"
	case "external_access":
		return "another writer had already refreshed this grant, and agentctl adopted it"
	case "changed_before_post":
		return "the credential changed before the send, and the grant now in it is fresh"
	default:
		return "the stored grant did not need a refresh"
	}
}

func refreshStaleLine(step provider.RefreshStep) string {
	switch step.Reason {
	case "not_enough_time":
		return "there was not enough time left for the lock, the request and the write"
	case "cancelled":
		return "the command was cancelled before the send"
	case "torn":
		return "the credential was being rewritten and stayed unreadable"
	case "daemon_record_unreadable":
		return "a Codex daemon's record exists and cannot be read, so agentctl cannot tell whether the namespace is in use"
	case "changed_before_post":
		return "the credential changed between the read and the send"
	case "pre_send":
		return fmt.Sprintf("the request never left this machine (%s), so the grant is untouched", step.Detail)
	case "rejected":
		return fmt.Sprintf("the token host answered %d before processing the request, so the grant is taken as unused", step.Status)
	default:
		return "a refresh prerequisite was not met"
	}
}

func refreshNeedsLoginLine(reason string) string {
	switch reason {
	case "absent":
		return "agentctl holds no credential for it; `agentctl codex login` adds one"
	case "no_refresh_token":
		return "its stored credential carries no refresh token; `agentctl codex login` replaces it"
	case "dead":
		return "the token host called this grant dead; `agentctl codex login` replaces it"
	case "resend_rejected":
		return "the token host refused the re-send, and the re-send is spent; `agentctl codex login` replaces the grant"
	default:
		return "the stored grant needs a new login; `agentctl codex login` replaces it"
	}
}

func refreshNoteLine(kind provider.RefreshNoteKind) string {
	switch kind {
	case provider.RefreshNoteAuditLogRefused:
		return "the audit log refused an entry; the write it describes still happened"
	case provider.RefreshNoteCodexSession:
		return "a Codex session is using this namespace"
	case provider.RefreshNoteIdentityDrift:
		return "the new id token names another account"
	case provider.RefreshNoteIDTokenUnreadable:
		return "the new id token could not be decoded, and the stored one was kept"
	case provider.RefreshNotePendingReplayed:
		return "a parked credential was put in place first"
	case provider.RefreshNotePendingDiscarded:
		return "a parked credential was discarded first"
	case provider.RefreshNoteStaleMarkerCleared:
		return "a marker for an older grant was cleared"
	default:
		return "the refresh driver reported an unrecognized note"
	}
}

func localRefreshTime(at time.Time) string { return at.In(time.Local).Format("2006-01-02 15:04 MST") }
