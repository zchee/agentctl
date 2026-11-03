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
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	provider "github.com/zchee/agentctl/internal/provider/codex"
)

func TestRefreshStepState(t *testing.T) {
	tests := map[string]struct {
		step  provider.RefreshStep
		state provider.State
		note  *string
		ok    bool
	}{
		"success: absent credential":     {step: provider.RefreshStep{Kind: provider.RefreshStepNeedsLogin, Reason: "absent"}, state: provider.State{Kind: provider.StateNeedsLogin}, note: new("no credential; run agentctl codex login"), ok: true},
		"success: missing refresh token": {step: provider.RefreshStep{Kind: provider.RefreshStepNeedsLogin, Reason: "no_refresh_token"}, state: provider.State{Kind: provider.StateNeedsLogin}, note: new("no refresh token; run agentctl codex login"), ok: true},
		"success: dead grant":            {step: provider.RefreshStep{Kind: provider.RefreshStepNeedsLogin, Reason: "dead"}, state: provider.State{Kind: provider.StateNeedsLogin}, note: new("the token host rejected this grant; run agentctl codex login"), ok: true},
		"success: rejected resend":       {step: provider.RefreshStep{Kind: provider.RefreshStepNeedsLogin, Reason: "resend_rejected"}, state: provider.State{Kind: provider.StateNeedsLogin}, note: new("refresh outcome unknown; its one --resend is spent; run agentctl codex login"), ok: true},
		"success: unknown login reason":  {step: provider.RefreshStep{Kind: provider.RefreshStepNeedsLogin, Reason: "future"}, state: provider.State{Kind: provider.StateNeedsLogin}, note: new("no credential; run agentctl codex login"), ok: true},
		"success: unknown outcome":       {step: provider.RefreshStep{Kind: provider.RefreshStepOutcomeUnknown, Class: provider.RefreshUnknownServerError}, state: provider.State{Kind: provider.StateRefreshUnknown, Class: "server_error"}, note: new("its one --resend is spent; run agentctl codex login"), ok: true},
		"success: unavailable marker":    {step: provider.RefreshStep{Kind: provider.RefreshStepStateUnavailable, Reason: "unreadable"}, state: provider.State{Kind: provider.StateRefreshUnavailable, Reason: "unreadable"}, ok: true},
		"success: live session":          {step: provider.RefreshStep{Kind: provider.RefreshStepSessionDetected, PID: 123}, state: provider.State{Kind: provider.StateSessionDetected, Evidence: "pid 123 alive"}, ok: true},
		"success: busy":                  {step: provider.RefreshStep{Kind: provider.RefreshStepBusy}, state: provider.State{Kind: provider.StateBusy}, ok: true},
		"success: terminal unauthorized": {step: provider.RefreshStep{Kind: provider.RefreshStepUnauthorizedTerminal}, state: provider.State{Kind: provider.StateUnauthorizedTerminal}, ok: true},
		"success: failure":               {step: provider.RefreshStep{Kind: provider.RefreshStepFailed, Reason: "read failed"}, state: provider.State{Kind: provider.StateError, Reason: "read failed"}, ok: true},
		"success: unprojected step":      {step: provider.RefreshStep{Kind: provider.RefreshStepRefreshed}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			state, note, ok := stepState(test.step)
			if diff := gocmp.Diff(test.state, state); diff != "" {
				t.Errorf("state (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(test.note, note); diff != "" {
				t.Errorf("note (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(test.ok, ok); diff != "" {
				t.Errorf("projected (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshStepNote(t *testing.T) {
	tests := map[string]struct {
		step provider.RefreshStep
		want *string
	}{
		"success: insufficient budget":  {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "not_enough_time"}, new("no refresh sent: not enough time left in this pass")},
		"success: torn credential":      {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "torn"}, new("no refresh sent: auth.json was being rewritten")},
		"success: unreadable daemon":    {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "daemon_record_unreadable"}, new("no refresh sent: a Codex daemon record is unreadable")},
		"success: changed credential":   {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "changed_before_post"}, new("no refresh sent: the file changed before the send")},
		"success: request not sent":     {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "pre_send", Detail: "connection refused"}, new("no refresh sent: not sent (connection refused); retried next pass")},
		"success: rejected request":     {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "rejected", Status: 400}, new("no refresh sent: the token host answered 400")},
		"success: cancelled":            {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "cancelled"}, new("no refresh sent: cancelled")},
		"success: unknown stale reason": {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "future"}, new("no refresh sent: future")},
		"success: server refresh floor": {provider.RefreshStep{Kind: provider.RefreshStepNotBefore, Until: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, new("the token host allows no refresh before 2026-01-02T03:04:05Z; run agentctl codex login")},
		"success: disabled policy":      {provider.RefreshStep{Kind: provider.RefreshStepDisabled}, new("refresh policy is never")},
		"success: step without note":    {provider.RefreshStep{Kind: provider.RefreshStepRefreshed}, nil},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, stepNote(test.step)); diff != "" {
				t.Fatalf("step note (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDaemonLabel(t *testing.T) {
	tests := map[string]struct {
		daemon provider.Daemon
		want   string
	}{
		"success: live":       {provider.Daemon{Kind: provider.DaemonAlive, PID: 123}, "pid 123 alive"},
		"success: recycled":   {provider.Daemon{Kind: provider.DaemonRecycled, PID: 123}, "pid 123 recycled"},
		"success: artefact":   {provider.Daemon{Kind: provider.DaemonArtefact}, "artefact only"},
		"success: unreadable": {provider.Daemon{Kind: provider.DaemonUnreadable}, "record unreadable"},
		"success: none":       {provider.Daemon{Kind: provider.DaemonNone}, "none"},
		"success: unknown":    {provider.Daemon{Kind: "future"}, "none"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, daemonLabel(test.daemon)); diff != "" {
				t.Fatalf("daemon label (-want +got):\n%s", diff)
			}
		})
	}
}
