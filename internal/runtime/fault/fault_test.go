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

package fault_test

import (
	"context"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/runtime/fault"
)

func TestNone(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name string
	}{
		"success: the empty set reports every name inactive": {name: "rename_fail"},
		"success: the empty string is inactive too":          {name: ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if fault.None().Is(tt.name) {
				t.Errorf("None().Is(%q) = true, want false", tt.name)
			}
		})
	}
}

func TestStall(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		deadline time.Duration
		cancelIn time.Duration // zero means never
		maxWait  time.Duration
	}{
		"success: the stall ends at the deadline": {
			deadline: 50 * time.Millisecond,
			maxWait:  2 * time.Second,
		},
		"success: cancellation ends the stall before the deadline": {
			deadline: 10 * time.Second,
			cancelIn: 50 * time.Millisecond,
			maxWait:  2 * time.Second,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelIn > 0 {
				timer := time.AfterFunc(tt.cancelIn, cancel)
				defer timer.Stop()
			}

			started := time.Now()
			fault.Stall(ctx, time.Now().Add(tt.deadline))
			if elapsed := time.Since(started); elapsed > tt.maxWait {
				t.Errorf("the stall held for %v, want under %v", elapsed, tt.maxWait)
			}
		})
	}
}
