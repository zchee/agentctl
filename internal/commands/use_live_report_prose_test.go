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
	"bytes"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func TestUseProseReportsWhatTheWriteCarried(t *testing.T) {
	applied := func(carried *[]useCarriedMember, readback *secret.SiblingsReadback, transport *secret.WriteTransport) *useReport {
		return &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapApplied}, service: "Claude Code-credentials", toDigest8: new("0a1b2c3d"), auditID: new("audit-id"), carriedMembers: carried, siblingsReadback: readback, transport: transport}
	}
	argv := new(secret.TransportArgv)
	tests := map[string]struct {
		report *useReport
		// want is every stdout line after the outcome line.
		want []string
	}{
		"success: carried members read back equal": {
			report: applied(&[]useCarriedMember{{Name: "mcpOAuth", Entries: new(2)}, {Name: "mcpOAuthClientConfig", Entries: new(1)}, {Name: "pluginSecrets"}}, new(secret.SiblingsEqual), argv),
			want: []string{
				"audit: audit-id",
				"keychain item members: carried mcpOAuth (2 entries), mcpOAuthClientConfig (1 entry), pluginSecrets; readback equal; transport argv",
			},
		},
		"success: an item with nothing beside the account carries none": {
			report: applied(&[]useCarriedMember{}, new(secret.SiblingsEqual), argv),
			want: []string{
				"audit: audit-id",
				"keychain item members: carried none; readback equal; transport argv",
			},
		},
		"success: a failed readback leaves the carry unverified": {
			report: &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapUnknown}, service: "Claude Code-credentials", note: new("the write could not be confirmed; re-run `agentctl claude status`"), auditID: new("audit-id"), carriedMembers: &[]useCarriedMember{{Name: "mcpOAuth", Entries: new(3)}}, siblingsReadback: new(secret.SiblingsUnverified), transport: argv},
			want: []string{
				"audit: audit-id",
				"keychain item members: carried mcpOAuth (3 entries); readback unverified; transport argv",
			},
		},
		"success: a discard after the update was built names the carry only": {
			report: &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapDiscarded}, service: "Claude Code-credentials", note: new("the hold ran out of budget before the write"), carriedMembers: &[]useCarriedMember{{Name: "mcpOAuth", Entries: new(1)}}},
			want: []string{
				"keychain item members: carried mcpOAuth (1 entry)",
			},
		},
		"success: a refusal says nothing about members": {
			report: useRefused(claude.SwapRefusal{Kind: claude.SwapCredentialTooLarge}, "Claude Code-credentials", "too large"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := (SessionProcess{Out: &stdout, Err: &stderr}).emitUse(tt.report, false); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
			if len(lines) == 0 || !strings.HasPrefix(lines[0], tt.report.outcome.Word()+":") && !strings.HasPrefix(lines[0], "swapped:") {
				t.Fatalf("the first line is not the outcome:\n%s", stdout.String())
			}
			got := lines[1:]
			if len(got) == 0 {
				got = nil
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("stdout after the outcome line (-want +got):\n%s", diff)
			}
		})
	}
}
