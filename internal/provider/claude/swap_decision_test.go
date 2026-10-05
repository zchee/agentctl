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

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func TestSwapDecisionPhases(t *testing.T) {
	tests := map[string]struct {
		refusal SwapRefusalKind
		phase   SwapPhase
	}{
		"success: ownership": {SwapNotOwned, SwapReadOnly}, "success: namespace environment": {SwapLiveNamespaceEnv, SwapReadOnly}, "success: missing live tree": {SwapLiveUnreachable, SwapReadOnly}, "success: override token": {SwapEnvToken, SwapReadOnly}, "success: missing item": {SwapLiveItemAbsent, SwapReadOnly}, "success: profile unavailable": {SwapProfileUnavailable, SwapReadOnly}, "success: expired token": {SwapTokenExpired, SwapReadOnly}, "success: foreign login": {SwapLiveUndoForeignLogin, SwapReadOnly}, "success: line size": {SwapLineTooLong, SwapPrepare}, "success: refused audit": {SwapAuditRefused, SwapPrepare}, "success: remote session": {SwapRemoteControlNotDisconnected, SwapPrepare}, "success: adoption": {SwapCannotAdopt, SwapPrepare}, "success: compromised hold": {SwapCompromisedHold, SwapHeld},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := SwapRefusal{Kind: tt.refusal}
			if r.DecidedIn() != tt.phase {
				t.Fatalf("phase=%v want=%v", r.DecidedIn(), tt.phase)
			}
			if r.DecidedIn().HoldsLocks() != (tt.refusal == SwapCompromisedHold) {
				t.Fatal("only compromise may be decided with peer locks held")
			}
		})
	}
	previous := SwapReadOnly
	for _, r := range SwapDecisionOrder() {
		if r.DecidedIn() < previous {
			t.Fatalf("decision order moves backwards: %+v", r)
		}
		previous = r.DecidedIn()
	}
	order := SwapDecisionOrder()
	order[0].Kind = SwapCompromisedHold
	if SwapDecisionOrder()[0].Kind != SwapNotOwned {
		t.Fatal("decision table is mutable")
	}
	if diff := gocmp.Diff([]string{"a", "b", "c"}, []string{SwapReadOnly.Name(), SwapPrepare.Name(), SwapHeld.Name()}); diff != "" {
		t.Fatal(diff)
	}
}

func TestSwapIdentityRules(t *testing.T) {
	tests := map[string]struct {
		identity *Identity
		org      string
		want     bool
	}{
		"success: absent identity":                 {org: "org", want: true},
		"success: both match":                      {identity: &Identity{AccountUUID: "account", OrganizationUUID: new("org")}, org: "org", want: true},
		"error: wrong account":                     {identity: &Identity{AccountUUID: "other", OrganizationUUID: new("org")}, org: "org"},
		"error: wrong organization":                {identity: &Identity{AccountUUID: "account", OrganizationUUID: new("other")}, org: "org"},
		"success: unknown record organization":     {identity: &Identity{AccountUUID: "account", OrganizationUUID: new("org")}, org: config.UnknownOrg, want: true},
		"success: unknown credential organization": {identity: &Identity{AccountUUID: "account"}, org: "org", want: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			record := &config.AccountRecord{AccountUUID: "account", OrganizationUUID: tt.org}
			if got := IdentityIs(tt.identity, record); got != tt.want {
				t.Fatalf("identity match=%v want=%v", got, tt.want)
			}
		})
	}
	a := &Identity{AccountUUID: "a", OrganizationUUID: new("o")}
	b := &Identity{AccountUUID: "a"}
	if !IdentitiesAgree(a, b) || !IdentitiesAgree(b, a) {
		t.Fatal("missing organization is not contradictory")
	}
	b.OrganizationUUID = new("other")
	if IdentitiesAgree(a, b) {
		t.Fatal("contradicting organizations agree")
	}
	b.OrganizationUUID = nil
	b.AccountUUID = "b"
	if IdentitiesAgree(a, b) {
		t.Fatal("different accounts agree")
	}
}

func TestSwapCredentialIdentityAndOccupant(t *testing.T) {
	old := parseFixture(t, "credentials-old-blob.json")
	record := &config.AccountRecord{AccountUUID: "11111111-1111-4111-8111-111111111111", OrganizationUUID: config.UnknownOrg}
	if !SameIdentity(old, record) || OccupantOf(old) != "an unidentified credential" {
		t.Fatal("old credential was treated as a contradictory identity")
	}
	current := parseFixture(t, "credentials-new-blob.json")
	if !SameIdentity(current, record) || OccupantOf(current) != "user@example.com" {
		t.Fatal("named credential identity mismatch")
	}
	current.TokenAccount.EmailAddress = nil
	if OccupantOf(current) != record.AccountUUID {
		t.Fatal("account UUID fallback missing")
	}
	record.AccountUUID = "different"
	if SameIdentity(current, record) {
		t.Fatal("different account accepted")
	}
}

func TestSwapBusyNote(t *testing.T) {
	tests := map[string]struct {
		alive bool
		pids  []int32
		want  string
	}{
		"success: holder":            {alive: true, want: "another process is refreshing this store's credentials"},
		"success: unbroken lock":     {want: "this store's refresh lock is held; agentctl did not break it"},
		"success: stopped processes": {alive: true, pids: []int32{123, 456}, want: "a stopped claude process is present (pid 123, 456). agentctl will not break this lock while one is, because it cannot tell whether that process is the holder. Resume or end it, or run `agentctl claude doctor --remove-stale <path> --yes`"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, SwapBusyNote(tt.alive, tt.pids)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
