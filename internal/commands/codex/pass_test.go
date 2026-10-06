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
	"math"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
)

func TestStatusBudgets(t *testing.T) {
	tests := map[string]struct {
		timeout time.Duration
		renew   bool
		want    time.Duration
	}{
		"success: default read":       {timeout: 10 * time.Second, want: 40 * time.Second},
		"success: default renewal":    {timeout: 10 * time.Second, renew: true, want: 101 * time.Second},
		"success: small request":      {timeout: time.Second, want: 6 * time.Second},
		"success: saturating timeout": {timeout: time.Duration(math.MaxInt64), renew: true, want: time.Duration(math.MaxInt64)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, PassBudget(test.timeout, test.renew)); diff != "" {
				t.Fatalf("budget (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStatusSelectors(t *testing.T) {
	row := config.CodexAccountRecord{ChatGPTUserID: "user-a", ChatGPTAccountID: "account-a", Email: new("owner@example.invalid")}
	plans := []RowPlan{{Index: 0, Source: codexprovider.Source{Kind: codexprovider.SourceLive}}, {Index: 1, Source: codexprovider.Source{Kind: codexprovider.SourceOwned, Record: &row}}}
	tests := map[string]struct {
		selectors []string
		indices   []int
		wantError string
	}{
		"success: all":   {indices: []int{0, 1}},
		"success: live":  {selectors: []string{"live"}, indices: []int{0}},
		"success: user":  {selectors: []string{"user-a"}, indices: []int{1}},
		"success: pair":  {selectors: []string{"user-a/account-a"}, indices: []int{1}},
		"success: email": {selectors: []string{"owner@example.invalid"}, indices: []int{1}},
		"success: duplicate selector preserves discovery order": {selectors: []string{"user-a", "live", "user-a"}, indices: []int{0, 1}},
		"error: unmatched": {selectors: []string{"absent"}, wantError: "known accounts: live, user-a/account-a"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := selectRows(plans, test.selectors)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("selector error=%v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			indices := make([]int, len(got))
			for i, row := range got {
				indices[i] = row.Index
			}
			if diff := gocmp.Diff(test.indices, indices); diff != "" {
				t.Fatalf("selection (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStatusHiddenSiblingProjection(t *testing.T) {
	home := t.TempDir()
	row := config.CodexAccountRecord{ChatGPTUserID: "user-old", ChatGPTAccountID: "account-old", Kind: config.CodexKindHomeReadOnly(home)}
	tests := map[string]struct {
		sameIdentity, sameDigest bool
		wantVisible              bool
		wantKind                 codexprovider.StateKind
	}{
		"success: duplicate live credential folded":          {sameIdentity: true, sameDigest: true, wantKind: codexprovider.StateOK},
		"success: changed live identity hides stale sibling": {sameDigest: true, wantKind: codexprovider.StateStaleSibling},
		"success: independent home credential shown":         {sameIdentity: true, wantVisible: true, wantKind: codexprovider.StateOK},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			user, account := "user-new", "account-new"
			if test.sameIdentity {
				user, account = row.ChatGPTUserID, row.ChatGPTAccountID
			}
			digest := "other"
			if test.sameDigest {
				digest = "live"
			}
			passes := []rowPass{
				{account: codexprovider.Account{UserID: "user-new", AccountID: "account-new", Kind: codexprovider.RowLive, Visible: true}, accessDigest: "live", plan: RowPlan{Source: codexprovider.Source{Kind: codexprovider.SourceLive, Home: home}}},
				{account: codexprovider.Account{UserID: user, AccountID: account, Kind: codexprovider.RowHomeReadOnly, State: codexprovider.State{Kind: codexprovider.StateOK}, Visible: true}, accessDigest: digest, plan: RowPlan{Source: codexprovider.Source{Kind: codexprovider.SourceHomeReadOnly, Home: home, Record: &row}}},
			}
			got := finishRows(passes)[1]
			if diff := gocmp.Diff(test.wantVisible, got.VisibleByDefault()); diff != "" {
				t.Fatalf("visibility (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(test.wantKind, got.State.Kind); diff != "" {
				t.Fatalf("state (-want +got):\n%s", diff)
			}
		})
	}
}
