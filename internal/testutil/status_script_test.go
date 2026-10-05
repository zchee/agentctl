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

package testutil

import (
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestScriptCmds(t *testing.T) {
	if diff := gocmp.Diff([]string{"statusfixture"}, slices.Sorted(maps.Keys(ScriptCmds()))); diff != "" {
		t.Fatalf("in-process script commands mismatch (-want +got):\n%s", diff)
	}
}

func TestExpectedResetCell(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 5, 14, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		at   time.Time
		zone *time.Location
		want string
	}{
		"success: today uses an unpadded hour":          {at: now.Add(time.Hour), zone: time.UTC, want: "1h (3:00 PM)"},
		"success: local midnight uses the next weekday": {at: now.Add(time.Hour), zone: tokyo, want: "1h (Tue 12:00 AM)"},
		"success: later weekday zero pads the hour":     {at: now.AddDate(0, 0, 2), zone: time.UTC, want: "1h (Wed 02:00 PM)"},
		"success: noon is PM":                           {at: time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC), zone: time.UTC, want: "1h (12:00 PM)"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, expectedResetCell(now, tt.at, tt.zone, "1h")); diff != "" {
				t.Errorf("reset cell mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStatusFixtureServesUsageAndCountsRequests(t *testing.T) {
	dir := t.TempDir()
	script := `statusfixture owned
usage-calls 0 0 0
probeusage 200
usage-calls 1 0 0
usage-status 429
probeusage 429
usage-calls 2 0 0
`
	if err := os.WriteFile(filepath.Join(dir, "usage.txtar"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := ScriptCmds()
	commands["probeusage"] = func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: probeusage <expected-status>")
		}
		want, err := strconv.Atoi(args[0])
		ts.Check(err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.Getenv("AGENTCTL_CLAUDE_USAGE_URL")+UsagePath, nil)
		ts.Check(err)
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Do(request)
		ts.Check(err)
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		ts.Check(err)
		if response.StatusCode != want {
			ts.Fatalf("usage response = %d, want %d: %s", response.StatusCode, want, body)
		}
		if want == http.StatusTooManyRequests && response.Header.Get("Retry-After") != "30" {
			ts.Fatalf("rate limit response has no thirty-second retry window")
		}
	}
	testscript.Run(t, testscript.Params{Dir: dir, Setup: ScriptSetup, Cmds: commands})
}
