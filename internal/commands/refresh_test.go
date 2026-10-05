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

package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func refreshOAuth(t *testing.T, handler http.HandlerFunc) *claude.OAuthClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := claude.NewOAuthClient(server.URL+"/token", server.URL+"/profile", "agentctl-test")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func refreshRecord(t *testing.T, fixture *testutil.Fixture) (*config.Paths, *config.AccountRecord) {
	t.Helper()
	paths := config.NewPaths(fixture.ConfigDir())
	registry, err := config.LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	return paths, &registry.Accounts[0]
}

func TestStatusRefreshPersistsAndFetches(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("expired-access", "old-refresh", testutil.ExpiredAt()))
	var posts atomic.Int64
	world.status.Refresher = refreshOAuth(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		posts.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`))
	})
	stdout, err := world.run(t, cli.ClaudeStatusOptions{Refresh: true, Accounts: ownedOnly})
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stdout)
	}
	if diff := gocmp.Diff([]int64{1, 1}, []int64{posts.Load(), world.calls.Load()}); diff != "" {
		t.Fatalf("POST/usage (-want +got):\n%s", diff)
	}
	paths, _ := refreshRecord(t, world.fixture)
	stored := rereadCredential(paths.NamespaceDir(testutil.Acct, testutil.Org))
	if stored == nil || stored.AccessExpired(time.Now().UnixMilli(), claude.RefreshMarginMillis) {
		t.Fatal("refreshed credential was not persisted")
	}
	if stored.AuthorizationHeader() != "Bearer rotated-access" {
		t.Fatal("stored credential does not contain the returned token")
	}
	info, err := os.Stat(filepath.Join(paths.NamespaceDir(testutil.Acct, testutil.Org), secret.CredentialsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions: %v, %v", info, err)
	}
}

func TestRefreshFileBarriers(t *testing.T) {
	tests := map[string]struct {
		before bool
		mutate func(*testing.T, string)
		want   claude.AccountStateKind
		posts  int64
	}{
		"error: a session before the POST refuses the grant": {before: true, mutate: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, secret.RefreshLockName), 0o700); err != nil {
				t.Fatal(err)
			}
		}, want: claude.StateClaudeSessionDetected},
		"error: a session after the POST discards the result": {mutate: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, secret.RefreshLockName), 0o700); err != nil {
				t.Fatal(err)
			}
		}, want: claude.StateRefreshDiscarded, posts: 1},
		"error: replacement after the POST discards the result": {mutate: func(t *testing.T, dir string) {
			t.Helper()
			target := filepath.Join(dir, secret.CredentialsFile)
			if err := os.Rename(target, target+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: claude.StateRefreshDiscarded, posts: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			world.seedOwned(t, world.fixture.Blob("expired-access", "old-refresh", testutil.ExpiredAt()))
			paths, record := refreshRecord(t, world.fixture)
			dir := paths.NamespaceDir(testutil.Acct, testutil.Org)
			var posts atomic.Int64
			world.status.Refresher = refreshOAuth(t, func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				tt.mutate(t, dir)
				_, _ = w.Write([]byte(`{"access_token":"rotated-access","expires_in":3600}`))
			})
			if tt.before {
				tt.mutate(t, dir)
			}
			result := world.status.refreshExpired(t.Context(), paths, record, nil)
			if result.state == nil || result.state.Kind != tt.want || posts.Load() != tt.posts {
				t.Fatalf("state = %v; posts = %d; want kind %v, posts %d", result.state, posts.Load(), tt.want, tt.posts)
			}
			if result.credentials != nil {
				t.Fatal("refused refresh returned credentials")
			}
		})
	}
}

func TestConcurrentFileRefreshSpendsOneGrant(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("expired-access", "old-refresh", testutil.ExpiredAt()))
	paths, record := refreshRecord(t, world.fixture)
	var posts atomic.Int64
	world.status.Refresher = refreshOAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"rotated-access","expires_in":3600}`))
	})
	results := make([]refreshOutcome, 2)
	var group sync.WaitGroup
	for i := range results {
		group.Go(func() { results[i] = world.status.refreshExpired(t.Context(), paths, record, nil) })
	}
	group.Wait()
	for _, result := range results {
		if result.credentials == nil {
			t.Fatalf("refresh failed: %+v", result)
		}
	}
	if posts.Load() != 1 {
		t.Fatalf("POST count = %d; want 1", posts.Load())
	}
}

func TestRefreshClassifiesTokenFailuresWithoutOverwriting(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		want   claude.AccountStateKind
	}{
		"error: invalid grant requires login":   {status: 400, body: `{"error":"invalid_grant"}`, want: claude.StateNeedsLogin},
		"error: rate limit preserves grant":     {status: 429, body: `{}`, want: claude.StateRateLimited},
		"error: server failure preserves grant": {status: 500, body: `{}`, want: claude.StateError},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			world.seedOwned(t, world.fixture.Blob("expired-access", "old-refresh", testutil.ExpiredAt()))
			paths, record := refreshRecord(t, world.fixture)
			world.status.Refresher = refreshOAuth(t, serveBody(tt.status, []byte(tt.body)))
			result := world.status.refreshExpired(t.Context(), paths, record, nil)
			if result.state == nil || result.state.Kind != tt.want {
				t.Fatalf("state = %v; want kind %v", result.state, tt.want)
			}
			stored := rereadCredential(paths.NamespaceDir(testutil.Acct, testutil.Org))
			if stored == nil || stored.AuthorizationHeader() != "Bearer expired-access" {
				t.Fatal("failed refresh changed the stored credential")
			}
		})
	}
}

func TestStatusUsageUnauthorizedIsRetriedOnlyOnce(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusUnauthorized, []byte(`{}`)))
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))
	stdout, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly})
	assertPartial(t, err, 1)
	if world.calls.Load() != 2 || !strings.Contains(stdout, "needs login") {
		t.Fatalf("usage calls = %d; output: %s", world.calls.Load(), stdout)
	}
}

func TestRefreshPlanBudgetAndFailureDoNotLoseGrant(t *testing.T) {
	tests := map[string]struct {
		budget        time.Duration
		profileStatus int
		wantGets      int64
	}{
		"success: profile is persisted":                {budget: time.Minute, profileStatus: 200, wantGets: 1},
		"success: profile failure still saves refresh": {budget: time.Minute, profileStatus: 500, wantGets: 1},
		"success: short budget skips profile":          {budget: time.Second, profileStatus: 200},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			blob := strings.ReplaceAll(world.fixture.Blob("expired-access", "old-refresh", testutil.ExpiredAt()), `"subscriptionType":"max",`, "")
			world.seedOwned(t, blob)
			paths, record := refreshRecord(t, world.fixture)
			current := rereadCredential(paths.NamespaceDir(testutil.Acct, testutil.Org))
			current.SubscriptionType = nil
			current.RateLimitTier = nil
			data, err := current.BlobJSON()
			if err != nil {
				t.Fatal(err)
			}
			world.fixture.WriteCredentials(testutil.Acct, testutil.Org, string(data))
			var gets atomic.Int64
			oauth := refreshOAuth(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"access_token":"rotated-access","expires_in":3600}`))
					return
				}
				gets.Add(1)
				w.WriteHeader(tt.profileStatus)
				_, _ = w.Write([]byte(`{"account":{"uuid":"` + testutil.Acct + `","email":"` + testutil.Email + `"},"organization":{"uuid":"` + testutil.Org + `","organization_type":"claude_pro","rate_limit_tier":"default_claude_pro"}}`))
			})
			world.status.Refresher, world.status.Profiles = oauth, oauth
			ctx, cancel := context.WithTimeout(t.Context(), tt.budget)
			defer cancel()
			result := world.status.refreshExpired(ctx, paths, record, nil)
			if result.credentials == nil || gets.Load() != tt.wantGets {
				t.Fatalf("result = %+v; profile calls = %d; want %d", result, gets.Load(), tt.wantGets)
			}
			stored := rereadCredential(paths.NamespaceDir(testutil.Acct, testutil.Org))
			if stored == nil || stored.AuthorizationHeader() != "Bearer rotated-access" {
				t.Fatal("refresh not persisted")
			}
			if tt.wantGets > 0 && tt.profileStatus == 200 && (stored.SubscriptionType == nil || *stored.SubscriptionType != "pro") {
				t.Fatal("profile plan not persisted")
			}
		})
	}
}
