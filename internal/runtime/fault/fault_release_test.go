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

package fault

import (
	"testing"
	"time"
)

// A release build must have no way to reach an injection: the factory
// ignores the environment, and every pause compiles to an immediate
// return. The environment names are spelled out here rather than through
// the constants, because the constants do not exist in this build — which
// is itself part of what the release gate proves.
func TestReleaseBuildIsInert(t *testing.T) {
	tests := map[string]struct{}{
		"success: the factory ignores the environment and every pause returns at once": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_FAULT", "pause_before_rename,rename_fail")
			t.Setenv("AGENTCTL_FAULT_RESUME", "")

			f := Active()
			if f.Is("rename_fail") {
				t.Error("a release factory produced an active fault")
			}

			started := time.Now()
			f.PausePoint("before_rename")
			f.WaitIf("pause_before_rename")
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Errorf("a release pause held for %v, want an immediate return", elapsed)
			}
		})
	}
}
