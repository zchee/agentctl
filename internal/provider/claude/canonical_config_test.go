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

package claude

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRenderConfig(t *testing.T) {
	tests := map[string]struct {
		input, want string
		fail        bool
	}{
		"success: numeric and escape spellings": {
			input: `{"hasCompletedOnboarding":true,"theme":{"n":1e-7,"x":1e16,"minus":-0,"fraction":1.25,"text":"é\/\n\u001b"}}`,
			want:  "{\n  \"hasCompletedOnboarding\": true,\n  \"theme\": {\n    \"n\": 1e-7,\n    \"x\": 1e+16,\n    \"minus\": -0.0,\n    \"fraction\": 1.25,\n    \"text\": \"é/\\n\\u001b\"\n  }\n}",
		},
		"success: empty object":   {input: `{}`, want: `{}`},
		"error: invalid document": {input: `{"private-token`, fail: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := RenderConfig([]byte(tt.input))
			if (err != nil) != tt.fail {
				t.Fatalf("RenderConfig error=%v, want failure=%v", err, tt.fail)
			}
			if !tt.fail {
				if diff := gocmp.Diff(tt.want, string(got)); diff != "" {
					t.Fatalf("canonical output (-want +got):\n%s", diff)
				}
			}
		})
	}
}
