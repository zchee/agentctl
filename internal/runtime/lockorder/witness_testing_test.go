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

//go:build agentctl_testing

package lockorder_test

import (
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/runtime/lockorder"
)

func TestForbiddenOrderPanicsUnderTheTag(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		wantPrefix string
	}{
		"error: the configuration lock under a namespace guard panics with the witness literal": {
			wantPrefix: "agentctl lock order violated: ",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				v := recover()
				if v == nil {
					t.Fatal("the forbidden order did not panic in a tagged build")
				}
				message, ok := v.(string)
				if !ok || !strings.HasPrefix(message, tt.wantPrefix) {
					t.Errorf("panic = %v, want a string with prefix %q", v, tt.wantPrefix)
				}
			}()

			ctx := lockorder.WithOwned(t.Context(), lockorder.CodexNamespace)
			_ = lockorder.Check(ctx, lockorder.ConfigLock)
		})
	}
}
