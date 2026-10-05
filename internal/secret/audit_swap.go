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
	"slices"
	"strings"
)

// UndoKind classifies the write selected for reversal.
type UndoKind uint8

const (
	UndoNothing UndoKind = iota
	UndoUnreadable
	UndoNamespace
	UndoLive
)

// Undoable names a reversible write, or why no write can be selected.
// It contains audit metadata only, never a credential.
type Undoable struct {
	Kind             UndoKind
	Line             int
	SHA8             string
	FromDigest8      *string
	ToDigest8        string
	Outcome          KeychainWriteOutcome
	Direction        WriteDirection
	IncomingIdentity *IncomingIdentity
}

// SelectUndo prefers an outstanding live forward swap over namespace refreshes.
// The caller must supply the whole log: any unreadable line refuses selection,
// because skipping a truncated write could reverse the wrong credential.
func SelectUndo(tail AuditTail) Undoable {
	if len(tail.Unreadable) != 0 {
		return Undoable{Kind: UndoUnreadable, Line: tail.Unreadable[0].Line}
	}
	var newest Undoable
	undoneLater := false
	for _, v := range slices.Backward(tail.Entries) {
		candidate, ok := undoable(v)
		if !ok {
			continue
		}
		if newest.Kind == UndoNothing {
			newest = candidate
		}
		if candidate.Kind != UndoLive {
			continue
		}
		if candidate.Direction == DirectionUndo {
			undoneLater = undoneLater || candidate.Outcome == WriteApplied
			continue
		}
		if !undoneLater {
			return candidate
		}
		break
	}
	return newest
}

func undoable(entry AuditEntry) (Undoable, bool) {
	write, ok := entry.Event.(*WriteEvent)
	if !ok || write == nil || (write.Outcome != WriteApplied && write.Outcome != WriteUnknown) {
		return Undoable{}, false
	}
	result := Undoable{FromDigest8: write.FromDigest8, ToDigest8: write.ToDigest8}
	if write.Target == TargetLive {
		result.Kind = UndoLive
		result.Outcome = write.Outcome
		result.Direction = write.Direction
		if result.Direction == "" {
			result.Direction = DirectionForward
		}
		result.IncomingIdentity = write.IncomingIdentity
		return result, true
	}
	sha8, ok := strings.CutPrefix(string(write.Target), "namespace:")
	if !ok {
		return Undoable{}, false
	}
	result.Kind, result.SHA8 = UndoNamespace, sha8
	return result, true
}
