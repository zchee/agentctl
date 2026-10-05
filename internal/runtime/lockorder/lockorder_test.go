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

package lockorder_test

import (
	"context"
	"testing"

	"github.com/zchee/agentctl/internal/runtime/lockorder"
)

func TestWithOwned(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		build func(ctx context.Context) context.Context
		kind  lockorder.Kind
		want  int
	}{
		"success: a fresh context owns nothing": {
			build: func(ctx context.Context) context.Context { return ctx },
			kind:  lockorder.CodexNamespace,
			want:  0,
		},
		"success: one derivation owns one guard": {
			build: func(ctx context.Context) context.Context {
				return lockorder.WithOwned(ctx, lockorder.CodexNamespace)
			},
			kind: lockorder.CodexNamespace,
			want: 1,
		},
		"success: nested derivations count each guard": {
			build: func(ctx context.Context) context.Context {
				ctx = lockorder.WithOwned(ctx, lockorder.CodexNamespace)
				return lockorder.WithOwned(ctx, lockorder.CodexNamespace)
			},
			kind: lockorder.CodexNamespace,
			want: 2,
		},
		"success: kinds are counted independently": {
			build: func(ctx context.Context) context.Context {
				return lockorder.WithOwned(ctx, lockorder.ConfigLock)
			},
			kind: lockorder.CodexNamespace,
			want: 0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := tt.build(t.Context())
			if got := lockorder.OwnedCount(ctx, tt.kind); got != tt.want {
				t.Errorf("OwnedCount = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestOwnershipScopes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{}{
		"success: a parent context is untouched by a derived guard": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			parent := t.Context()
			derived := lockorder.WithOwned(parent, lockorder.CodexNamespace)

			if got := lockorder.OwnedCount(derived, lockorder.CodexNamespace); got != 1 {
				t.Errorf("derived OwnedCount = %d, want 1", got)
			}
			// Releasing the guard means going back to the parent, which
			// must still own nothing — the record is immutable.
			if got := lockorder.OwnedCount(parent, lockorder.CodexNamespace); got != 0 {
				t.Errorf("parent OwnedCount = %d, want 0", got)
			}
			if err := lockorder.Check(parent, lockorder.ConfigLock); err != nil {
				t.Errorf("Check on the parent after scoping = %v, want nil", err)
			}
		})
	}
}

func TestCheckAllowedOrders(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		build  func(ctx context.Context) context.Context
		wanted lockorder.Kind
	}{
		"success: the configuration lock is free when nothing is owned": {
			build:  func(ctx context.Context) context.Context { return ctx },
			wanted: lockorder.ConfigLock,
		},
		"success: a namespace guard may be taken under the configuration lock": {
			build: func(ctx context.Context) context.Context {
				return lockorder.WithOwned(ctx, lockorder.ConfigLock)
			},
			wanted: lockorder.CodexNamespace,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := tt.build(t.Context())
			if err := lockorder.Check(ctx, tt.wanted); err != nil {
				t.Errorf("Check = %v, want nil", err)
			}
		})
	}
}
