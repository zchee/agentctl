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

package signals

import (
	"testing"
	"time"
)

func TestDeferralLimit(t *testing.T) {
	tests := map[string]struct {
		value string
		want  time.Duration
	}{
		"success: default":   {want: exitDeferralLimit},
		"success: shortened": {value: "200ms", want: 200 * time.Millisecond},
		"error: invalid":     {value: "not a duration", want: exitDeferralLimit},
		"error: zero":        {value: "0s", want: exitDeferralLimit},
		"error: negative":    {value: "-1s", want: exitDeferralLimit},
		"error: extended":    {value: "11s", want: exitDeferralLimit},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_TEST_EXIT_DEFERRAL", tt.value)
			if got := deferralLimit(); got != tt.want {
				t.Errorf("deferral = %v, want %v", got, tt.want)
			}
		})
	}
}
