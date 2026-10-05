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

// Package fault makes otherwise unreachable failure branches reachable
// from tests.
//
// Several behaviours this module must get right cannot be driven from the
// public surface: a rename that fails, a lock another process holds past
// the deadline, a keychain helper that never answers. Each is a real
// failure mode with a real branch, and each is otherwise only reproducible
// by breaking the machine. So the branches are reachable through one
// environment variable holding a comma-separated list of fault names —
// in a tagged build only.
//
// A release build has no way to reach any of it: the active set is always
// empty, Is is always false and every pause returns at once, because the
// file that reads the environment only compiles under the testing tag.
// Production code receives its fault set from Active and never reads the
// environment variable itself.
package fault

import (
	"context"
	"time"
)

// Fault is the set of faults active in this process. The zero value is
// the empty set. Values are immutable after construction and cheap to
// copy, because they are carried by requests that cross goroutines.
type Fault struct {
	names map[string]struct{}
}

// None returns the empty fault set: nothing is injected. It is what
// production code gets in a release build.
func None() Fault {
	return Fault{}
}

// Is reports whether name is active.
func (f Fault) Is(name string) bool {
	_, ok := f.names[name]
	return ok
}

// Stall sleeps until deadline or cancellation, whichever comes first.
//
// Shared by the injections that hold something open — a held lock, a
// hanging helper — so both wait the same cooperative way rather than
// blocking a worker the pass is trying to wind down.
func Stall(ctx context.Context, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
