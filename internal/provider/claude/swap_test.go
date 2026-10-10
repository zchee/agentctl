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

import "testing"

func TestSwapRefusalVocabulary(t *testing.T) {
	tests := map[string]struct {
		kind           SwapRefusalKind
		letter, reason string
		exit           int
	}{
		"error: compromised":                {SwapCompromisedHold, "A", "", 10},
		"error: token environment":          {SwapEnvToken, "C", "", 11},
		"error: line bound":                 {SwapLineTooLong, "D", "", 12},
		"error: namespace environment":      {SwapLiveNamespaceEnv, "E", "", 13},
		"error: adoption":                   {SwapCannotAdopt, "F", "", 14},
		"error: ownership":                  {SwapNotOwned, "", "not_owned", 15},
		"error: audit":                      {SwapAuditRefused, "", "audit_refused", 22},
		"error: unreachable":                {SwapLiveUnreachable, "", "live_unreachable", 23},
		"error: absent item":                {SwapLiveItemAbsent, "", "live_item_absent", 24},
		"error: foreign login":              {SwapLiveUndoForeignLogin, "", "live_undo_foreign_login", 27},
		"error: profile":                    {SwapProfileUnavailable, "", "profile_unavailable", 29},
		"error: expired":                    {SwapTokenExpired, "", "live_token_expired", 29},
		"error: remote control platform":    {SwapRemoteControlUnsupportedPlatform, "", "remote_control_unsupported_platform", 30},
		"error: remote control unreachable": {SwapRemoteControlUnreachable, "", "remote_control_unreachable", 30},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			refusal := SwapRefusal{Kind: tt.kind, Adoption: AdoptionNewerCopy}
			if refusal.Letter() != tt.letter || refusal.Reason() != tt.reason || refusal.ExitCode() != tt.exit {
				t.Fatalf("got letter=%q reason=%q exit=%d, want %+v", refusal.Letter(), refusal.Reason(), refusal.ExitCode(), tt)
			}
			outcome := SwapOutcome{Kind: SwapRefused, Refusal: refusal}
			if outcome.Word() != "refused" || outcome.ExitCode() != tt.exit {
				t.Fatalf("outcome %+v, exit=%d", outcome, outcome.ExitCode())
			}
		})
	}
}

func TestSwapOutcomeExitCodes(t *testing.T) {
	tests := map[string]struct {
		kind SwapOutcomeKind
		word string
		exit int
	}{
		"success: applied":     {SwapApplied, "applied", 0},
		"success: active":      {SwapAlreadyActive, "already_active", 0},
		"error: busy":          {SwapBusy, "busy", 16},
		"error: discarded":     {SwapDiscarded, "discarded", 17},
		"error: unknown":       {SwapUnknown, "unknown", 18},
		"error: failed":        {SwapFailed, "failed", 19},
		"error: cancelled":     {SwapCancelled, "cancelled", 20},
		"error: needs refresh": {SwapNeedsRefresh, "needs_refresh", 21},
		"error: invalid":       {"", "", 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			o := SwapOutcome{Kind: tt.kind}
			if o.Word() != tt.word || o.ExitCode() != tt.exit {
				t.Fatalf("got %q/%d, want %q/%d", o.Word(), o.ExitCode(), tt.word, tt.exit)
			}
		})
	}
}
