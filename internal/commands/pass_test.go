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
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedRunner(t *testing.T) {
	tests := map[string]struct {
		workers int
		jobs    int
		wantCap int
	}{
		"success: four at once is the default":       {workers: 0, jobs: 12, wantCap: DefaultMaxWorkers},
		"success: an explicit cap bounds the flight": {workers: 2, jobs: 8, wantCap: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var running, peak, ran atomic.Int64
			var mu sync.Mutex
			jobs := make([]func(context.Context), 0, tt.jobs)
			for range tt.jobs {
				jobs = append(jobs, func(context.Context) {
					now := running.Add(1)
					mu.Lock()
					if now > peak.Load() {
						peak.Store(now)
					}
					mu.Unlock()
					time.Sleep(5 * time.Millisecond)
					running.Add(-1)
					ran.Add(1)
				})
			}

			BoundedRunner{Workers: tt.workers}.Run(t.Context(), jobs)
			if got := ran.Load(); got != int64(tt.jobs) {
				t.Fatalf("ran = %d, want %d", got, tt.jobs)
			}
			if got := peak.Load(); got > int64(tt.wantCap) {
				t.Fatalf("peak concurrency = %d, want at most %d", got, tt.wantCap)
			}
		})
	}
}

func TestBoundedRunnerStartsNothingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var ran atomic.Int64
	BoundedRunner{}.Run(ctx, []func(context.Context){func(context.Context) { ran.Add(1) }})
	if got := ran.Load(); got != 0 {
		t.Fatalf("ran = %d, want 0 after cancellation", got)
	}
}

func TestPassBudget(t *testing.T) {
	tests := map[string]struct {
		timeout time.Duration
		want    time.Duration
	}{
		"success: three requests' worth":     {timeout: 10 * time.Second, want: 30 * time.Second},
		"success: an absurd value saturates": {timeout: math.MaxInt64 / 2, want: math.MaxInt64},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := PassBudget(tt.timeout); got != tt.want {
				t.Fatalf("budget = %d, want %d", got, tt.want)
			}
		})
	}
}
