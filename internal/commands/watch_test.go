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
	"os"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestWatchRunsFreshStatusPasses(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))
	paths := config.NewPaths(world.fixture.ConfigDir())
	factories := 0
	watch := Watch{NewStatus: func() *Status { factories++; copy := *world.status; return &copy }}
	for i, forced := range []bool{false, false, true} {
		rows, err := watch.collect(t.Context(), paths, forced)
		if err != nil {
			t.Fatal(err)
		}
		var owned *rowOutcome
		for j := range rows {
			if rows[j].id == testutil.Acct {
				owned = &rows[j]
			}
		}
		if owned == nil || owned.usage == nil || len(owned.Gauges()) == 0 {
			t.Fatalf("pass %d did not produce owned usage", i)
		}
		want := int64(1)
		if forced {
			want = 2
		}
		if diff := gocmp.Diff(want, world.calls.Load()); diff != "" {
			t.Fatalf("pass %d requests (-want +got): %s", i, diff)
		}
	}
	world.fixture.WriteRegistry(nil)
	rows, err := watch.collect(t.Context(), paths, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.record.Kind.Owned != nil {
			t.Fatal("removed registry entry survived the next pass")
		}
	}
	if factories != 4 {
		t.Fatalf("fresh status factories=%d, want 4", factories)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err = watch.collect(t.Context(), paths, false)
	if err == nil || rows != nil || factories != 4 {
		t.Fatalf("unreadable registry must preserve the display: rows=%v error=%v factories=%d", rows, err, factories)
	}
}

func TestWatchRefusesInvalidStartup(t *testing.T) {
	tests := map[string]struct {
		interval time.Duration
		want     string
	}{
		"error: polling floor":          {30 * time.Second, "below the 60s floor"},
		"error: missing status factory": {cli.WatchDefault, "factory is not configured"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			watch := Watch{}
			err := watch.Run(t.Context(), cli.Globals{ConfigDir: t.TempDir()}, cli.ClaudeWatchOptions{Interval: tt.interval})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want %q", err, tt.want)
			}
		})
	}
}

func TestWatchRowAdapter(t *testing.T) {
	tests := map[string]struct {
		state claude.AccountState
		badge string
	}{
		"success: healthy":          {claude.StateOfOK(), ""},
		"success: stale":            {claude.StateOfStale(), "stale"},
		"success: rate limited":     {claude.StateOfRateLimited(), "rate-limited"},
		"success: active session":   {claude.StateOfClaudeSessionDetected(".lock", 4000), "claude-detected"},
		"success: keychain locked":  {claude.StateOfKeychainLocked(""), "keychain-locked"},
		"success: keychain timeout": {claude.StateOfKeychainTimeout(), "keychain-locked"},
		"success: busy":             {claude.StateOfBusy(), "busy"},
		"success: needs login":      {claude.StateOfNeedsLogin(), "needs login"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			snapshot, err := claude.ParseUsage(usageBody(t), now, false)
			if err != nil {
				t.Fatal(err)
			}
			row := rowOutcome{account: "person@example.com", org: "Acme", plan: "max", state: tt.state, note: "test note", usage: snapshot, visibleByDefault: true}
			reset, _ := snapshot.NextReset()
			want := render.DetailLine(now, tt.badge, tt.state.Label(), row.note, reset)
			if diff := gocmp.Diff(want, row.DetailLine(now)); diff != "" {
				t.Fatal(diff)
			}
			if row.StateToken() != tt.state.Name() || !row.VisibleByDefault() || row.WatchTitle() != watchTitle || row.HiddenHint() != watchHiddenHint {
				t.Fatal("row metadata differs")
			}
			if diff := gocmp.Diff(render.UsageGauges(snapshot), row.Gauges()); diff != "" {
				t.Fatal(diff)
			}
			if row.BlockHeight() != 3+len(row.Gauges()) {
				t.Fatal("block height does not fit the gauges")
			}
			if diff := gocmp.Diff("▸ person@example.com · Acme · max ", row.AccountTitle(true)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
