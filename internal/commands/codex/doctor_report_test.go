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
	json "encoding/json/v2"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/zchee/agentctl/schemas"
)

func fullDoctorReport() doctorReport {
	return doctorReport{
		Version:           1,
		Home:              doctorHome{Path: new("/tmp/codex-home"), SymlinkChain: []string{"/tmp/codex-home -> /elsewhere"}},
		Store:             doctorStore{Mode: "auto", Read: "auto (file in effect)", CoarseMatch: true, BaseURL: new("https://example.invalid"), ConfigNote: new("unparseable config.toml (line 3)")},
		Environment:       []doctorEnv{{Name: "CODEX_API_KEY", Present: true}},
		Live:              doctorLive{State: "credentials", AuthMode: new("chatgpt"), ModeBits: new("0600"), ModeWarning: new("warning: mode"), Size: new(uint64(1024)), AccessExpiry: new("in 9m"), LastRefresh: new("expired 2h ago"), MatchesNamespace: new("user-0001+acct-0001"), Daemon: "a daemon is running", MissingKnownMembers: []string{"bedrock_api_key"}, UnknownMemberCount: 2},
		Foreign:           doctorForeign{MultiAuthPresent: true, SwitcherItems: 3, CodexAuthItems: 1, UnexplainedRemovals: []string{`security delete-generic-password -s "Codex Auth" -a "cli|00112233abcdefff"`}, UnexplainedItems: 4, UnnameableItems: 2},
		Namespaces:        []doctorNamespace{{User: "user-0001", Acct: "acct-0001", Path: "/tmp/store/codex/user-0001/acct-0001", CredentialsPresent: true, RefreshPolicy: "auto", Lock: &doctorLock{PID: 4242, AcquiredAt: "2026-09-22T00:00:00Z", Holder: "held"}, Artefacts: []string{"stray tmp (rotated grant?): 1 file(s)"}, Marker: doctorMarker{State: "present", InflightDigest8: new("0123abcd"), InflightAge: new("2h"), Class: new("tls"), FloorMin: new(uint32(60)), DidNotHelp: new(uint8(1)), Resent: new(false), AmbiguousSince: new("2h"), ResendEligible: new(true)}, Notes: []string{"a note"}}},
		Orphans:           []doctorOrphan{{Kind: "stale scratch", Subject: "agctl-codex-login-0000abcd", Age: new("22m")}},
		UnnameableOrphans: 1, Audit: []string{`{"provider":"codex"}`}, Notes: []string{"a report-wide note"},
	}
}

func TestDoctorReportSchemaAndOrder(t *testing.T) {
	schemaBytes, err := schemas.FS.ReadFile("codex-doctor.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("codex-doctor.v1.json", schemaDoc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("codex-doctor.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	unavailable := fullDoctorReport()
	unavailable.Namespaces[0].Marker = doctorMarker{State: "unavailable", Unavailable: new("`/tmp/store/codex/.state/x.refresh` (permission denied)")}
	tests := map[string]struct{ report doctorReport }{
		"success: every optional field": {fullDoctorReport()},
		"success: nothing found":        {doctorReport{Version: 1, Home: doctorHome{Error: new("no home")}, Store: doctorStore{Mode: "file", Read: "not read"}, Live: doctorLive{State: "absent", Daemon: "none"}}},
		"success: unavailable marker":   {unavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(tt.report)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("schema: %v\n%s", err, data)
			}
			text := string(data)
			previous := -1
			for _, key := range []string{"version", "home", "store", "environment", "live", "foreign", "namespaces", "orphans", "unnameable_orphans", "audit", "notes"} {
				// Search from the previous top-level field to avoid nested notes.
				index := strings.Index(text[previous+1:], `"`+key+`":`)
				if index < 0 {
					t.Fatalf("missing ordered field %s", key)
				}
				previous += index + 1
			}
			if !strings.HasPrefix(text, `{"version":1,`) {
				t.Fatal("report version or first member changed")
			}
		})
	}
}

func TestDoctorReportRendering(t *testing.T) {
	full := fullDoctorReport()
	unavailable := fullDoctorReport()
	unavailable.Namespaces[0].Marker = doctorMarker{State: "unavailable", Unavailable: new("`/tmp/store/codex/.state/x.refresh` (permission denied)")}
	tests := map[string]struct {
		report           doctorReport
		contains, absent []string
	}{
		"success: all facts rendered": {report: full, contains: []string{
			"codex home", "/tmp/codex-home -> /elsewhere", "mode auto (auto (file in effect))", "coarse match", "profiles not consulted", "chatgpt_base_url https://example.invalid", "unparseable config.toml (line 3)", "CODEX_API_KEY present", "auth_mode chatgpt", "mode 0600, 1024 bytes", "daemon evidence: a daemon is running", "fields absent: bedrock_api_key", "2 field(s) this build does not know", "`multi-auth/` present", "codex-switcher keychain items: 3", "`Codex Auth` keychain items: 1", "2 further `Codex Auth` item(s) are listed under an account agentctl would not have written", "stray tmp (rotated grant?): 1 file(s)", "send outstanding for 0123abcd, 2h ago", "class tls", "ambiguous refresh outstanding since 2h", "--resend eligible", "stale scratch (agctl-codex-login-0000abcd, 22m)", doctorStateWarning,
		}},
		"success: clean machine":                   {report: doctorReport{Version: 1, Home: doctorHome{Error: new("no home")}, Store: doctorStore{Mode: "file", Read: "not read"}, Live: doctorLive{State: "absent", Daemon: "none"}}, contains: []string{"owned namespaces\n  none", "left behind\n  nothing", "no Codex write has been recorded"}, absent: []string{doctorStateWarning}},
		"success: unavailable marker names reason": {report: unavailable, contains: []string{"refresh state unavailable: `/tmp/store/codex/.state/x.refresh` (permission denied)"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			text := renderDoctor(tt.report)
			for _, want := range tt.contains {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q:\n%s", want, text)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(text, absent) {
					t.Fatalf("unexpected %q:\n%s", absent, text)
				}
			}
		})
	}
}
