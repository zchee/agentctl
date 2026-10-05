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

//go:build !agentctl_testing

package claude

import "testing"

func TestOAuthEndpointsAreTheVendorsInARelease(t *testing.T) {
	if got, want := oauthTokenURL(), "https://platform.claude.com/v1/oauth/token"; got != want {
		t.Errorf("oauthTokenURL() = %q, want %q", got, want)
	}
	if got, want := oauthProfileURL(), "https://api.anthropic.com/api/oauth/profile"; got != want {
		t.Errorf("oauthProfileURL() = %q, want %q", got, want)
	}
}
