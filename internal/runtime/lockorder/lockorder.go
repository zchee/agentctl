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

// Package lockorder witnesses one lock-order rule: an operation never
// takes the registry's configuration lock while it owns a Codex namespace
// guard.
//
// A login releases the namespace lock before it records the account, and
// an import does the same. Pinned by a comment alone, that order is one
// refactor away from silent inversion — moving the registry update inside
// the guarded block would pass every suite — and the inverted order is a
// deadlock shape: one process holding the namespace guard and wanting the
// configuration lock, another holding the configuration lock and wanting
// the guard.
//
// The witness is an explicit ownership record carried in the operation's
// context.Context, not in any goroutine-local state: the rule is about
// what this operation holds when it blocks on the configuration lock, a
// guard some other operation holds is not an ordering fault here, and Go
// work moves freely between goroutines. Whoever acquires a guard derives
// the operation's context with WithOwned and passes the derived context
// down; releasing the guard means going back to using the parent context,
// so ownership scopes exactly like the guard itself. The acquisition of
// the configuration lock calls Check right before it blocks.
//
// In a tagged build a violation panics, so the test that reaches the
// forbidden order fails loudly at the exact call site; a release build
// returns the typed error instead, refusing the operation rather than
// crashing a user's run on an invariant the suite should have caught.
package lockorder

import (
	"context"
	"maps"
)

// Kind names one lock or guard the witness tracks.
type Kind int

const (
	// CodexNamespace is a held Codex namespace guard.
	CodexNamespace Kind = iota
	// ConfigLock is the registry's configuration lock.
	ConfigLock
)

// String returns the name diagnostics print for this kind.
func (k Kind) String() string {
	switch k {
	case CodexNamespace:
		return "a Codex namespace guard"
	case ConfigLock:
		return "the configuration lock"
	default:
		return "an unknown lock"
	}
}

// OrderError reports a forbidden acquisition order: the operation asked
// for Wanted while owning Owned.
type OrderError struct {
	// Owned is what the operation already holds.
	Owned Kind
	// Wanted is what it asked for.
	Wanted Kind
}

// Error names both sides of the inversion. The wording here is the
// release build's refusal; the gated witness literal lives only in the
// tagged build.
func (e *OrderError) Error() string {
	return "refusing to take " + e.Wanted.String() + " while this operation holds " + e.Owned.String()
}

// ctxKey carries the ownership record; the record is immutable, so a
// derived context never mutates what its parent observes.
type ctxKey struct{}

// ownership counts the guards an operation's context owns, per kind.
type ownership struct {
	counts map[Kind]int
}

// WithOwned returns a context that additionally owns one guard of kind.
// Pass the derived context down for exactly as long as the guard is held;
// code that runs after the release goes back to the parent context, so
// ownership scopes the way the guard itself does.
func WithOwned(ctx context.Context, kind Kind) context.Context {
	counts := make(map[Kind]int)
	if prev, ok := ctx.Value(ctxKey{}).(ownership); ok {
		maps.Copy(counts, prev.counts)
	}
	counts[kind]++
	return context.WithValue(ctx, ctxKey{}, ownership{counts: counts})
}

// OwnedCount returns how many guards of kind the operation's context
// owns.
func OwnedCount(ctx context.Context, kind Kind) int {
	if record, ok := ctx.Value(ctxKey{}).(ownership); ok {
		return record.counts[kind]
	}
	return 0
}

// Check decides whether the operation may take wanted now. It is called
// right before the acquisition would block, which is the one point where
// the inverted order becomes a deadlock shape.
//
// The single rule: the configuration lock may not be requested while the
// operation owns a Codex namespace guard. In a tagged build a violation
// panics; a release build returns the typed *OrderError.
func Check(ctx context.Context, wanted Kind) error {
	if wanted == ConfigLock && OwnedCount(ctx, CodexNamespace) > 0 {
		return report(&OrderError{Owned: CodexNamespace, Wanted: ConfigLock})
	}
	return nil
}
