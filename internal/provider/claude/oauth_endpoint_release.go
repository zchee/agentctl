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

// TokenURL is the token endpoint, for both OAuth grants.
const TokenURL = "https://platform.claude.com/v1/oauth/token"

// ProfileURL is the profile endpoint: who a credential belongs to, as the
// server names it.
const ProfileURL = "https://api.anthropic.com/api/oauth/profile"

// oauthTokenURL returns the token endpoint this build talks to. A release
// build always talks to the vendor: an environment override of an endpoint
// that receives a refresh token would be an exfiltration vector, so none
// exists outside the testing build tag.
func oauthTokenURL() string {
	return TokenURL
}

// oauthProfileURL returns the profile endpoint this build talks to, under
// the same no-override rule as the token endpoint: the request carries a
// bearer token.
func oauthProfileURL() string {
	return ProfileURL
}
