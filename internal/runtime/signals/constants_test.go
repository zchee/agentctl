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

package signals

import (
	"testing"
	"time"
)

func TestTimingConstants(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		got  time.Duration
		want time.Duration
	}{
		"success: child polling": {got: childPollInterval, want: 5 * time.Millisecond},
		"success: kill settling": {got: killSettle, want: 50 * time.Millisecond},
		"success: TERM grace":    {got: childTermBudget, want: 500 * time.Millisecond},
		"success: exit deferral": {got: exitDeferralLimit, want: 10 * time.Second},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("duration = %v, want %v", tt.got, tt.want)
			}
		})
	}
}
