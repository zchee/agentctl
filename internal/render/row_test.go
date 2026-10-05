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

package render

import (
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/usage"
)

func TestUsageGauges(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		snapshot *usage.UsageSnapshot
		want     []Gauge
	}{
		"success: one gauge per window the response described": {
			snapshot: &usage.UsageSnapshot{Windows: healthyWindows(t), Credits: creditsUnavailable()},
			want:     []Gauge{{"5h", 21}, {"weekly", 35}, {"Fable", 56}},
		},
		"success: credits earn a gauge only when on with a utilisation figure": {
			snapshot: &usage.UsageSnapshot{
				Windows: healthyWindows(t),
				Credits: creditsOn(money(1234, "USD", 2), money(5000, "USD", 2), 25),
			},
			want: []Gauge{{"5h", 21}, {"weekly", 35}, {"Fable", 56}, {"credits", 25}},
		},
		"success: credits that are off earn no gauge": {
			// "off" is not a percentage.
			snapshot: &usage.UsageSnapshot{Windows: healthyWindows(t), Credits: creditsOff()},
			want:     []Gauge{{"5h", 21}, {"weekly", 35}, {"Fable", 56}},
		},
		"success: credits on without a figure earn no gauge": {
			snapshot: &usage.UsageSnapshot{
				Windows: healthyWindows(t),
				Credits: creditsOn(money(1234, "USD", 2), nil, -1),
			},
			want: []Gauge{{"5h", 21}, {"weekly", 35}, {"Fable", 56}},
		},
		"success: a window without a percentage earns no gauge rather than one at zero": {
			// A bar at zero is a claim that nothing has been used, which
			// is a different statement from "the server did not say".
			snapshot: &usage.UsageSnapshot{
				Windows: []usage.LimitWindow{{Kind: sessionWindowKind()}},
				Credits: creditsUnavailable(),
			},
			want: nil,
		},
		"success: a row without numbers earns no gauges": {
			snapshot: nil,
			want:     nil,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.want, UsageGauges(tt.snapshot)); diff != "" {
				t.Errorf("UsageGauges mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDetailLine(t *testing.T) {
	t.Parallel()

	now := ts(t, reportNow)

	tests := map[string]struct {
		badge     string
		state     string
		note      string
		nextReset time.Time
		want      string
	}{
		"success: a healthy row is the bare state": {
			state: "ok",
			want:  "ok",
		},
		"success: every piece joins with middle dots": {
			badge: "rate-limited", state: "rate-limited (retry in 30s)",
			note: "showing cached values", nextReset: ts(t, "2026-09-08T02:13:40Z"),
			want: "[rate-limited] · rate-limited (retry in 30s) · (showing cached values) · next reset in 2h13m",
		},
		"success: a reset alone follows the state": {
			state: "ok", nextReset: ts(t, "2026-09-10T20:00:00Z"),
			want: "ok · next reset in 2d20h",
		},
		"success: a reset already passed counts down to now": {
			state: "ok", nextReset: ts(t, "2026-09-07T20:00:00Z"),
			want: "ok · next reset in now",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := DetailLine(now, tt.badge, tt.state, tt.note, tt.nextReset)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("DetailLine mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAccountTitle(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		selected bool
		account  string
		middle   string
		plan     string
		want     string
	}{
		"success: the selected row carries the marker": {
			selected: true, account: "alice@example.com", middle: "Acme", plan: "max",
			want: "▸ alice@example.com · Acme · max ",
		},
		"success: an unselected row keeps the marker's width": {
			account: "alice@example.com", middle: "Acme", plan: "max",
			want: "  alice@example.com · Acme · max ",
		},
		"success: empty identity cells render the em dash": {
			account: "alice@example.com",
			want:    "  alice@example.com · — · — ",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := AccountTitle(tt.selected, tt.account, tt.middle, tt.plan)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("AccountTitle mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBlockHeight(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		gauges int
		want   int
	}{
		"success: no gauges is the chrome alone":  {gauges: 0, want: 3},
		"success: each gauge adds one line":       {gauges: 3, want: 6},
		"success: the credits gauge counts too":   {gauges: 4, want: 7},
		"success: two gauges fit the codex shape": {gauges: 2, want: 5},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := BlockHeight(tt.gauges); got != tt.want {
				t.Errorf("BlockHeight(%d) = %d, want %d", tt.gauges, got, tt.want)
			}
		})
	}
}
