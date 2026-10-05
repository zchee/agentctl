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

package commands

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func TestUseIncomingIdentity(t *testing.T) {
	tests := map[string]struct {
		organization string
		want         *secret.IncomingIdentity
	}{
		"success: known organization is retained": {organization: "org", want: &secret.IncomingIdentity{AccountUUID: "account", OrganizationUUID: new("org")}},
		"success: unknown organization is absent": {organization: config.UnknownOrg, want: &secret.IncomingIdentity{AccountUUID: "account"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			record := &config.AccountRecord{AccountUUID: "account", OrganizationUUID: test.organization}
			got := useIncomingIdentity(record)
			if diff := gocmp.Diff(test.want, got); diff != "" {
				t.Fatalf("audit identity mismatch (-want +got):\n%s", diff)
			}
			actual := &claude.Identity{AccountUUID: "account", OrganizationUUID: new("other-org")}
			recorded := &claude.Identity{AccountUUID: got.AccountUUID, OrganizationUUID: got.OrganizationUUID}
			if agree := claude.IdentitiesAgree(actual, recorded); agree != (test.organization == config.UnknownOrg) {
				t.Fatalf("organization agreement=%t for registry organization %q", agree, test.organization)
			}
			actual.AccountUUID = "other-account"
			if claude.IdentitiesAgree(actual, recorded) {
				t.Fatal("different accounts must not agree even when the organization is unknown")
			}
		})
	}
}
