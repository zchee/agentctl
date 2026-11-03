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

package claude

// PlanLabel returns the subscription's display label, or empty when unknown.
// Matching is exact and case-sensitive. This is display only: the stored
// subscription and rate-limit tier words are unchanged.
func PlanLabel(c *Credentials) string {
	if c == nil || c.SubscriptionType == nil {
		return ""
	}
	plan := *c.SubscriptionType
	if plan == "max" && c.RateLimitTier != nil && *c.RateLimitTier == "default_claude_max_20x" {
		return "ultra"
	}
	return plan
}
