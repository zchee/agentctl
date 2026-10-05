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

func TestOAuthEndpointsFailClosedWithoutAnOverride(t *testing.T) {
	tests := map[string]struct {
		envName string
		value   string
		resolve func() string
		want    string
	}{
		"success: an unset token override fails closed to a dead port": {
			envName: TokenURLEnv,
			value:   "",
			resolve: oauthTokenURL,
			want:    "http://127.0.0.1:9",
		},
		"success: a blank profile override fails closed to a dead port": {
			envName: ProfileURLEnv,
			value:   "   ",
			resolve: oauthProfileURL,
			want:    "http://127.0.0.1:9",
		},
		"success: a set token override is used verbatim": {
			envName: TokenURLEnv,
			value:   "http://127.0.0.1:4545/v1/oauth/token",
			resolve: oauthTokenURL,
			want:    "http://127.0.0.1:4545/v1/oauth/token",
		},
		"success: a set profile override is used verbatim": {
			envName: ProfileURLEnv,
			value:   "http://127.0.0.1:4545/api/oauth/profile",
			resolve: oauthProfileURL,
			want:    "http://127.0.0.1:4545/api/oauth/profile",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tt.envName, tt.value)
			if got := tt.resolve(); got != tt.want {
				t.Errorf("resolved endpoint = %q, want %q", got, tt.want)
			}
		})
	}
}
