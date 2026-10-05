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
	"time"

	"github.com/zchee/agentctl/internal/runtime/coordinator"
)

// DefaultMaxWorkers is how many rows a pass works on at once. Four keeps
// a dozen accounts from serializing behind one slow keychain child while
// still bounding the subprocess and socket fan-out.
const DefaultMaxWorkers = coordinator.DefaultMaxWorkers

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

// PassRunner runs one pass's jobs with bounded concurrency.
type PassRunner interface {
	// Run waits for all started jobs to finish. Cancellation prevents queued
	// jobs from starting and reaches active jobs through their context.
	Run(ctx context.Context, jobs []func(context.Context))
}

// CoordinatedRunner runs status jobs through the shared pass coordinator.
type CoordinatedRunner struct {
	// Workers caps the jobs in flight; zero or less means
	// [DefaultMaxWorkers].
	Workers int
}

var _ PassRunner = CoordinatedRunner{}

// Run implements [PassRunner] with the shared cancellation and worker bounds.
func (r CoordinatedRunner) Run(ctx context.Context, jobs []func(context.Context)) {
	workers := r.Workers
	if workers <= 0 {
		workers = DefaultMaxWorkers
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Duration(math.MaxInt64))
	}
	passJobs := make([]coordinator.Job[struct{}], 0, len(jobs))
	for _, job := range jobs {
		passJobs = append(passJobs, func(pass *coordinator.PassCtx) struct{} {
			job(pass.Context())
			return struct{}{}
		})
	}
	for range coordinator.RunPass(ctx, nil, passJobs, deadline, workers) {
	}
}
