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

package codex

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/runtime/lockorder"
	"github.com/zchee/agentctl/internal/secret"
)

var errInvalidProof = errors.New("the Codex namespace proof is invalid or its lock has been released")

type lockState struct {
	mu            sync.Mutex
	guard         *secret.LockGuard
	ctx           context.Context
	user, account string
	released      atomic.Bool
}

// Lock proves that this process holds one owned namespace's lock.
// Copies share their lifetime; releasing one invalidates every bound carrier.
type Lock struct{ state *lockState }

// AcquireCodex takes an owned registry namespace's lock within budget.
// A zero proof is refused before creating any file.
func AcquireCodex(ctx context.Context, paths *config.Paths, owned *OwnedRecord, budget time.Duration) (*Lock, error) {
	if owned == nil || owned.user == "" || owned.account == "" {
		return nil, errInvalidProof
	}
	return acquireCodex(ctx, paths, owned.user, owned.account, budget)
}

// AcquireCodexForInstall takes the lock for a verified, unconsumed login.
func AcquireCodexForInstall(ctx context.Context, paths *config.Paths, login *VerifiedLogin, budget time.Duration) (*Lock, error) {
	if login == nil || login.state == nil || login.state.consumed.Load() {
		return nil, errInvalidProof
	}
	return acquireCodex(ctx, paths, login.state.user, login.state.account, budget)
}

func acquireCodex(ctx context.Context, paths *config.Paths, user, account string, budget time.Duration) (*Lock, error) {
	path, err := paths.CodexLockPath(user, account)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(max(budget, 0))
	guard, err := secret.Acquire(ctx, paths.CodexLocksDir(), filepath.Base(path), deadline)
	if err != nil {
		return nil, err
	}
	return &Lock{state: &lockState{guard: guard, ctx: lockorder.WithOwned(ctx, lockorder.CodexNamespace), user: user, account: account}}, nil
}

// Context carries the ownership witness for calls made while the lock is held.
// After Release, callers must return to their parent context.
func (l *Lock) Context() context.Context {
	if l == nil || l.state == nil {
		return nil
	}
	return l.state.ctx
}

// Path returns the held lock's path, or an empty string for a zero proof.
func (l *Lock) Path() string {
	if l == nil || l.state == nil {
		return ""
	}
	return l.state.guard.Path()
}

// IDs returns the validated namespace identity, not claims from a token.
func (l *Lock) IDs() (string, string) {
	if l == nil || l.state == nil {
		return "", ""
	}
	return l.state.user, l.state.account
}

// Valid reports whether the proof still holds its original lock.
func (l *Lock) Valid() bool {
	return l != nil && l.state != nil && l.state.guard != nil && !l.state.released.Load()
}

// Release releases the lock once and invalidates all derived capabilities.
func (l *Lock) Release() error {
	if l == nil || l.state == nil {
		return nil
	}
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	if l.state.released.Swap(true) {
		return nil
	}
	return l.state.guard.Release()
}
