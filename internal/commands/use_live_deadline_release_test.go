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

package commands

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestUseSwapDurationIgnoresEnvironment(t *testing.T) {
	tests := map[string]struct{ value string }{
		"success: empty":                            {value: ""},
		"success: zero cannot expire release":       {value: "0"},
		"success: positive cannot shorten release":  {value: "1"},
		"success: positive cannot lengthen release": {value: "180000"},
		"success: invalid cannot change release":    {value: "invalid"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_SWAP_DEADLINE_MS", test.value)
			if diff := gocmp.Diff(useSwapDeadline, useSwapDuration()); diff != "" {
				t.Fatalf("release duration mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
