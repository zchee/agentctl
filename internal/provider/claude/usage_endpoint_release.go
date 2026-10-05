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

// DefaultUsageBaseURL is where the usage endpoint lives.
const DefaultUsageBaseURL = "https://api.anthropic.com"

// usageBaseURL returns the base URL the usage client talks to. A release
// build always talks to the vendor: an environment override of an
// endpoint that receives a bearer token would be an exfiltration vector,
// so none exists outside the testing build tag.
func usageBaseURL() string {
	return DefaultUsageBaseURL
}
