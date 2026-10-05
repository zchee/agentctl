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

package config

import (
	"context"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestPlanKeychainImport(t *testing.T) {
	const service = "Claude Code-credentials-12345678"
	const live = "Claude Code-credentials"
	identity := &ImportIdentity{AccountUUID: "account", OrganizationUUID: new("org"), Email: new("one@example.com"), OrgName: new("Imported Org")}
	tests := map[string]struct {
		items    []ImportItem
		existing Registry
		identity *ImportIdentity
		live     string
		reads    int
		records  int
		key      string
		contains string
	}{
		"success: named directory":                {items: []ImportItem{{Service: service, Dir: new("/work"), Listed: true}}, identity: identity, reads: 1, records: 1, key: "account/org", contains: "import config-dir-read-only: account/org (/work)"},
		"success: missing item":                   {items: []ImportItem{{Service: service, Dir: new("/missing")}}, contains: "skipped (no keychain item): no keychain item for /missing"},
		"success: alias warns":                    {items: []ImportItem{{Service: service, Dir: new("/alias"), Listed: true, SharesLiveDir: true}}, identity: identity, reads: 1, records: 1, key: "account/org", contains: "warning: /alias is an alias of the live config dir; recorded as a stale sibling"},
		"success: unknown identity uses service":  {items: []ImportItem{{Service: service, Listed: true}}, reads: 1, records: 1, key: service + "/" + UnknownOrg, contains: "identity unknown"},
		"success: missing organization":           {items: []ImportItem{{Service: service, Listed: true}}, identity: &ImportIdentity{AccountUUID: "account"}, reads: 1, records: 1, key: "account/" + UnknownOrg, contains: "imported 1, skipped 0, already known 0"},
		"success: live named":                     {items: []ImportItem{{Service: service, Dir: new("/work")}}, live: service, contains: "already known as live: live"},
		"success: live listed omitted":            {items: []ImportItem{{Service: live, Listed: true}}, contains: "imported 0, skipped 0, already known 0"},
		"success: empty directory refused":        {items: []ImportItem{{Service: live, Dir: new(""), Unsuffixed: true}}, contains: "skipped (names the live keychain item): "},
		"success: claimed named service":          {items: []ImportItem{{Service: service, Dir: new("/work"), Listed: true}}, existing: Registry{Accounts: []AccountRecord{{AccountUUID: "account", OrganizationUUID: "org", Kind: AccountKindConfigDirReadOnly("/work", service, false)}}}, contains: "already known as config-dir-read-only: " + service},
		"success: claimed listed service omitted": {items: []ImportItem{{Service: service, Listed: true}}, existing: Registry{Accounts: []AccountRecord{{Kind: AccountKindConfigDirReadOnly("", service, false)}}}, contains: "imported 0, skipped 0, already known 0"},
		"success: owned account never downgraded": {items: []ImportItem{{Service: service, Listed: true}}, identity: identity, existing: Registry{Accounts: []AccountRecord{{AccountUUID: "account", OrganizationUUID: "org", Kind: AccountKindOwned("/owned", "abcd1234")}}}, reads: 1, contains: "already known as owned: account"},
		"success: duplicate identity":             {items: []ImportItem{{Service: service, Listed: true}, {Service: "Claude Code-credentials-abcdef12", Listed: true}}, identity: identity, reads: 2, records: 1, key: "account/org", contains: "imported 1, skipped 1 (duplicate), already known 0"},
		"error: unusable account":                 {items: []ImportItem{{Service: service, Listed: true}}, identity: &ImportIdentity{AccountUUID: "../bad"}, reads: 1, contains: "skipped (unusable ids):"},
		"error: unusable organization":            {items: []ImportItem{{Service: service, Listed: true}}, identity: &ImportIdentity{AccountUUID: "account", OrganizationUUID: new("../bad")}, reads: 1, contains: "skipped (unusable ids):"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var reads int
			current := tt.live
			if current == "" {
				current = live
			}
			plan := PlanKeychainImport(t.Context(), tt.items, current, &tt.existing, func(context.Context, string) *ImportIdentity { reads++; return tt.identity })
			if diff := gocmp.Diff(tt.reads, reads); diff != "" {
				t.Errorf("reads (-want +got):\n%s", diff)
			}
			records := plan.Records()
			if diff := gocmp.Diff(tt.records, len(records)); diff != "" {
				t.Fatalf("records (-want +got):\n%s", diff)
			}
			if len(records) > 0 {
				if diff := gocmp.Diff(tt.key, records[0].AccountUUID+"/"+records[0].OrganizationUUID); diff != "" {
					t.Errorf("key (-want +got):\n%s", diff)
				}
				if records[0].Kind.ConfigDirReadOnly == nil {
					t.Fatal("import must only create read-only records")
				}
				if tt.identity != nil && !gocmp.Equal(tt.identity.Email, records[0].Email) {
					t.Error("email not preserved")
				}
			}
			if output := strings.Join(plan.Lines(), "\n"); !strings.Contains(output, tt.contains) {
				t.Errorf("output %q does not contain %q", output, tt.contains)
			}
		})
	}
}

func TestImportSummarySortsReasons(t *testing.T) {
	plan := ImportPlan{Decisions: []ImportDecision{
		{Reason: "unusable ids"}, {Reason: "duplicate"}, {Reason: "duplicate"}, {Warning: "ignored"}, {KnownKind: "owned"},
	}}
	want := "imported 0, skipped 3 (2 duplicate, 1 unusable ids), already known 1"
	if diff := gocmp.Diff(want, plan.Summary()); diff != "" {
		t.Fatalf("summary (-want +got):\n%s", diff)
	}
}
