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

package commands

import (
	"os"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestUseSwapDurationEnvironment(t *testing.T) {
	tests := map[string]struct {
		value *string
		want  time.Duration
	}{
		"success: unset uses default":           {want: useSwapDeadline},
		"success: empty uses default":           {value: new(""), want: useSwapDeadline},
		"success: zero expires at adoption":     {value: new("0")},
		"success: positive milliseconds":        {value: new("1234"), want: 1234 * time.Millisecond},
		"success: longer than default":          {value: new("180000"), want: 3 * time.Minute},
		"error: negative uses default":          {value: new("-1"), want: useSwapDeadline},
		"error: malformed uses default":         {value: new("invalid"), want: useSwapDeadline},
		"error: fractional uses default":        {value: new("1.5"), want: useSwapDeadline},
		"error: duration overflow uses default": {value: new("9223372036855"), want: useSwapDeadline},
		"error: integer overflow uses default":  {value: new("18446744073709551616"), want: useSwapDeadline},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_SWAP_DEADLINE_MS", "")
			if test.value == nil {
				if err := os.Unsetenv("AGENTCTL_SWAP_DEADLINE_MS"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("AGENTCTL_SWAP_DEADLINE_MS", *test.value)
			}
			if diff := gocmp.Diff(test.want, useSwapDuration()); diff != "" {
				t.Fatalf("testing duration mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
