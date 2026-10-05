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

package secret

import (
	"context"
	rand "math/rand/v2"
	"time"
)

// Clock is where the lock protocol's time comes from.
//
// Injectable because the break rule's honesty rests on a 12-second wait
// and a comparison between two clocks, and a test that could not move
// either would have to take 12 s per case or assert something weaker than
// the rule. The jitter draw lives here too, so a test can pin every wait
// the contention schedules make.
type Clock interface {
	// Wall returns the wall clock, which can step in either direction.
	// The reading carries no monotonic component, so arithmetic on it
	// reflects wall time alone.
	Wall() time.Time
	// Monotonic returns the monotonic clock as the time since an
	// arbitrary fixed origin. It cannot step.
	Monotonic() time.Duration
	// Sleep waits howLong or until ctx is done, and returns ctx.Err()
	// when cancellation ended the wait early.
	Sleep(ctx context.Context, howLong time.Duration) error
	// Jitter returns a uniform draw in [0, span), and zero for a span
	// that is not positive.
	Jitter(span time.Duration) time.Duration
}

// systemClock is the real clocks.
type systemClock struct {
	origin time.Time
}

// SystemClock returns the real clocks and sleeper.
func SystemClock() Clock {
	return &systemClock{origin: time.Now()}
}

// Wall returns the wall-clock reading with its monotonic component
// stripped, so two readings subtract as wall time.
func (*systemClock) Wall() time.Time {
	return time.Now().Round(0)
}

// Monotonic returns the time since this clock was made, measured on the
// runtime's monotonic reading.
func (c *systemClock) Monotonic() time.Duration {
	return time.Since(c.origin)
}

// Sleep waits on a timer or the context, whichever ends first, so a
// cancellation during the 12-second sampling window ends it at once
// instead of twelve seconds later.
func (*systemClock) Sleep(ctx context.Context, howLong time.Duration) error {
	if howLong <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(howLong)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Jitter draws uniformly from [0, span).
func (*systemClock) Jitter(span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(span)))
}
