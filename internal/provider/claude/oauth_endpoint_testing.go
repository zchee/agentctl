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

import (
	"os"
	"strings"
)

// TokenURLEnv redirects the token endpoint in a tagged build.
const TokenURLEnv = "AGENTCTL_CLAUDE_TOKEN_URL"

// ProfileURLEnv redirects the profile endpoint in a tagged build.
const ProfileURLEnv = "AGENTCTL_CLAUDE_PROFILE_URL"

// closedOAuthEndpoint is where a tagged build sends OAuth requests when no
// override is set: a loopback port nothing listens on, so a test that
// forgot to set its endpoint fails fast instead of silently calling the
// vendor with a real token.
const closedOAuthEndpoint = "http://127.0.0.1:9"

// oauthTokenURL returns the token endpoint this build talks to, failing
// closed when the override is unset.
func oauthTokenURL() string {
	if override := strings.TrimSpace(os.Getenv(TokenURLEnv)); override != "" {
		return override
	}
	return closedOAuthEndpoint
}

// oauthProfileURL returns the profile endpoint this build talks to, failing
// closed when the override is unset.
func oauthProfileURL() string {
	if override := strings.TrimSpace(os.Getenv(ProfileURLEnv)); override != "" {
		return override
	}
	return closedOAuthEndpoint
}
