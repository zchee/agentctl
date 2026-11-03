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

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestPlanLabel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		credentials *Credentials
		want        string
	}{
		"success: the twentyfold max tier displays ultra":              {credentials: &Credentials{SubscriptionType: new("max"), RateLimitTier: new("default_claude_max_20x")}, want: "ultra"},
		"success: the fivefold max tier displays max":                  {credentials: &Credentials{SubscriptionType: new("max"), RateLimitTier: new("default_claude_max_5x")}, want: "max"},
		"success: an unknown max tier keeps max":                       {credentials: &Credentials{SubscriptionType: new("max"), RateLimitTier: new("future_tier")}, want: "max"},
		"success: a missing max tier keeps max":                        {credentials: &Credentials{SubscriptionType: new("max")}, want: "max"},
		"success: team stays team despite a max tier":                  {credentials: &Credentials{SubscriptionType: new("team"), RateLimitTier: new("default_claude_max_5x")}, want: "team"},
		"success: a non-max plan stays verbatim despite an ultra tier": {credentials: &Credentials{SubscriptionType: new("enterprise"), RateLimitTier: new("default_claude_max_20x")}, want: "enterprise"},
		"success: plan matching is case sensitive":                     {credentials: &Credentials{SubscriptionType: new("Max"), RateLimitTier: new("default_claude_max_20x")}, want: "Max"},
		"success: tier matching is case sensitive":                     {credentials: &Credentials{SubscriptionType: new("max"), RateLimitTier: new("DEFAULT_CLAUDE_MAX_20X")}, want: "max"},
		"success: plan words are not trimmed":                          {credentials: &Credentials{SubscriptionType: new(" max"), RateLimitTier: new("default_claude_max_20x")}, want: " max"},
		"success: a missing subscription has no label":                 {credentials: &Credentials{RateLimitTier: new("default_claude_max_20x")}},
		"success: a missing credential has no label":                   {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var plan, tier *string
			if tt.credentials != nil {
				plan, tier = tt.credentials.SubscriptionType, tt.credentials.RateLimitTier
			}
			if diff := gocmp.Diff(tt.want, PlanLabel(tt.credentials)); diff != "" {
				t.Errorf("plan label mismatch (-want +got):\n%s", diff)
			}
			if tt.credentials != nil && (tt.credentials.SubscriptionType != plan || tt.credentials.RateLimitTier != tier) {
				t.Error("displaying the plan changed the stored metadata")
			}
		})
	}
}
