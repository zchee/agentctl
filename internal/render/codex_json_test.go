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

package render_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestCodexUsageJSONGolden(t *testing.T) {
	body, err := os.ReadFile("../../fixtures/codex/usage-2026-09-16.json")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, "2026-09-16T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := codex.Normalize(body, at, true)
	if err != nil {
		t.Fatal(err)
	}
	fragment := struct {
		Windows []render.JSONWindowV2 `json:"windows"`
		Credits render.JSONCreditsV2  `json:"credits"`
	}{render.CodexWindowsV2(snapshot), render.CodexCreditsV2(snapshot.Credits)}
	rendered, err := json.Marshal(&fragment, jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	golden := testutil.ReadGolden(t, "provider__codex__usage__capture_json_v2")
	if diff := gocmp.Diff(strings.TrimSuffix(string(golden), "\n"), string(rendered)); diff != "" {
		t.Fatalf("usage JSON (-want +got):\n%s", diff)
	}
}

func TestCodexJSONPercentSpelling(t *testing.T) {
	tests := map[string]struct {
		value render.JSONPercentV2
		want  string
	}{
		"success: integral":       {9, "9.0"},
		"success: zero":           {0, "0.0"},
		"success: fraction":       {33.333333333333336, "33.333333333333336"},
		"success: clamped":        {100, "100.0"},
		"success: tiny":           {0.00000001, "1e-8"},
		"success: fixed boundary": {0.00001, "0.00001"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, string(got)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestCodexStatusJSONSchemaAndMemberOrder(t *testing.T) {
	tests := map[string]struct{ raw bool }{"success: plain": {}, "success: raw": {raw: true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, "2026-09-16T12:00:00Z")
			if err != nil {
				t.Fatal(err)
			}
			report := render.NewStatusReportV2(at, 0)
			report.Rows = append(report.Rows, render.JSONRowV2{Provider: "codex", ID: "user+account", Identity: render.JSONIdentityV2{UserID: "user", AccountID: "account"}, Kind: "owned", State: "ok", StateLabel: "OK", LockState: "idle", Windows: render.CodexWindowsV2(nil), Credits: render.UnavailableCreditsV2()})
			if tt.raw {
				report.Raw = &render.RawBodies{}
				report.Raw.Add("user+account", jsontext.Value(`{"rate_limit":{}}`))
			}
			got, err := render.MarshalStatusReportV2(&report)
			if err != nil {
				t.Fatal(err)
			}
			testutil.ValidateSchema(t, "status.v2.json", got)
			fields := []string{`"version"`, `"generated_at"`, `"rows"`, `"hidden"`}
			previous := -1
			for _, field := range fields {
				next := strings.Index(string(got), field)
				if next <= previous {
					t.Fatalf("member order: %s", got)
				}
				previous = next
			}
			if strings.Contains(string(got), `"raw"`) != tt.raw {
				t.Fatalf("raw presence: %s", got)
			}
			if strings.HasSuffix(string(got), "\n") {
				t.Fatal("marshal unexpectedly adds newline")
			}
		})
	}
}
