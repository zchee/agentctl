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

package claude

import "github.com/zchee/agentctl/internal/cli"

// SwapRefusalKind names a condition that prohibits a swap.
type SwapRefusalKind uint8

const (
	SwapCompromisedHold SwapRefusalKind = iota + 1
	SwapEnvToken
	SwapLineTooLong
	SwapCannotAdopt
	SwapNotOwned
	SwapLiveNamespaceEnv
	SwapLiveUnreachable
	SwapLiveItemAbsent
	SwapAuditRefused
	SwapRemoteControlNotDisconnected
	SwapProfileUnavailable
	SwapTokenExpired
	SwapLiveUndoForeignLogin
)

// SwapRefusal carries the reason without credential material.
type SwapRefusal struct {
	Kind     SwapRefusalKind
	Adoption AdoptionRefusal
}

// Letter returns a lettered refusal's public token, or empty for an unlettered one.
func (r SwapRefusal) Letter() string {
	switch r.Kind {
	case SwapCompromisedHold:
		return "A"
	case SwapEnvToken:
		return "C"
	case SwapLineTooLong:
		return "D"
	case SwapLiveNamespaceEnv:
		return "E"
	case SwapCannotAdopt:
		return "F"
	default:
		return ""
	}
}

// Reason returns the public token of an unlettered refusal.
func (r SwapRefusal) Reason() string {
	switch r.Kind {
	case SwapNotOwned:
		return "not_owned"
	case SwapLiveUnreachable:
		return "live_unreachable"
	case SwapLiveItemAbsent:
		return "live_item_absent"
	case SwapAuditRefused:
		return "audit_refused"
	case SwapRemoteControlNotDisconnected:
		return "remote_control_not_disconnected"
	case SwapProfileUnavailable:
		return "profile_unavailable"
	case SwapTokenExpired:
		return "live_token_expired"
	case SwapLiveUndoForeignLogin:
		return "live_undo_foreign_login"
	default:
		return ""
	}
}

// ExitCode is the stable process exit status for the refusal.
func (r SwapRefusal) ExitCode() int {
	switch r.Kind {
	case SwapCompromisedHold:
		return cli.SwapExitRefusedA
	case SwapEnvToken:
		return cli.SwapExitRefusedC
	case SwapLineTooLong:
		return cli.SwapExitRefusedD
	case SwapCannotAdopt:
		return cli.SwapExitRefusedF
	case SwapNotOwned:
		return cli.SwapExitPrecondition
	case SwapLiveNamespaceEnv:
		return cli.SwapExitRefusedE
	case SwapLiveUnreachable:
		return cli.SwapExitLiveUnreachable
	case SwapLiveItemAbsent:
		return cli.SwapExitLiveItemAbsent
	case SwapAuditRefused:
		return cli.SwapExitAuditRefused
	case SwapRemoteControlNotDisconnected:
		return cli.SwapExitRCNotDisconnected
	case SwapProfileUnavailable, SwapTokenExpired:
		return cli.SwapExitIdentityUnavailable
	case SwapLiveUndoForeignLogin:
		return cli.SwapExitLiveUndoItemChanged
	default:
		return 1
	}
}

// SwapOutcomeKind is the word both terminal output and JSON use.
type SwapOutcomeKind string

const (
	SwapApplied       SwapOutcomeKind = "applied"
	SwapAlreadyActive SwapOutcomeKind = "already_active"
	SwapUnknown       SwapOutcomeKind = "unknown"
	SwapFailed        SwapOutcomeKind = "failed"
	SwapCancelled     SwapOutcomeKind = "cancelled"
	SwapNeedsRefresh  SwapOutcomeKind = "needs_refresh"
	SwapDiscarded     SwapOutcomeKind = "discarded"
	SwapBusy          SwapOutcomeKind = "busy"
	SwapRefused       SwapOutcomeKind = "refused"
)

// SwapOutcome is the result, with a refusal only when Kind is SwapRefused.
type SwapOutcome struct {
	Kind    SwapOutcomeKind
	Refusal SwapRefusal
}

// Word returns the stable output word.
func (o SwapOutcome) Word() string { return string(o.Kind) }

// ExitCode reports success only for an applied or already-active credential.
func (o SwapOutcome) ExitCode() int {
	switch o.Kind {
	case SwapApplied, SwapAlreadyActive:
		return 0
	case SwapUnknown:
		return cli.SwapExitUnknown
	case SwapFailed:
		return cli.SwapExitWriteFailed
	case SwapCancelled:
		return cli.SwapExitCancelled
	case SwapNeedsRefresh:
		return cli.SwapExitNeedsRefresh
	case SwapDiscarded:
		return cli.SwapExitDiscarded
	case SwapBusy:
		return cli.SwapExitBusy
	case SwapRefused:
		return o.Refusal.ExitCode()
	default:
		return 1
	}
}
