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
		"success: empty":                                {AuditTail{}, Undoable{}},
		"success: namespace first write":                {AuditTail{Entries: []AuditEntry{namespace}}, wantNamespace},
		"success: live outranks later refresh":          {AuditTail{Entries: []AuditEntry{live, namespace}}, wantLive},
		"success: unknown undo leaves live outstanding": {AuditTail{Entries: []AuditEntry{live, unknownUndo, namespace}}, wantLive},
		"success: applied undo clears forward":          {AuditTail{Entries: []AuditEntry{live, appliedUndo, namespace}}, wantNamespace},
		"success: undo is reversible":                   {AuditTail{Entries: []AuditEntry{live, appliedUndo}}, Undoable{Kind: UndoLive, ToDigest8: "aaaaaaaa", Outcome: WriteApplied, Direction: DirectionUndo}},
		"success: newer forward rearms":                 {AuditTail{Entries: []AuditEntry{appliedUndo, live, namespace}}, wantLive},
		"error: damaged line outranks all":              {AuditTail{Entries: []AuditEntry{live, namespace}, Unreadable: []UnreadableAuditLine{{Line: 7}, {Line: 8}}}, Undoable{Kind: UndoUnreadable, Line: 7}},
		"success: failed and discarded are ignored":     {AuditTail{Entries: []AuditEntry{namespace, {Event: &WriteEvent{Target: TargetLive, Outcome: WriteFailed}}, {Event: &WriteEvent{Target: TargetLive, Outcome: WriteDiscarded}}, {Event: &ConfigWriteRecord{}}, {Event: &UnrecognizedEvent{}}}}, wantNamespace},
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
