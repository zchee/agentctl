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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	provider "github.com/zchee/agentctl/internal/provider/codex"
)

func TestRefreshAdoptedLine(t *testing.T) {
	tests := map[string]struct{ reason, want string }{
		"success: fresh":               {"fresh", "the stored access token is not due for a refresh"},
		"success: external access":     {"external_access", "another writer had already refreshed this grant, and agentctl adopted it"},
		"success: changed before post": {"changed_before_post", "the credential changed before the send, and the grant now in it is fresh"},
		"success: unknown reason":      {"future", "the stored grant did not need a refresh"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, refreshAdoptedLine(test.reason)); diff != "" {
				t.Fatalf("adopted explanation (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshStaleLine(t *testing.T) {
	tests := map[string]struct {
		step provider.RefreshStep
		want string
	}{
		"success: insufficient budget": {provider.RefreshStep{Reason: "not_enough_time"}, "there was not enough time left for the lock, the request and the write"},
		"success: cancelled":           {provider.RefreshStep{Reason: "cancelled"}, "the command was cancelled before the send"},
		"success: torn credential":     {provider.RefreshStep{Reason: "torn"}, "the credential was being rewritten and stayed unreadable"},
		"success: unreadable daemon":   {provider.RefreshStep{Reason: "daemon_record_unreadable"}, "a Codex daemon's record exists and cannot be read, so agentctl cannot tell whether the namespace is in use"},
		"success: changed credential":  {provider.RefreshStep{Reason: "changed_before_post"}, "the credential changed between the read and the send"},
		"success: request not sent":    {provider.RefreshStep{Reason: "pre_send", Detail: "connection refused"}, "the request never left this machine (connection refused), so the grant is untouched"},
		"success: rejected request":    {provider.RefreshStep{Reason: "rejected", Status: 400}, "the token host answered 400 before processing the request, so the grant is taken as unused"},
		"success: unknown reason":      {provider.RefreshStep{Reason: "future"}, "a refresh prerequisite was not met"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, refreshStaleLine(test.step)); diff != "" {
				t.Fatalf("stale explanation (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshNeedsLoginLine(t *testing.T) {
	tests := map[string]struct{ reason, want string }{
		"success: absent":                {"absent", "agentctl holds no credential for it; `agentctl codex login` adds one"},
		"success: missing refresh token": {"no_refresh_token", "its stored credential carries no refresh token; `agentctl codex login` replaces it"},
		"success: dead grant":            {"dead", "the token host called this grant dead; `agentctl codex login` replaces it"},
		"success: refused resend":        {"resend_rejected", "the token host refused the re-send, and the re-send is spent; `agentctl codex login` replaces the grant"},
		"success: unknown reason":        {"future", "the stored grant needs a new login; `agentctl codex login` replaces it"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, refreshNeedsLoginLine(test.reason)); diff != "" {
				t.Fatalf("login explanation (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshNoteLine(t *testing.T) {
	tests := map[string]struct {
		kind provider.RefreshNoteKind
		want string
	}{
		"success: audit refusal":        {provider.RefreshNoteAuditLogRefused, "the audit log refused an entry; the write it describes still happened"},
		"success: codex session":        {provider.RefreshNoteCodexSession, "a Codex session is using this namespace"},
		"success: identity drift":       {provider.RefreshNoteIdentityDrift, "the new id token names another account"},
		"success: unreadable id token":  {provider.RefreshNoteIDTokenUnreadable, "the new id token could not be decoded, and the stored one was kept"},
		"success: pending replayed":     {provider.RefreshNotePendingReplayed, "a parked credential was put in place first"},
		"success: pending discarded":    {provider.RefreshNotePendingDiscarded, "a parked credential was discarded first"},
		"success: stale marker cleared": {provider.RefreshNoteStaleMarkerCleared, "a marker for an older grant was cleared"},
		"success: unknown note":         {provider.RefreshNoteKind("future"), "the refresh driver reported an unrecognized note"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, refreshNoteLine(test.kind)); diff != "" {
				t.Fatalf("refresh note (-want +got):\n%s", diff)
			}
		})
	}
}
