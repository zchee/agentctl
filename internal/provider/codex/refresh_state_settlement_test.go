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
)

func TestRefreshStateDefiniteAndUnknownSettlement(t *testing.T) {
	tests := map[string]struct {
		action string
		resent bool
	}{
		"success: definite never sent clears inflight":    {action: "clear"},
		"success: applied floor settles":                  {action: "applied"},
		"success: permanent records dead grant":           {action: "dead"},
		"success: never sent resend restores eligibility": {action: "restore"},
		"success: rejected resend remains spent":          {action: "restore", resent: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, guard, _ := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			credentials := lockedRead(t, namespace)
			grant, _ := credentials.RefreshDigest8()
			if _, err := store.writeInflight(t.Context(), credentials); err != nil {
				t.Fatal(err)
			}
			if err := store.markUnknown(t.Context(), guard, RefreshUnknownServerError, time.Now(), nil); err != nil {
				t.Fatal(err)
			}
			before := store.Load(t.Context()).State
			earliest := &EarliestRefresh{GrantDigest8: "1234abcd", At: time.Unix(1790417597, 0).UTC()}
			var err error
			switch test.action {
			case "clear":
				err = store.clearInflight(t.Context(), guard)
			case "applied":
				err = store.settleInflight(t.Context(), guard, earliest, nil)
			case "dead":
				err = store.settleInflight(t.Context(), guard, nil, new(grant))
			case "restore":
				err = store.restoreUnknown(t.Context(), guard, test.resent)
			}
			if err != nil {
				t.Fatal(err)
			}
			state := store.Load(t.Context()).State
			if diff := gocmp.Diff(before.LastSentAt, state.LastSentAt); diff != "" {
				t.Fatal(diff)
			}
			if test.action == "restore" {
				if state.Resent != test.resent || state.Class == nil || state.Inflight == nil {
					t.Fatal("original unknown state lost")
				}
				if diff := gocmp.Diff(before.AmbiguousSince, state.AmbiguousSince); diff != "" {
					t.Fatal(diff)
				}
				return
			}
			if state.Inflight != nil || state.Class != nil || state.AmbiguousSince != nil || state.RetryAfter != nil || state.Resent {
				t.Fatal("definite outcome retained inflight")
			}
			if test.action == "applied" {
				if diff := gocmp.Diff(earliest, state.EarliestRefresh); diff != "" {
					t.Fatal(diff)
				}
			}
			if test.action == "dead" && (state.DeadDigest8 == nil || *state.DeadDigest8 != grant) {
				t.Fatal("dead grant not recorded")
			}
		})
	}
}
