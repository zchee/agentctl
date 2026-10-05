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

import "testing"

func TestPostPermitOnlyExposesItsClientInsideProvider(t *testing.T) {
	tests := map[string]struct {
		permit *PostPermit
		valid  bool
	}{
		"success: constructed permit": {NewPostPermit(), true},
		"error: zero permit":          {new(PostPermit), false},
		"error: absent permit":        {nil, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if (test.permit.client() != nil) != test.valid {
				t.Fatal("permit validity drift")
			}
		})
	}
}
