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

package schemas_test

import (
	json "encoding/json/v2"
	"testing"

	"github.com/zchee/agentctl/schemas"
)

// TestFSContents proves each schema document is embedded and is valid JSON,
// so a truncated or dropped file fails here instead of inside a consumer.
func TestFSContents(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
	}{
		"success: claude status schema embedded":    {path: "status.v1.json"},
		"success: codex status schema embedded":     {path: "status.v2.json"},
		"success: isolation report schema embedded": {path: "doctor.v1.json"},
		"success: codex doctor schema embedded":     {path: "codex-doctor.v1.json"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := schemas.FS.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", tt.path, err)
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("Unmarshal(%q): %v", tt.path, err)
			}
			if _, ok := doc["$schema"]; !ok {
				t.Fatalf("schema %q has no $schema member", tt.path)
			}
		})
	}
}
