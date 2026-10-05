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
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/lockfile"
	"github.com/zchee/agentctl/internal/runtime/lockorder"
)

func TestRegistryOrderUsesOperationContext(t *testing.T) {
	tests := map[string]struct{ contended bool }{
		"error: registry update under codex lock":           {},
		"error: order check precedes contended config lock": {contended: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, _ := writerNamespace(t)
			if lockorder.OwnedCount(guard.Context(), lockorder.CodexNamespace) != 1 {
				t.Fatal("guard did not witness ownership")
			}
			if lockorder.OwnedCount(t.Context(), lockorder.CodexNamespace) != 0 {
				t.Fatal("ownership mutated the parent operation")
			}
			if tt.contended {
				held, err := lockfile.Lock(t.Context(), paths.ConfigLock(), time.Now().Add(time.Second))
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := held.Release(); err != nil {
						t.Error(err)
					}
				}()
			}
			assertRegistryOrderRefused(t, func() error {
				return config.UpdateRegistry(guard.Context(), paths, func(*config.Registry) { t.Error("forbidden update reached mutation") })
			})
		})
	}
}

func TestRegistryUpdateAfterReleaseAndIndependentOperation(t *testing.T) {
	paths, _, guard, _ := writerNamespace(t)
	otherOperation := make(chan error, 1)
	go func() { otherOperation <- config.UpdateRegistry(t.Context(), paths, func(*config.Registry) {}) }()
	if err := <-otherOperation; err != nil {
		t.Fatal("an independent context inherited another operation's guard", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateRegistry(t.Context(), paths, func(*config.Registry) {}); err != nil {
		t.Fatal("parent context could not update after release", err)
	}
}

func TestContendedRegistryWithoutCodexOwnershipIsRefused(t *testing.T) {
	paths, _, guard, _ := writerNamespace(t)
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	held, err := lockfile.Lock(t.Context(), paths.ConfigLock(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Release(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	err = config.UpdateRegistry(ctx, paths, func(*config.Registry) { t.Error("contended update reached mutation") })
	if _, ok := errors.AsType[*errs.RefusedError](err); !ok {
		t.Fatalf("lock contention returned %T rather than RefusedError", err)
	}
}
