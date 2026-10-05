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
	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/zchee/agentctl/internal/secret"
)

// BuildOAuthAccount constructs the peer's account object in its field order.
// Absent and null values use the peer's defaults without coercing other types.
func BuildOAuthAccount(profile *Profile, nowMS int64) jsontext.Value {
	var document map[string]jsontext.Value
	if json.Unmarshal(profile.Document, &document) != nil {
		document = nil
	}
	block := func(key string) map[string]jsontext.Value {
		var values map[string]jsontext.Value
		if json.Unmarshal(document[key], &values) != nil {
			return nil
		}
		return values
	}
	account, organization := block("account"), block("organization")
	present := func(key, fallback string) jsontext.Value {
		value := organization[key]
		if len(value) == 0 || value.Kind() == 'n' {
			if fallback == "" {
				return nil
			}
			return jsontext.Value(fallback)
		}
		return value
	}
	truthy := func(key string) jsontext.Value {
		value := account[key]
		var decoded any
		if json.Unmarshal(value, &decoded) != nil {
			return nil
		}
		switch v := decoded.(type) {
		case nil:
			return nil
		case bool:
			if !v {
				return nil
			}
		case float64:
			if v == 0 {
				return nil
			}
		case string:
			if v == "" {
				return nil
			}
		}
		return value
	}
	value := struct {
		AccountUUID      string         `json:"accountUuid"`
		Email            string         `json:"emailAddress"`
		OrganizationUUID string         `json:"organizationUuid"`
		Extra            jsontext.Value `json:"hasExtraUsageEnabled"`
		Billing          jsontext.Value `json:"billingType,omitzero"`
		Created          jsontext.Value `json:"accountCreatedAt,omitzero"`
		Subscription     jsontext.Value `json:"subscriptionCreatedAt,omitzero"`
		Onboarding       jsontext.Value `json:"ccOnboardingFlags"`
		TrialEnd         jsontext.Value `json:"claudeCodeTrialEndsAt"`
		TrialDays        jsontext.Value `json:"claudeCodeTrialDurationDays"`
		Seat             jsontext.Value `json:"seatTier"`
		Display          jsontext.Value `json:"displayName,omitzero"`
		Full             jsontext.Value `json:"fullName,omitzero"`
		Fetched          int64          `json:"profileFetchedAt"`
	}{profile.AccountUUID, profile.Email, profile.OrganizationUUID, present("has_extra_usage_enabled", "false"), present("billing_type", ""), account["created_at"], present("subscription_created_at", ""), present("cc_onboarding_flags", "{}"), present("claude_code_trial_ends_at", "null"), present("claude_code_trial_duration_days", "null"), present("seat_tier", "null"), truthy("display_name"), truthy("full_name"), nowMS}
	// Every raw value came from a successful JSON parse.
	encoded, _ := json.Marshal(value)
	return encoded
}

func configAlreadyCurrent(document []byte, ids *secret.IncomingIdentity) bool {
	var object map[string]jsontext.Value
	if json.Unmarshal(document, &object) != nil {
		return false
	}
	var account map[string]jsontext.Value
	if json.Unmarshal(object["oauthAccount"], &account) != nil {
		return false
	}
	var accountID, orgID string
	return ids != nil && ids.OrganizationUUID != nil && account["accountUuid"].Kind() == '"' && account["organizationUuid"].Kind() == '"' && json.Unmarshal(account["accountUuid"], &accountID) == nil && json.Unmarshal(account["organizationUuid"], &orgID) == nil && accountID == ids.AccountUUID && orgID == *ids.OrganizationUUID
}
