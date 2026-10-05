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

func TestSwapAuditLegacyWireCompatibility(t *testing.T) {
	tests := map[string]struct {
		line      string
		from, to  string
		direction WriteDirection
	}{
		"success: before direction was recorded":     {line: `{"ts":"2026-09-10T00:00:00Z","monotonic_ms":0,"agctl_pid":1,"event":"write","target":"live","from_digest8":"deadbeef","to_digest8":"cafebabe","outcome":"applied"}`, from: "deadbeef", to: "cafebabe", direction: DirectionForward},
		"success: undo before identity was recorded": {line: `{"ts":"2026-09-11T00:00:00Z","monotonic_ms":0,"agctl_pid":1,"event":"write","target":"live","from_digest8":"cafebabe","to_digest8":"deadbeef","outcome":"applied","direction":"undo"}`, from: "cafebabe", to: "deadbeef", direction: DirectionUndo},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			entry, err := decodeAuditLine([]byte(tt.line))
			if err != nil {
				t.Fatal(err)
			}
			want := Undoable{Kind: UndoLive, FromDigest8: &tt.from, ToDigest8: tt.to, Outcome: WriteApplied, Direction: tt.direction}
			if diff := gocmp.Diff(want, SelectUndo(AuditTail{Entries: []AuditEntry{entry}})); diff != "" {
				t.Fatal(diff)
			}
			line, err := auditEntryLine(entry)
			if err != nil {
				t.Fatal(err)
			}
			again, err := decodeAuditLine([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(entry, again); diff != "" {
				t.Fatalf("legacy round trip changed its meaning: %s", diff)
			}
		})
	}
}
