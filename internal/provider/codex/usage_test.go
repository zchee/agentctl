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

package codex

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/usage"
)

func usageFixture(t *testing.T, name string) jsontext.Value {
	t.Helper()
	body, err := os.ReadFile("../../../fixtures/codex/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func usageAt(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, "2026-09-16T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestNormalizeUsageFixtures(t *testing.T) {
	tests := map[string]struct {
		file     string
		kinds    []usage.WindowKind
		plan     string
		credits  Credits
		hasLimit bool
		note     *string
	}{
		"success: weekly then both feature windows":     {file: "usage-2026-09-16.json", kinds: []usage.WindowKind{{Class: usage.WindowWeeklyAll}, {Class: usage.WindowUnknown, Name: "GPT-5.3-Codex-Spark:primary"}, {Class: usage.WindowUnknown, Name: "GPT-5.3-Codex-Spark:secondary"}}, plan: "pro", credits: Credits{Available: true, Balance: new("12.34")}, hasLimit: true},
		"success: credits absent":                       {file: "usage-credits-absent.json", kinds: []usage.WindowKind{{Class: usage.WindowWeeklyAll}, {Class: usage.WindowSession}}, plan: "plus", hasLimit: true},
		"success: unlimited credits with reached limit": {file: "usage-credits-unlimited.json", kinds: []usage.WindowKind{{Class: usage.WindowWeeklyAll}}, plan: "enterprise", credits: Credits{Available: true, Unlimited: true}, hasLimit: true, note: new("limit reached: rate_limit_reached")},
		"success: no rate limit":                        {file: "usage-no-rate-limit.json", plan: "free", credits: Credits{Available: true, Balance: new("0.00")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := usageFixture(t, tt.file)
			got, err := Normalize(body, usageAt(t), true)
			if err != nil {
				t.Fatal(err)
			}
			var kinds []usage.WindowKind
			for _, window := range got.Windows {
				kinds = append(kinds, window.Window.Kind)
				if window.Window.IsActive {
					t.Fatal("unexpected active window")
				}
			}
			if diff := gocmp.Diff(tt.kinds, kinds); diff != "" {
				t.Fatalf("window kinds (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.credits, got.Credits); diff != "" {
				t.Fatalf("credits (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.note, got.Note); diff != "" {
				t.Fatalf("note (-want +got):\n%s", diff)
			}
			if got.PlanType == nil || *got.PlanType != tt.plan || got.HasRateLimit != tt.hasLimit {
				t.Fatalf("plan/rate limit: %+v", got)
			}
			if bytes.Contains(got.Raw, []byte(`"email"`)) {
				t.Fatal("raw retained email")
			}
			without, err := Normalize(body, usageAt(t), false)
			if err != nil || without.Raw != nil {
				t.Fatalf("keepRaw=false: %v, %s", err, without.Raw)
			}
		})
	}
}

func TestUsageDurations(t *testing.T) {
	tests := map[string]struct {
		seconds string
		kind    usage.WindowKind
	}{
		"success: session":         {"18000", usage.WindowKind{Class: usage.WindowSession}},
		"success: session ceiling": {"21600", usage.WindowKind{Class: usage.WindowSession}},
		"success: past ceiling":    {"21601", usage.WindowKind{Class: usage.WindowUnknown, Name: "6h"}},
		"success: day":             {"86400", usage.WindowKind{Class: usage.WindowUnknown, Name: "24h"}},
		"success: weekly floor":    {"518400", usage.WindowKind{Class: usage.WindowWeeklyAll}},
		"success: weekly ceiling":  {"691200", usage.WindowKind{Class: usage.WindowWeeklyAll}},
		"success: monthly":         {"2592000", usage.WindowKind{Class: usage.WindowUnknown, Name: "720h"}},
		"success: short session":   {"1800", usage.WindowKind{Class: usage.WindowSession}},
		"success: zero":            {"0", usage.WindowKind{Class: usage.WindowUnknown, Name: "unknown duration"}},
		"success: negative":        {"-5", usage.WindowKind{Class: usage.WindowUnknown, Name: "unknown duration"}},
		"success: string":          {`"604800"`, usage.WindowKind{Class: usage.WindowUnknown, Name: "unknown duration"}},
		"success: null":            {"null", usage.WindowKind{Class: usage.WindowUnknown, Name: "unknown duration"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := jsontext.Value(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":%s}}}`, tt.seconds))
			got, err := Normalize(body, usageAt(t), false)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.kind, got.Windows[0].Window.Kind); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUsagePercentAndReset(t *testing.T) {
	body := jsontext.Value(`{"rate_limit":{"primary_window":{"used_percent":140.5,"limit_window_seconds":18000,"reset_after_seconds":60},"secondary_window":{"used_percent":-3,"limit_window_seconds":604800,"reset_after_seconds":9223372036854775807}}}`)
	got, err := Normalize(body, usageAt(t), false)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Windows[0].Window.Percent != 100 || !got.Windows[0].Window.ResetsAt.Equal(usageAt(t).Add(time.Minute)) {
		t.Fatalf("primary: %+v", got.Windows[0])
	}
	if *got.Windows[1].Window.Percent != 0 || !got.Windows[1].Window.ResetsAt.IsZero() {
		t.Fatalf("secondary: %+v", got.Windows[1])
	}
}

func TestUsageLabels(t *testing.T) {
	tests := map[string]struct {
		value string
		want  *string
	}{
		"success: plain":                    {`"Model 7.1-mini"`, new("Model 7.1-mini")},
		"success: colon refused":            {`"a:b"`, new("<unrecognised>")},
		"success: terminal control refused": {`"x\r\ny"`, new("<unrecognised>")},
		"success: padded refused":           {`" padded"`, new("<unrecognised>")},
		"success: too long refused":         {`"` + strings.Repeat("n", 65) + `"`, new("<unrecognised>")},
		"success: absent":                   {`null`, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, usageLabel(jsontext.Value(tt.value))); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUsageBalanceSpelling(t *testing.T) {
	tests := map[string]struct {
		value string
		want  *string
	}{
		"success: decimals":                {`"12.34"`, new("12.34")},
		"success: trailing zero":           {`"0.10"`, new("0.10")},
		"success: negative":                {`"-1.5"`, new("-1.5")},
		"success: whole":                   {`"100"`, new("100")},
		"success: numeric fraction absent": {`7.25`, nil},
		"success: numeric whole absent":    {`3`, nil},
		"success: exponent absent":         {`"1e3"`, nil},
		"success: incomplete absent":       {`"12."`, nil},
		"success: leading decimal absent":  {`".5"`, nil},
		"success: empty absent":            {`""`, nil},
		"success: NaN absent":              {`"NaN"`, nil},
		"success: bool absent":             {`true`, nil},
		"success: null absent":             {`null`, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Normalize(jsontext.Value(`{"credits":{"balance":`+tt.value+`}}`), usageAt(t), false)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, got.Credits.Balance); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUsageNotesAndInvalidBodies(t *testing.T) {
	tests := map[string]struct {
		body    string
		note    *string
		plan    *string
		wantErr bool
	}{
		"success: specific note":   {body: `{"rate_limit":{"allowed":false,"limit_reached":true},"rate_limit_reached_type":{"type":"workspace_owner_credits_depleted"}}`, note: new("limit reached: workspace_owner_credits_depleted")},
		"success: limit reached":   {body: `{"rate_limit":{"allowed":false,"limit_reached":true}}`, note: new("limit reached")},
		"success: not allowed":     {body: `{"rate_limit":{"allowed":false}}`, note: new("requests are not currently allowed")},
		"success: allowed":         {body: `{"rate_limit":{"allowed":true}}`},
		"success: unknown plan":    {body: `{"plan_type":"ultra\u0007"}`, plan: new("unknown")},
		"success: missing plan":    {body: `{}`},
		"error: array":             {body: `[]`, wantErr: true},
		"error: string":            {body: `"usage"`, wantErr: true},
		"error: null":              {body: `null`, wantErr: true},
		"error: malformed":         {body: `{"email":"private"`, wantErr: true},
		"error: trailing document": {body: `{} {}`, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Normalize(jsontext.Value(tt.body), usageAt(t), true)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v wantErr=%t", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if diff := gocmp.Diff(tt.note, got.Note); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(tt.plan, got.PlanType); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUsageEmailRemovedAndDebugSummarized(t *testing.T) {
	got, err := Normalize(usageFixture(t, "usage-sentinel-email.json"), usageAt(t), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(got.Raw), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)} {
		if strings.Contains(text, "agctl-test-codex-email-0001") {
			t.Fatalf("email leaked: %s", text)
		}
	}
	var original, kept map[string]any
	if err := json.Unmarshal(usageFixture(t, "usage-2026-09-16.json"), &original); err != nil {
		t.Fatal(err)
	}
	normal, err := Normalize(usageFixture(t, "usage-2026-09-16.json"), usageAt(t), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(normal.Raw, &kept); err != nil {
		t.Fatal(err)
	}
	delete(original, "email")
	if diff := gocmp.Diff(original, kept); diff != "" {
		t.Fatal(diff)
	}
}
