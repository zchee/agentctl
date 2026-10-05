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

package coordinator

import (
	"testing"
	"time"
)

// The pass budgets are behavioural contracts: a keychain prompt is held
// open for at most these bounds, and a cancelled watch must come down
// inside them. The values are pinned here so a drive-by edit shows up as a
// failing test, not as a silently different latency.
func TestPassBudgets(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		got  time.Duration
		want time.Duration
	}{
		"success: workers get half a second to drain after their children die": {
			got:  WorkerJoinBudget,
			want: 500 * time.Millisecond,
		},
		"success: a wait re-checks its child every ten milliseconds": {
			got:  childPollInterval,
			want: 10 * time.Millisecond,
		},
		"success: the watchdog re-checks the deadline every twenty-five milliseconds": {
			got:  watchdogPollInterval,
			want: 25 * time.Millisecond,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Errorf("budget = %v, want %v", tt.got, tt.want)
			}
		})
	}
}

// TestWorkerBound pins the fan-out width the same way.
func TestWorkerBound(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		got  int
		want int
	}{
		"success: a pass runs at most four workers": {
			got:  DefaultMaxWorkers,
			want: 4,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Errorf("bound = %d, want %d", tt.got, tt.want)
			}
		})
	}
}
