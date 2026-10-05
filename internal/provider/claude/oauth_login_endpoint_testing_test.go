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

package claude

import "testing"

func TestLoginEndpointOverride(t *testing.T) {
	tests := map[string]struct{ value, want string }{
		"success: unset fails closed":      {"", "http://127.0.0.1:9"},
		"success: whitespace fails closed": {" \t", "http://127.0.0.1:9"},
		"success: explicit endpoint":       {"http://127.0.0.1:1234/authorize", "http://127.0.0.1:1234/authorize"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_CLAUDE_AUTHORIZE_URL", test.value)
			if got := oauthAuthorizeURL(); got != test.want {
				t.Fatalf("authorize URL = %q; want %q", got, test.want)
			}
		})
	}
}
