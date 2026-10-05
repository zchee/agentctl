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
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func TestUseCatchUpOrdering(t *testing.T) {
	tests := map[string]struct {
		document   string
		profile    bool
		yes        bool
		refusedLog bool
		outcome    secret.ConfigOutcome
		reason     secret.ConfigReason
		audits     int
		plan       bool
	}{
		"success: confirmed config-only rewrite":        {document: "{\n  \"other\": 1\n}", profile: true, yes: true, outcome: secret.ConfigApplied, audits: 1, plan: true},
		"success: already current avoids prompt":        {document: `{"oauthAccount":{"accountUuid":"account","organizationUuid":"org"}}`, profile: true, outcome: secret.ConfigSkipped, reason: secret.ConfigReasonAlreadyCurrent, audits: 1},
		"success: missing configuration emits note":     {profile: true, yes: true, outcome: secret.ConfigSkipped, reason: secret.ConfigReasonAbsent, audits: 1},
		"error: missing profile opens no log":           {document: `{}`, outcome: secret.ConfigNotAttempted, reason: secret.ConfigReasonProfileUnavailable},
		"error: refused log precedes config validation": {document: `broken`, profile: true, yes: true, refusedLog: true, outcome: secret.ConfigRefused, reason: secret.ConfigReasonAuditRefused},
		"error: malformed configuration never asks":     {document: `broken`, profile: true, outcome: secret.ConfigRefused, reason: secret.ConfigReasonUnparseable, audits: 1},
		"error: json does not authorize a rewrite":      {document: "{\n  \"other\": 1\n}", profile: true, outcome: secret.ConfigNotAttempted, reason: secret.ConfigReasonDeclined, plan: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := claude.EnvWithHome(home)
			paths := config.NewPaths(filepath.Join(home, "registry"))
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(home, ".claude"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := claude.GlobalConfigPath(&env)
			if test.document != "" {
				if err := os.WriteFile(path, []byte(test.document), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.refusedLog {
				if err := os.WriteFile(secret.AuditLogPath(paths), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var profile *claude.Profile
			if test.profile {
				var err error
				profile, err = claude.ParseProfile([]byte(`{"account":{"uuid":"account","email":"owner@example.invalid"},"organization":{"uuid":"org"}}`))
				if err != nil {
					t.Fatal(err)
				}
			}
			var out, diagnostic bytes.Buffer
			process := SessionProcess{Out: &out, Err: &diagnostic}
			report := &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapAlreadyActive}, note: new("already active")}
			process.catchUpUse(t.Context(), useCatchUp{paths: paths, env: &env, record: &config.AccountRecord{AccountUUID: "account", OrganizationUUID: "org"}, profile: profile, direction: secret.DirectionForward, opts: cli.ClaudeUseOptions{JSON: true, Yes: test.yes}}, report)
			if report.config == nil {
				t.Fatal("missing configuration report")
			}
			if diff := gocmp.Diff(test.outcome, report.config.Outcome); diff != "" {
				t.Fatal(diff)
			}
			var wantReason *secret.ConfigReason
			if test.reason != "" {
				wantReason = new(test.reason)
			}
			if diff := gocmp.Diff(wantReason, report.config.Reason); diff != "" {
				t.Fatal(diff)
			}
			if report.outcome.Kind != claude.SwapAlreadyActive {
				t.Fatal("catch-up changed credential outcome")
			}
			if strings.Contains(out.String(), `"direction": "config"`) != test.plan {
				t.Fatalf("unexpected plan: %s", out.String())
			}
			if test.outcome == secret.ConfigApplied {
				if !strings.Contains(*report.note, "now names that account too") {
					t.Fatalf("missing completion clause: %s", *report.note)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), `"accountUuid": "account"`) || !strings.Contains(string(data), `"other": 1`) {
					t.Fatalf("incorrect config: %s", data)
				}
			} else if test.document != "" {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(test.document, string(data)); diff != "" {
					t.Fatal(diff)
				}
			}
			if test.refusedLog {
				return
			}
			tail, err := secret.TailAuditLog(paths, 64)
			if err != nil {
				t.Fatal(err)
			}
			if len(tail.Entries) != test.audits {
				t.Fatalf("audit entries=%d, want %d", len(tail.Entries), test.audits)
			}
			for _, entry := range tail.Entries {
				event, ok := entry.Event.(*secret.ConfigWriteRecord)
				if !ok || event.After != nil {
					t.Fatalf("catch-up audit must have no credential-write predecessor: %T %+v", entry.Event, entry.Event)
				}
			}
		})
	}
}

func TestUseConfigStepOnlyFollowsLiveWrite(t *testing.T) {
	tests := map[string]struct {
		live    bool
		outcome claude.SwapOutcomeKind
		reason  secret.ConfigReason
	}{
		"success: namespace applied has no step":              {outcome: claude.SwapApplied},
		"success: live failed has no step":                    {live: true, outcome: claude.SwapFailed},
		"success: live busy has no step":                      {live: true, outcome: claude.SwapBusy},
		"success: live already active uses separate catch-up": {live: true, outcome: claude.SwapAlreadyActive},
		"error: applied without profile":                      {live: true, outcome: claude.SwapApplied, reason: secret.ConfigReasonProfileUnavailable},
		"error: unknown write never rewrites config":          {live: true, outcome: claude.SwapUnknown, reason: secret.ConfigReasonSwapUnknown},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var env *claude.EnvView
			if test.live {
				env = new(claude.EnvWithHome(t.TempDir()))
			}
			report := useConfigStep(t.Context(), claude.SwapOutcome{Kind: test.outcome}, env, nil)
			if test.reason == "" {
				if report != nil {
					t.Fatalf("unexpected config step: %+v", report)
				}
				return
			}
			if report == nil || report.Reason == nil || *report.Reason != test.reason || report.Outcome != secret.ConfigNotAttempted {
				t.Fatalf("config=%+v; want not-attempted %s", report, test.reason)
			}
		})
	}
}
