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

// UsageURLEnv redirects the usage base URL in a tagged build.
const UsageURLEnv = "AGENTCTL_CLAUDE_USAGE_URL"

// usageBaseURL returns the base URL the usage client talks to. A tagged
// build without an explicit override fails closed to a loopback port
// nothing listens on, so a test that forgot to set its endpoint fails
// fast instead of silently calling the vendor.
func usageBaseURL() string {
	if override := strings.TrimSpace(os.Getenv(UsageURLEnv)); override != "" {
		return override
	}
	return "http://127.0.0.1:9"
}
