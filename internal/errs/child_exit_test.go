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

package errs

import (
	"fmt"
	"testing"
)

func TestChildExit(t *testing.T) {
	tests := map[string]struct {
		err  error
		want int
	}{
		"error: ordinary child exit":  {&ChildExit{Code: 37}, 37},
		"error: signalled child exit": {&ChildExit{Code: 143}, 143},
		"error: wrapped child exit":   {fmt.Errorf("wrapper: %w", &ChildExit{Code: 7}), 7},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Fatalf("ExitCode=%d, want %d", got, tt.want)
			}
		})
	}
}
