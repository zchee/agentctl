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

	"github.com/zchee/agentctl/internal/config"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/render"
)

const resendSpent = "its one --resend is spent; run agentctl codex login"

type markerKind uint8

const (
	markerClear markerKind = iota
	markerDead
	markerUnknown
	markerUnavailable
)

type markerView struct {
	kind     markerKind
	since    time.Time
	class    codexprovider.RefreshUnknownClass
	resendAt *time.Time
	reason   string
}

func unknownState(since time.Time, class codexprovider.RefreshUnknownClass, resendAt *time.Time, now time.Time) (codexprovider.State, string) {
	eligible := resendAt != nil && !resendAt.After(now)
	note := resendSpent
	if eligible {
		note = "agentctl codex accounts refresh --resend is available (read the risk it names first)"
	} else if resendAt != nil {
		note = "--resend is available after " + render.FormatInstant(*resendAt)
	}
	return codexprovider.State{Kind: codexprovider.StateRefreshUnknown, Since: since, Class: class.Label(), ResendEligible: eligible}, note
}

func stepState(step codexprovider.RefreshStep) (codexprovider.State, *string, bool) {
	state := codexprovider.State{}
	var note *string
	switch step.Kind {
	case codexprovider.RefreshStepNeedsLogin:
		state.Kind = codexprovider.StateNeedsLogin
		reason := "no credential; run agentctl codex login"
		switch step.Reason {
		case "no_refresh_token":
			reason = "no refresh token; run agentctl codex login"
		case "dead":
			reason = "the token host rejected this grant; run agentctl codex login"
		case "resend_rejected":
			reason = "refresh outcome unknown; " + resendSpent
		}
		note = &reason
	case codexprovider.RefreshStepOutcomeUnknown:
		var text string
		state, text = unknownState(step.Since, step.Class, step.ResendEligibleAt, time.Now())
		note = &text
	case codexprovider.RefreshStepStateUnavailable:
		state = codexprovider.State{Kind: codexprovider.StateRefreshUnavailable, Reason: step.Reason}
	case codexprovider.RefreshStepSessionDetected:
		state = codexprovider.State{Kind: codexprovider.StateSessionDetected, Evidence: fmt.Sprintf("pid %d alive", step.PID)}
	case codexprovider.RefreshStepBusy:
		state.Kind = codexprovider.StateBusy
	case codexprovider.RefreshStepUnauthorizedTerminal:
		state.Kind = codexprovider.StateUnauthorizedTerminal
	case codexprovider.RefreshStepFailed:
		state = codexprovider.State{Kind: codexprovider.StateError, Reason: step.Reason}
	default:
		return state, nil, false
	}
	return state, note, true
}

func stepNote(step codexprovider.RefreshStep) *string {
	switch step.Kind {
	case codexprovider.RefreshStepStale:
		reason := step.Reason
		switch step.Reason {
		case "not_enough_time":
			reason = "not enough time left in this pass"
		case "torn":
			reason = "auth.json was being rewritten"
		case "daemon_record_unreadable":
			reason = "a Codex daemon record is unreadable"
		case "changed_before_post":
			reason = "the file changed before the send"
		case "pre_send":
			reason = "not sent (" + step.Detail + "); retried next pass"
		case "rejected":
			reason = fmt.Sprintf("the token host answered %d", step.Status)
		}
		return new("no refresh sent: " + reason)
	case codexprovider.RefreshStepNotBefore:
		return new("the token host allows no refresh before " + render.FormatInstant(step.Until) + "; run agentctl codex login")
	case codexprovider.RefreshStepDisabled:
		return new("refresh policy is never")
	default:
		return nil
	}
}

func pushReportNotes(note **string, report *codexprovider.RefreshReport) {
	for _, item := range report.Notes {
		text := ""
		switch item.Kind {
		case codexprovider.RefreshNoteAuditLogRefused:
			text = "audit log refused"
		case codexprovider.RefreshNoteCodexSession:
			text = "codex session detected (" + item.Evidence + ")"
		case codexprovider.RefreshNoteIdentityDrift:
			text = "identity drift"
		case codexprovider.RefreshNoteIDTokenUnreadable:
			text = "the new id token did not decode"
		case codexprovider.RefreshNotePendingReplayed:
			text = "pending replayed"
		case codexprovider.RefreshNotePendingDiscarded:
			text = "pending discarded"
		case codexprovider.RefreshNoteStaleMarkerCleared:
			continue
		}
		pushNote(note, text)
	}
	switch report.Step.Kind {
	case codexprovider.RefreshStepRefreshed:
		if report.Step.Parked {
			pushNote(note, "refreshed (saved as pending)")
		} else {
			pushNote(note, "refreshed")
		}
	case codexprovider.RefreshStepRacedExternal:
		pushNote(note, "refresh raced an external writer")
	case codexprovider.RefreshStepDiscardedExternal:
		pushNote(note, "refresh discarded (external writer)")
	}
}

func expiredState(kind codexprovider.RowKind, record *config.CodexAccountRecord, allowPost bool, prePass *codexprovider.RefreshReport) (codexprovider.State, *string) {
	if kind != codexprovider.RowOwned {
		return codexprovider.State{Kind: codexprovider.StateExpired, Reason: "read-only; run codex to refresh"}, nil
	}
	if record != nil && record.Kind.Owned != nil && record.Kind.Owned.Refresh == config.RefreshNever {
		return codexprovider.State{Kind: codexprovider.StateExpired, Reason: "run agentctl codex login"}, nil
	}
	if !allowPost {
		return codexprovider.State{Kind: codexprovider.StateExpired, Reason: "run agentctl codex status"}, nil
	}
	if prePass != nil {
		if state, note, ok := stepState(prePass.Step); ok {
			return state, note
		}
		return codexprovider.State{Kind: codexprovider.StateStale}, stepNote(prePass.Step)
	}
	return codexprovider.State{Kind: codexprovider.StateStale}, nil
}

func daemonLabel(evidence codexprovider.Daemon) string {
	switch evidence.Kind {
	case codexprovider.DaemonAlive:
		return fmt.Sprintf("pid %d alive", evidence.PID)
	case codexprovider.DaemonRecycled:
		return fmt.Sprintf("pid %d recycled", evidence.PID)
	case codexprovider.DaemonArtefact:
		return "artefact only"
	case codexprovider.DaemonUnreadable:
		return "record unreadable"
	default:
		return "none"
	}
}
