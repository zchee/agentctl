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
	"time"
)

// DefaultMaxWorkers is how many rows a pass works on at once. Four keeps
// a dozen accounts from serializing behind one slow keychain child while
// still bounding the subprocess and socket fan-out.
const DefaultMaxWorkers = 4

// PassTimeoutMultiplier is how much longer than one request the whole
// pass may take.
//
// Three requests' worth: a refresh POST, a usage GET, and one retry of
// the GET after a 401. A worker that has not finished by then is holding
// the pass open past anything the user asked for.
const PassTimeoutMultiplier = 3

// PassBudget is the whole-pass deadline derived from one request's
// timeout. It saturates rather than wrapping on an absurd timeout: a
// wrapped budget would land in the past and abort the pass before it
// began.
func PassBudget(timeout time.Duration) time.Duration {
	if timeout > math.MaxInt64/PassTimeoutMultiplier {
		return math.MaxInt64
	}
	return timeout * PassTimeoutMultiplier
}

// PassRunner runs one pass's jobs. It is the seam between a command and
// the machinery that bounds its concurrency, so the pass logic does not
// change when a richer coordinator — cancellation plumbing, child
// ownership — replaces the bounded default.
type PassRunner interface {
	// Run runs every job and returns once all of them have. Each job owns
	// its own result slot, so the runner moves no data; a job observes
	// cancellation through the context it is handed.
	Run(ctx context.Context, jobs []func(context.Context))
}

// BoundedRunner is the default runner: at most Workers jobs at once, no
// ordering guarantee, every job started exactly once.
type BoundedRunner struct {
	// Workers caps the jobs in flight; zero or less means
	// [DefaultMaxWorkers].
	Workers int
}

var _ PassRunner = BoundedRunner{}

// Run implements [PassRunner] with a semaphore over goroutines. A
// cancelled context stops new jobs from starting; jobs already running
// see the same context and finish on their own terms, which is what lets
// the pass return whatever rows it completed.
func (r BoundedRunner) Run(ctx context.Context, jobs []func(context.Context)) {
	workers := r.Workers
	if workers <= 0 {
		workers = DefaultMaxWorkers
	}
	semaphore := make(chan struct{}, workers)
	var group sync.WaitGroup
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		semaphore <- struct{}{}
		group.Go(func() {
			defer func() { <-semaphore }()
			job(ctx)
		})
	}
	group.Wait()
}
