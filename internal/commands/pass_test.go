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

func TestCoordinatedRunner(t *testing.T) {
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
			release := make(chan struct{})
			started := make(chan struct{}, tt.jobs)
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
					started <- struct{}{}
					<-release
					running.Add(-1)
					ran.Add(1)
				})
			}

			finished := make(chan struct{})
			go func() {
				CoordinatedRunner{Workers: tt.workers}.Run(t.Context(), jobs)
				close(finished)
			}()
			for range tt.wantCap {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					close(release)
					t.Fatal("workers did not reach the expected concurrency")
				}
			}
			close(release)
			<-finished
			if got := ran.Load(); got != int64(tt.jobs) {
				t.Fatalf("ran = %d, want %d", got, tt.jobs)
			}
			if got := peak.Load(); got != int64(tt.wantCap) {
				t.Fatalf("peak concurrency = %d, want %d", got, tt.wantCap)
			}
		})
	}
}

func TestCoordinatedRunnerStartsNothingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var ran atomic.Int64
	CoordinatedRunner{}.Run(ctx, []func(context.Context){func(context.Context) { ran.Add(1) }})
	if got := ran.Load(); got != 0 {
		t.Fatalf("ran = %d, want 0 after cancellation", got)
	}
}

func TestCoordinatedRunnerCancelsActiveJobsBeforeStartingQueuedJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{}, 12)
	var ran, stopped atomic.Int64
	jobs := make([]func(context.Context), 12)
	for i := range jobs {
		jobs[i] = func(ctx context.Context) {
			ran.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			stopped.Add(1)
		}
	}
	finished := make(chan struct{})
	go func() {
		CoordinatedRunner{}.Run(ctx, jobs)
		close(finished)
	}()
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the coordinator did not start four workers")
		}
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the coordinator did not drain cancelled jobs")
	}
	if ran.Load() != 4 || stopped.Load() != 4 {
		t.Fatalf("ran = %d, stopped = %d; want four active jobs and no queued jobs", ran.Load(), stopped.Load())
	}
}

func TestCoordinatedRunnerPropagatesDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var stopped atomic.Bool
	CoordinatedRunner{}.Run(ctx, []func(context.Context){func(ctx context.Context) {
		<-ctx.Done()
		stopped.Store(ctx.Err() == context.DeadlineExceeded)
	}})
	if !stopped.Load() {
		t.Fatal("the active worker did not receive the deadline")
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
