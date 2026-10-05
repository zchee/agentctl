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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/usage"
)

func TestStatusHiddenFailureOnlyChangesExitWhenShown(t *testing.T) {
	tests := map[string]struct {
		all    bool
		failed int
	}{
		"success: a hidden rate limit does not fail the pass": {},
		"error: showing the rate limited row fails the pass":  {all: true, failed: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			const hiddenAccount = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			forgotten := world.fixture.OwnedRecord(hiddenAccount, testutil.Org)
			forgotten["forgotten"] = true
			world.fixture.WriteRegistry([]any{world.fixture.OwnedRecord(testutil.Acct, testutil.Org), forgotten})
			world.fixture.WriteCredentials(testutil.Acct, testutil.Org, world.fixture.Blob("fresh-access", "fresh-refresh", testutil.FreshAt()))
			paths := config.NewPaths(world.fixture.ConfigDir())
			entry := usage.NewCacheEntry(time.Now().UnixMilli(), usageBody(t))
			entry.RateLimitedUntilMs = new(time.Now().Add(time.Hour).UnixMilli())
			if err := usage.StoreCache(t.Context(), usage.CachePath(paths, hiddenAccount, testutil.Org), &entry); err != nil {
				t.Fatal(err)
			}
			stdout, err := world.run(t, cli.ClaudeStatusOptions{All: tt.all, Accounts: ownedOnly})
			if tt.failed == 0 && err != nil {
				t.Fatalf("hidden failure leaked to exit: %v\n%s", err, stdout)
			}
			if tt.failed != 0 {
				assertPartial(t, err, tt.failed)
				if !strings.Contains(stdout, "rate-limited") {
					t.Fatalf("the failing row was not shown: %s", stdout)
				}
			}
		})
	}
}
