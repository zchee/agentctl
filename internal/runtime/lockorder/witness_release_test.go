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

//go:build !agentctl_testing

package lockorder_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/runtime/lockorder"
)

func TestForbiddenOrderRefusesInRelease(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{}{
		"error: the configuration lock under a namespace guard returns the typed refusal": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := lockorder.WithOwned(t.Context(), lockorder.CodexNamespace)
			err := lockorder.Check(ctx, lockorder.ConfigLock)
			if err == nil {
				t.Fatal("the forbidden order was allowed in a release build")
			}

			orderErr, ok := errors.AsType[*lockorder.OrderError](err)
			if !ok {
				t.Fatalf("Check error = %T, want *lockorder.OrderError", err)
			}
			if orderErr.Owned != lockorder.CodexNamespace || orderErr.Wanted != lockorder.ConfigLock {
				t.Errorf("OrderError = %+v, want Owned CodexNamespace, Wanted ConfigLock", orderErr)
			}
			// The release wording must not carry the gated witness
			// literal: the release gate proves that string absent from a
			// shipped binary.
			if strings.Contains(err.Error(), "lock order violated") {
				t.Errorf("release refusal %q carries the gated witness literal", err)
			}
		})
	}
}
