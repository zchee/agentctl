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

package testutil

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestCodexTranscriptNormalization(t *testing.T) {
	tests := map[string]struct{ input, want string }{
		"success: empty stream":                            {},
		"success: only report timestamp replaced":          {input: "  \"generated_at\": \"2026-09-17T12:00:00Z\",\n  \"created_at\": \"2026-09-17T00:00:00Z\"\n", want: "  \"generated_at\": <per-run>\n  \"created_at\": \"2026-09-17T00:00:00Z\"\n"},
		"success: lock holder tail replaced":               {input: "lock holder  pid 123 (alive), taken 2026-09-17T00:00:00Z\n", want: "lock holder  <per-run>\n"},
		"success: nonnumeric pid text unchanged":           {input: "pid unavailable\n", want: "pid unavailable\n"},
		"success: other timestamp and row bytes preserved": {input: "owned  owner@example.com  2026-09-17T00:00:00Z\n", want: "owned  owner@example.com  2026-09-17T00:00:00Z\n"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, normalizeCodexTranscript(test.input)); diff != "" {
				t.Fatalf("transcript (-want +got):\n%s", diff)
			}
		})
	}
}
