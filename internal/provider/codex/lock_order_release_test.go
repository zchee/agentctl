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

package codex

import (
	"errors"
	"testing"

	"github.com/zchee/agentctl/internal/runtime/lockorder"
)

func assertRegistryOrderRefused(t *testing.T, update func() error) {
	t.Helper()
	err := update()
	violation, ok := errors.AsType[*lockorder.OrderError](err)
	if !ok || violation.Owned != lockorder.CodexNamespace || violation.Wanted != lockorder.ConfigLock {
		t.Fatalf("registry inversion returned %T instead of the expected OrderError", err)
	}
}
