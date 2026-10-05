//go:build agentctl_testing

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
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider"
)

func TestUsageTestingEndpointFailsClosed(t *testing.T) {
	tests := map[string]struct {
		base string
		want string
	}{
		"error: empty fallback":      {want: "http://127.0.0.1:9"},
		"error: whitespace fallback": {base: " \t", want: "http://127.0.0.1:9"},
		"success: explicit endpoint": {base: "http://127.0.0.1:12345", want: "http://127.0.0.1:12345"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_CODEX_USAGE_URL", tt.base)
			client := NewUsageClientFromEnv(time.Second)
			if diff := gocmp.Diff(tt.want+UsagePath, client.UsageURL()); diff != "" {
				t.Fatal(diff)
			}
			if tt.base == "" {
				_, err := client.Fetch(t.Context(), provider.AccountRef{ID: "row", Auth: testUsageAuth(t)})
				if err == nil {
					t.Fatal("fallback accepted a request")
				}
			}
		})
	}
}
