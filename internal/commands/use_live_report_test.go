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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func TestUseOutcomeJSONReferenceOrder(t *testing.T) {
	// Literals are reference binary captures, with the temporary home normalized.
	tests := map[string]struct {
		report *useReport
		want   string
	}{
		"success: reason follows config": {
			report: useRefused(claude.SwapRefusal{Kind: claude.SwapLiveUnreachable}, claude.LiveService, "the live store `/home/.claude` could not be resolved: No such file or directory (os error 2)"),
			want: `{
  "kind": "outcome",
  "outcome": "refused",
  "target": null,
  "service": "Claude Code-credentials",
  "from": {
    "digest8": null
  },
  "to": {
    "digest8": null
  },
  "audit": {
    "id": null
  },
  "adopted_to": null,
  "lock": {
    "hold_ms": null,
    "budget_ms": null,
    "break": null
  },
  "warnings": [],
  "note": "the live store ` + "`/home/.claude`" + ` could not be resolved: No such file or directory (os error 2)",
  "refusal": null,
  "config": null,
  "reason": "live_unreachable"
}
`,
		},
		"success: letter replaces refusal in place": {
			report: &useReport{
				outcome: claude.SwapOutcome{Kind: claude.SwapRefused, Refusal: claude.SwapRefusal{Kind: claude.SwapEnvToken}},
				target:  new("namespace:6a38ef6e"),
				service: "Claude Code-credentials-6a38ef6e",
				note:    new("`CLAUDE_CODE_OAUTH_TOKEN` is set in agctl's own environment, which short-circuits every credential store; unset it and run this again"),
			},
			want: `{
  "kind": "outcome",
  "outcome": "refused",
  "target": "namespace:6a38ef6e",
  "service": "Claude Code-credentials-6a38ef6e",
  "from": {
    "digest8": null
  },
  "to": {
    "digest8": null
  },
  "audit": {
    "id": null
  },
  "adopted_to": null,
  "lock": {
    "hold_ms": null,
    "budget_ms": null,
    "break": null
  },
  "warnings": [],
  "note": "` + "`CLAUDE_CODE_OAUTH_TOKEN`" + ` is set in agctl's own environment, which short-circuits every credential store; unset it and run this again",
  "refusal": "C",
  "config": null
}
`,
		},
		"success: config retains its reference member order": {
			report: &useReport{
				outcome:     claude.SwapOutcome{Kind: claude.SwapApplied},
				target:      new("live"),
				service:     claude.LiveService,
				fromDigest8: new("6ffa07d8"),
				toDigest8:   new("636f49a9"),
				auditID:     new("2026-10-05T21:30:55.941888Z#26589"),
				adoptedTo:   new(".credentials.adopted.json"),
				lock:        useLockReport{HoldMS: new(uint64(78)), BudgetMS: new(uint64(3000))},
				warnings:    []string{"agctl inspected its own environment for a secure-storage backend and found none; it cannot inspect the target session's"},
				config:      &claude.ConfigReport{Outcome: secret.ConfigApplied, Backup: new(".claude.json.backup.1791235855951"), HoldMS: new(uint64(22))},
			},
			want: `{
  "kind": "outcome",
  "outcome": "applied",
  "target": "live",
  "service": "Claude Code-credentials",
  "from": {
    "digest8": "6ffa07d8"
  },
  "to": {
    "digest8": "636f49a9"
  },
  "audit": {
    "id": "2026-10-05T21:30:55.941888Z#26589"
  },
  "adopted_to": ".credentials.adopted.json",
  "lock": {
    "hold_ms": 78,
    "budget_ms": 3000,
    "break": null
  },
  "warnings": [
    "agctl inspected its own environment for a secure-storage backend and found none; it cannot inspect the target session's"
  ],
  "note": null,
  "refusal": null,
  "config": {
    "outcome": "applied",
    "reason": null,
    "backup": ".claude.json.backup.1791235855951",
    "hold_ms": 22,
    "budget_ms": 1200
  }
}
`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := (SessionProcess{Out: &out}).emitUse(test.report, true); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, out.String()); diff != "" {
				t.Fatalf("reference JSON differs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUsePlanJSONReferenceOrder(t *testing.T) {
	// The reference capture's temporary store path is normalized to /store.
	tests := map[string]struct {
		direction  secret.WriteDirection
		account    config.AccountRecord
		from       *string
		configPath *string
		want       string
	}{
		"success: reference forward plan": {
			direction: secret.DirectionForward,
			account:   config.AccountRecord{AccountUUID: "44444444-4444-4444-8444-444444444444"},
			from:      new("6ffa07d8"),
			want: `{
  "kind": "plan",
  "direction": "forward",
  "store_dir": "/store",
  "service": "Claude Code-credentials-93e299a5",
  "account": "44444444-4444-4444-8444-444444444444",
  "from": {
    "digest8": "6ffa07d8"
  },
  "to": {
    "digest8": "636f49a9"
  },
  "config_path": null
}
`,
		},
		"success: reverse plan retains populated members and escaping": {
			direction:  secret.DirectionUndo,
			account:    config.AccountRecord{AccountUUID: "<account>&"},
			from:       new("87654321"),
			configPath: new("/home/.claude.json"),
			want: `{
  "kind": "plan",
  "direction": "reverse",
  "store_dir": "/store",
  "service": "Claude Code-credentials-93e299a5",
  "account": "<account>&",
  "from": {
    "digest8": "87654321"
  },
  "to": {
    "digest8": "636f49a9"
  },
  "config_path": "/home/.claude.json"
}
`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			subject := &useSubject{storeDir: "/store", service: "Claude Code-credentials-93e299a5"}
			if err := (SessionProcess{Out: &out}).emitUsePlan(subject, &test.account, test.from, "636f49a9", test.direction, test.configPath); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, out.String()); diff != "" {
				t.Fatalf("reference JSON differs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUseBreakJSONReferenceOrder(t *testing.T) {
	tests := map[string]struct {
		report useBreakReport
		want   string
	}{
		"success: clean break keeps null reason": {
			report: useBreakReport{Broke: true, Outcome: secret.OutcomeBroken, HolderEvidence: secret.EvidenceNoStoppedClaude},
			want: `{
  "broke": true,
  "outcome": "broken",
  "reason": null,
  "holder_evidence": "no_stopped_claude"
}
`,
		},
		"success: abandoned break retains reason": {
			report: useBreakReport{Outcome: secret.OutcomeAbandoned, Reason: new(secret.ReasonHolderUnreadable), HolderEvidence: secret.EvidenceUnreadable},
			want: `{
  "broke": false,
  "outcome": "abandoned",
  "reason": "holder_unreadable",
  "holder_evidence": "unreadable"
}
`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := (SessionProcess{Out: &out}).emitUseJSON(&test.report, "could not render the break as JSON"); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, out.String()); diff != "" {
				t.Fatalf("reference JSON differs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUseConfigPlanJSONReferenceOrder(t *testing.T) {
	tests := map[string]struct {
		shown   string
		profile claude.Profile
		want    string
	}{
		"success: reference config plan": {
			shown: "~/.claude.json",
			profile: claude.Profile{
				AccountUUID:      "11111111-2222-3333-4444-555555555555",
				OrganizationUUID: "66666666-7777-8888-9999-000000000000",
			},
			want: `{
  "kind": "plan",
  "direction": "config",
  "config_path": "~/.claude.json",
  "account": {
    "account_uuid": "11111111-2222-3333-4444-555555555555",
    "organization_uuid": "66666666-7777-8888-9999-000000000000"
  }
}
`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := (SessionProcess{Out: &out}).emitUseConfigPlan(test.shown, &test.profile); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, out.String()); diff != "" {
				t.Fatalf("reference JSON differs (-want +got):\n%s", diff)
			}
		})
	}
}
