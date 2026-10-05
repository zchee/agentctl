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

import (
	json "encoding/json/v2"
	"strconv"
	"strings"
	"testing"
)

func credentialLeaves(t *testing.T, document *Credentials) map[string]any {
	t.Helper()
	var value any
	if err := json.Unmarshal(credentialOutput(t, document), &value); err != nil {
		t.Fatal(err)
	}
	leaves := make(map[string]any)
	var walk func(string, any)
	walk = func(pointer string, value any) {
		switch node := value.(type) {
		case map[string]any:
			if len(node) > 0 {
				for name, child := range node {
					name = strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
					walk(pointer+"/"+name, child)
				}
				return
			}
		case []any:
			if len(node) > 0 {
				for index, child := range node {
					walk(pointer+"/"+strconv.Itoa(index), child)
				}
				return
			}
		}
		leaves[pointer] = value
	}
	walk("", value)
	return leaves
}

func assertNoCredentialNeedles(t *testing.T, rendered string) {
	t.Helper()
	for _, needle := range []string{"agctl-test-codex-at-", "agctl-test-codex-rt-", "agctl-test-codex-ak-", "agctl-test-codex-jwt-", "agctl-test-access", "agctl-test-refresh", "eyJ"} {
		if strings.Contains(rendered, needle) {
			t.Fatalf("rendered diagnostic contained a credential sentinel")
		}
	}
}
