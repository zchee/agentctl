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
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/usage"
)

func TestStatusRateLimitCache(t *testing.T) {
	tests := map[string]struct{ prior bool }{
		"success: hint persists without prior usage": {},
		"success: hint preserves prior usage":        {prior: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureCodexDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + ".rate-test"
			auth := fmt.Appendf(nil, `{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":"rate-account"}}`, jwt)
			if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0o600); err != nil {
				t.Fatal(err)
			}
			record := config.CodexAccountRecord{ChatGPTUserID: "rate-user", ChatGPTAccountID: "rate-account", Kind: config.CodexKindHomeReadOnly(home)}
			plan := RowPlan{Source: codexprovider.Source{Kind: codexprovider.SourceHomeReadOnly, Home: home, Record: &record}}
			cachePath := usage.CachePathIn(paths.CodexCacheDir(), record.ChatGPTUserID, record.ChatGPTAccountID)
			var prior *usage.CacheEntry
			if test.prior {
				entry := usage.NewCacheEntry(time.Now().Add(-time.Hour).UnixMilli(), []byte(`{"rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000}}}`))
				prior = &entry
				if err := usage.StoreCache(t.Context(), cachePath, prior); err != nil {
					t.Fatal(err)
				}
			}
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("usage method = %s, want GET", r.Method)
				}
				if gets.Add(1) == 1 {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000}}}`))
			}))
			defer server.Close()
			shared := &passShared{paths: paths, client: codexprovider.NewUsageClient(server.URL, "rate-test", time.Second), options: passOptions{refresh: true, noCache: true}}
			before := time.Now().UnixMilli()
			first := runRow(t.Context(), plan, shared)
			after := time.Now().UnixMilli()
			if diff := gocmp.Diff(codexprovider.StateRateLimited, first.account.State.Kind); diff != "" {
				t.Fatalf("first state (-want +got):\n%s", diff)
			}
			cached := usage.LoadCache(t.Context(), cachePath)
			if cached == nil || cached.RateLimitedUntilMs == nil {
				t.Fatalf("429 hint not persisted: %+v", cached)
			}
			if until := *cached.RateLimitedUntilMs; until < before+60_000 || until > after+60_000 {
				t.Fatalf("retry deadline = %d, want [%d,%d]", until, before+60_000, after+60_000)
			}
			want := usage.NewCacheEntry(0, []byte("{}"))
			if prior != nil {
				want = *prior
			}
			want.RateLimitedUntilMs = cached.RateLimitedUntilMs
			if diff := gocmp.Diff(want, *cached); diff != "" {
				t.Fatalf("persisted usage (-want +got):\n%s", diff)
			}
			second := runRow(t.Context(), plan, shared)
			if second.account.State.Kind != codexprovider.StateRateLimited || gets.Load() != 1 {
				t.Fatalf("active hint: state=%s GET=%d, want rate limited and one GET", second.account.State.Kind, gets.Load())
			}
			cached.RateLimitedUntilMs = new(time.Now().Add(-time.Second).UnixMilli())
			if err := usage.StoreCache(t.Context(), cachePath, cached); err != nil {
				t.Fatal(err)
			}
			third := runRow(t.Context(), plan, shared)
			if third.account.State.Kind != codexprovider.StateOK || gets.Load() != 2 {
				t.Fatalf("expired hint: state=%s GET=%d, want OK and two GETs", third.account.State.Kind, gets.Load())
			}
			if cached := usage.LoadCache(t.Context(), cachePath); cached == nil || cached.RateLimitedUntilMs != nil {
				t.Fatalf("success did not replace expired hint: %+v", cached)
			}
		})
	}
}

func TestStoreRateLimitRefusesOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	before := usage.NewCacheEntry(1234, []byte("{}"))
	if err := usage.StoreCache(t.Context(), path, &before); err != nil {
		t.Fatal(err)
	}
	storeRateLimit(t.Context(), path, &before, math.MaxInt64, time.Second)
	if diff := gocmp.Diff(&before, usage.LoadCache(t.Context(), path)); diff != "" {
		t.Fatalf("overflow changed existing cache (-want +got):\n%s", diff)
	}
}

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
