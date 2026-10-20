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

package secret

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestSelectUndo(t *testing.T) {
	live := AuditEntry{Event: &WriteEvent{Target: TargetLive, FromDigest8: new("aaaaaaaa"), ToDigest8: "bbbbbbbb", Outcome: WriteApplied, Direction: DirectionForward, IncomingIdentity: &IncomingIdentity{AccountUUID: "account"}}}
	namespace := AuditEntry{Event: &WriteEvent{Target: NamespaceTarget("12345678"), ToDigest8: "cccccccc", Outcome: WriteUnknown, Direction: DirectionForward}}
	appliedUndo := AuditEntry{Event: &WriteEvent{Target: TargetLive, ToDigest8: "aaaaaaaa", Outcome: WriteApplied, Direction: DirectionUndo}}
	unknownUndo := AuditEntry{Event: &WriteEvent{Target: TargetLive, ToDigest8: "dddddddd", Outcome: WriteUnknown, Direction: DirectionUndo}}
	wantLive := Undoable{Kind: UndoLive, FromDigest8: new("aaaaaaaa"), ToDigest8: "bbbbbbbb", Outcome: WriteApplied, Direction: DirectionForward, IncomingIdentity: &IncomingIdentity{AccountUUID: "account"}}
	wantNamespace := Undoable{Kind: UndoNamespace, SHA8: "12345678", ToDigest8: "cccccccc"}
	tests := map[string]struct {
		tail AuditTail
		want Undoable
	}{
		"success: empty":                                       {AuditTail{}, Undoable{}},
		"success: namespace first write":                       {AuditTail{Entries: []AuditEntry{namespace}}, wantNamespace},
		"success: live outranks later refresh":                 {AuditTail{Entries: []AuditEntry{live, namespace}}, wantLive},
		"success: unknown undo leaves live outstanding":        {AuditTail{Entries: []AuditEntry{live, unknownUndo, namespace}}, wantLive},
		"success: applied undo clears forward":                 {AuditTail{Entries: []AuditEntry{live, appliedUndo, namespace}}, wantNamespace},
		"success: undo is reversible":                          {AuditTail{Entries: []AuditEntry{live, appliedUndo}}, Undoable{Kind: UndoLive, ToDigest8: "aaaaaaaa", Outcome: WriteApplied, Direction: DirectionUndo}},
		"success: newer forward rearms":                        {AuditTail{Entries: []AuditEntry{appliedUndo, live, namespace}}, wantLive},
		"error: damaged line outranks all":                     {AuditTail{Entries: []AuditEntry{live, namespace}, Unreadable: []UnreadableAuditLine{{Line: 7}, {Line: 8}}}, Undoable{Kind: UndoUnreadable, Line: 7}},
		"success: the selected entry carries its own audit id": {AuditTail{Entries: []AuditEntry{{TS: "2026-10-13T07:00:00Z", PID: 4242, Event: &WriteEvent{Target: TargetLive, ToDigest8: "eeeeeeee", Outcome: WriteApplied, Direction: DirectionForward}}, {TS: "2026-10-13T07:00:01Z", PID: 4243, Event: &ConfigWriteRecord{}}}}, Undoable{Kind: UndoLive, ToDigest8: "eeeeeeee", Outcome: WriteApplied, Direction: DirectionForward, ID: AuditID{TS: "2026-10-13T07:00:00Z", PID: 4242}}},
		"success: failed and discarded are ignored":            {AuditTail{Entries: []AuditEntry{namespace, {Event: &WriteEvent{Target: TargetLive, Outcome: WriteFailed}}, {Event: &WriteEvent{Target: TargetLive, Outcome: WriteDiscarded}}, {Event: &ConfigWriteRecord{}}, {Event: &UnrecognizedEvent{}}}}, wantNamespace},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, SelectUndo(tt.tail)); diff != "" {
				t.Fatalf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestSwapAuditLinesRoundTrip(t *testing.T) {
	tests := map[string]struct {
		line      string
		direction WriteDirection
	}{
		"success: live forward":          {`{"ts":"2025-09-05T12:00:00Z","monotonic_ms":7,"agctl_pid":42,"event":"write","target":"live","from_digest8":"deadbeef","to_digest8":"0123abcd","outcome":"applied","direction":"forward","incoming_identity":{"account_uuid":"acct","organization_uuid":"org"}}`, DirectionForward},
		"success: live undo":             {`{"ts":"2025-09-05T12:00:00Z","monotonic_ms":7,"agctl_pid":42,"event":"write","target":"live","from_digest8":"0123abcd","to_digest8":"deadbeef","outcome":"unknown","direction":"undo","incoming_identity":{"account_uuid":"acct","organization_uuid":null}}`, DirectionUndo},
		"success: namespace first write": {`{"ts":"2025-09-05T12:00:00Z","monotonic_ms":7,"agctl_pid":42,"event":"write","target":"namespace:abcd1234","from_digest8":null,"to_digest8":"0123abcd","outcome":"applied","direction":"forward"}`, DirectionForward},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			entry, err := decodeAuditLine([]byte(tt.line))
			if err != nil {
				t.Fatal(err)
			}
			if event, ok := entry.Event.(*WriteEvent); !ok || event.Direction != tt.direction {
				t.Fatalf("wrong event: %+v", entry.Event)
			}
			line, err := auditEntryLine(entry)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.line+"\n", line); diff != "" {
				t.Fatal(diff)
			}
		})
	}
	old := `{"ts":"2025-09-05T12:00:00Z","monotonic_ms":7,"agctl_pid":42,"event":"write","target":"live","from_digest8":null,"to_digest8":"0123abcd","outcome":"applied"}`
	entry, err := decodeAuditLine([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if got := SelectUndo(AuditTail{Entries: []AuditEntry{entry}}); got.Kind != UndoLive || got.Direction != DirectionForward || got.IncomingIdentity != nil {
		t.Fatalf("legacy entry: %+v", got)
	}
}

func TestSelectUndoNeverSelectsASeed(t *testing.T) {
	// A seed creates an item that had no earlier credential, so nothing
	// could be put back: whatever its outcome, it is never an undo
	// candidate, and an earlier swap stays the one selected.
	live := AuditEntry{Event: &WriteEvent{Target: TargetLive, FromDigest8: new("aaaaaaaa"), ToDigest8: "bbbbbbbb", Outcome: WriteApplied, Direction: DirectionForward, IncomingIdentity: &IncomingIdentity{AccountUUID: "account"}}}
	namespace := AuditEntry{Event: &WriteEvent{Target: NamespaceTarget("12345678"), ToDigest8: "cccccccc", Outcome: WriteApplied, Direction: DirectionForward}}
	seeds := map[string]AuditEntry{
		"applied":   {Event: &SeedEvent{Target: NamespaceTarget("12345678"), ToDigest8: "dddddddd", Outcome: WriteApplied, Seeded: []string{"mcpOAuth"}, SiblingsReadback: SiblingsEqual, Transport: TransportArgv, OwnedFile: OwnedFileRemoved}},
		"unknown":   {Event: &SeedEvent{Target: NamespaceTarget("12345678"), ToDigest8: "dddddddd", Outcome: WriteUnknown, SiblingsReadback: SiblingsUnverified, Transport: TransportArgv, OwnedFile: OwnedFileKept, Signal: "SIGTERM"}},
		"own_state": {Event: &SeedEvent{Target: NamespaceTarget("12345678"), ToDigest8: "dddddddd", Outcome: WriteOwnState}},
	}
	wantLive := Undoable{Kind: UndoLive, FromDigest8: new("aaaaaaaa"), ToDigest8: "bbbbbbbb", Outcome: WriteApplied, Direction: DirectionForward, IncomingIdentity: &IncomingIdentity{AccountUUID: "account"}}
	wantNamespace := Undoable{Kind: UndoNamespace, SHA8: "12345678", ToDigest8: "cccccccc"}
	for outcome, seed := range seeds {
		tests := map[string]struct {
			tail AuditTail
			want Undoable
		}{
			"success: a tail ending in a seed alone selects nothing":        {AuditTail{Entries: []AuditEntry{seed}}, Undoable{}},
			"success: a seed after a namespace swap leaves the swap":        {AuditTail{Entries: []AuditEntry{namespace, seed}}, wantNamespace},
			"success: a seed after a live forward swap leaves the swap":     {AuditTail{Entries: []AuditEntry{live, seed}}, wantLive},
			"success: a seed between two swaps leaves the newest candidate": {AuditTail{Entries: []AuditEntry{live, seed, namespace, seed}}, wantLive},
		}
		for name, tt := range tests {
			t.Run(outcome+"/"+name, func(t *testing.T) {
				if diff := gocmp.Diff(tt.want, SelectUndo(tt.tail)); diff != "" {
					t.Fatalf("(-want +got):\n%s", diff)
				}
			})
		}
	}
}

func TestSelectUndoReadsASeedLineFromTheLog(t *testing.T) {
	// The same through the log itself: the seed lines decode, so the tail
	// has nothing unreadable, and the namespace swap before them is still
	// the one selected, by its own id.
	tests := map[string]struct {
		before []AuditEvent
		want   UndoKind
	}{
		"success: with an earlier namespace swap": {
			before: []AuditEvent{&WriteEvent{Target: NamespaceTarget("12345678"), ToDigest8: "cccccccc", Outcome: WriteApplied, Direction: DirectionForward}},
			want:   UndoNamespace,
		},
		"success: without an earlier swap": {
			want: UndoNothing,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths := newAuditStore(t)
			var wantID AuditID
			for _, event := range tt.before {
				id, err := AuditAppend(t.Context(), paths, NewAuditEntry(event))
				if err != nil {
					t.Fatalf("append the swap: %v", err)
				}
				wantID = id
			}
			for _, seed := range []*SeedEvent{
				{Target: NamespaceTarget("12345678"), ToDigest8: "dddddddd", Outcome: WriteOwnState},
				{Target: NamespaceTarget("12345678"), ToDigest8: "dddddddd", Outcome: WriteDiscarded},
				{Target: NamespaceTarget("12345678"), ToDigest8: "dddddddd", Outcome: WriteApplied, Seeded: []string{"mcpOAuth"}, SiblingsReadback: SiblingsEqual, Transport: TransportArgv, OwnedFile: OwnedFileRemoved},
			} {
				if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(seed)); err != nil {
					t.Fatalf("append the seed: %v", err)
				}
			}
			tail, err := TailAuditLog(paths, 1<<30)
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			if len(tail.Unreadable) != 0 {
				t.Fatalf("a seed line must never read as unreadable: %+v", tail.Unreadable)
			}
			got := SelectUndo(tail)
			if got.Kind != tt.want {
				t.Fatalf("SelectUndo kind = %d, want %d: %+v", got.Kind, tt.want, got)
			}
			if tt.want != UndoNothing && got.ID != wantID {
				t.Errorf("selected %s, want the swap %s", got.ID, wantID)
			}
		})
	}
}
