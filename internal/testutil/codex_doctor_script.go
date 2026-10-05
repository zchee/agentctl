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

package testutil

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/zchee/agentctl/schemas"
)

func init() { registerScriptCmd("codex-doctor-fixture", codexDoctorFixture) }

func codexDoctorFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: codex-doctor-fixture <kind>")
	}
	kind := args[0]
	root := ts.MkAbs(filepath.Join("doctor", kind))
	home, store := filepath.Join(root, "home", ".codex"), filepath.Join(root, "store")
	ts.Check(os.MkdirAll(store, 0o700))
	ts.Check(os.MkdirAll(filepath.Dir(home), 0o700))
	ts.Setenv("HOME", filepath.Dir(home))
	ts.Setenv("CODEX_HOME", "")
	ts.Setenv("AGENTCTL_CONFIG_DIR", store)
	ts.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "none")
	for _, name := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID"} {
		ts.Setenv(name, "")
	}
	write := func(path, text string) {
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, []byte(text), 0o600))
	}
	source, err := repoRoot()
	ts.Check(err)
	auth, err := os.ReadFile(filepath.Join(source, "fixtures", "codex", "auth-codex-format.json"))
	ts.Check(err)
	user, account := "user-0001", "11111111-2222-4333-8444-555555555555"
	ns := filepath.Join(store, "codex", user, account)
	audit := filepath.Join(store, "codex", "writes.jsonl")
	row := `{"chatgpt_user_id":"user-0001","chatgpt_account_id":"11111111-2222-4333-8444-555555555555","email":null,"plan_type":null,"label":null,"kind":{"kind":"owned","export_spelling":"/x","refresh":"auto"},"forgotten":false,"created_at":"2026-09-22T00:00:00Z"}`
	registry := func() {
		write(filepath.Join(store, "config.json"), `{"version":2,"accounts":[],"forgotten_services":[],"codex_accounts":[`+row+`]}`)
	}
	line := func(outcome string) string {
		if outcome == "login_keychain_gained" {
			return `{"ts":"2026-09-22T00:00:00Z","agctl_pid":1,"provider":"codex","user_id":"none","account_id":"none","outcome":"login_keychain_gained","keychain_account":"cli|00112233abcdefff"}`
		}
		return fmt.Sprintf(`{"ts":"2026-09-22T00:00:00Z","agctl_pid":1,"provider":"codex","user_id":%q,"account_id":%q,"outcome":%q}`, user, account, outcome)
	}
	listings := []struct{ service, account string }{}
	if kind != "no-home" {
		ts.Check(os.MkdirAll(home, 0o700))
	}
	switch kind {
	case "all":
		registry()
		write(filepath.Join(ns, "auth.json"), `{"auth_mode":"chatgpt"}`)
		ts.Check(os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))
		write(filepath.Join(home, "config.toml"), "cli_auth_credentials_store = 'keyring'\n")
		write(filepath.Join(home, "auth.json"), string(auth))
		ts.Check(os.Chmod(filepath.Join(home, "auth.json"), 0o644))
		write(filepath.Join(home, "multi-auth", "auth.json"), "agctl-test-codex-multiauth-0001")
		sum := sha256.Sum256([]byte(home))
		listings = append(listings, struct{ service, account string }{"Codex Auth", "cli|" + hex.EncodeToString(sum[:8])}, struct{ service, account string }{"codex-switcher:agctl-test-codex-email-0009", "example"})
		write(audit, line("applied")+"\n"+line("login_overwrite")+"\n")
		ts.Setenv("CODEX_API_KEY", "agctl-test-codex-ak-0005")
	case "shape":
		var document map[string]jsontext.Value
		ts.Check(json.Unmarshal(auth, &document))
		document["agctl-test-codex-ak-0004"] = jsontext.Value(`"value"`)
		data, err := json.Marshal(document)
		ts.Check(err)
		write(filepath.Join(home, "auth.json"), string(data))
	case "malformed":
		write(filepath.Join(home, "config.toml"), "model = 'gpt-test'\n\n[mcp_servers.x.env]\nKEY = \"agctl-test-codex-ak-0002\n")
	case "config-key":
		write(filepath.Join(home, "config.toml"), "cli_auth_credentials_store = 'file'\n\n[mcp_servers.x.env]\nKEY = 'agctl-test-codex-ak-0002'\n")
	case "orphans", "hostile-orphans":
		registry()
		ts.Check(os.MkdirAll(ns, 0o700))
		if kind == "orphans" {
			ts.Check(os.MkdirAll(filepath.Join(store, "codex", "user-orphan-0002", account), 0o700))
		} else {
			ts.Check(os.MkdirAll(filepath.Join(store, "codex", "user\x1b]0;pwned\x07", account), 0o700))
			ts.Check(os.MkdirAll(filepath.Join(store, "codex", "user-orphan-0003", "agctl-test-codex-ak-0007$(id)"), 0o700))
		}
		name := "agctl-codex-login-deadbeef"
		if kind == "hostile-orphans" {
			name = "agctl-codex-login-$(id)"
		}
		scratch := filepath.Join(store, "codex", ".scratch", name)
		ts.Check(os.MkdirAll(scratch, 0o700))
		old := time.Now().Add(-30 * time.Minute)
		ts.Check(os.Chtimes(scratch, old, old))
	case "hostile-log":
		registry()
		write(audit, "\x1b]0;pwned\x07 not a log line\n"+strings.Replace(line("applied"), user, "user\\u001b[2J", 1)+"\n"+line("login_install")+"\n")
	case "hostile-provider":
		registry()
		write(audit, strings.Replace(line("applied"), `"provider":"codex"`, `"provider":"codex\u001b]0;pwned\u0007$(id)"`, 1)+"\n"+line("login_install")+"\n")
	case "outcomes", "outcomes-gained":
		registry()
		outcomes := []string{"applied", "saved_to_pending", "discarded_external", "pending_replayed", "pending_discarded", "login_install", "login_overwrite", "delete", "adopted_external", "ambiguous"}
		if kind == "outcomes-gained" {
			outcomes = []string{"login_keychain_gained"}
		}
		lines := []string{}
		for _, outcome := range outcomes {
			lines = append(lines, line(outcome))
		}
		write(audit, strings.Join(lines, "\n")+"\n")
	case "readonly":
		registry()
		write(filepath.Join(home, "auth.json"), string(auth))
		write(filepath.Join(ns, "auth.json"), `{"auth_mode":"chatgpt"}`)
		write(filepath.Join(ns, "auth.json.pending"), `{"auth_mode":"chatgpt"}`)
		write(filepath.Join(ns, "auth.json.tmp.0badc0de"), `{}`)
		write(filepath.Join(store, "codex", ".state", user+"+"+account+".refresh"), `{"schema":1,"inflight":{"sent_digest8":"0123abcd","sent_at":"2026-09-21T00:00:00Z"},"floor_min":60,"did_not_help":1,"ambiguous_since":"2026-09-21T00:00:00Z","class":"tls","resent":false}`)
	case "unexplained":
		listings = append(listings, struct{ service, account string }{"Codex Auth", "cli|00112233abcdefff"})
	case "hostile-account":
		for _, account := range []string{"cli|00112233abcdefff", "agctl-test-codex-ak-0006", "cli|$(id)", "cli|`id`", "cli|\x1b]0;pwned\x07"} {
			listings = append(listings, struct{ service, account string }{"Codex Auth", account})
		}
		write(audit, line("login_keychain_gained")+"\n")
	case "no-home", "clean":
	default:
		ts.Fatalf("unknown doctor fixture %q", kind)
	}
	if len(listings) > 0 {
		ts.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
		var dump strings.Builder
		dump.WriteString("keychain: \"login.keychain-db\"\nversion: 512\n")
		for _, entry := range listings {
			fmt.Fprintf(&dump, "class: \"genp\"\nattributes:\n    \"acct\"<blob>=\"%s\"\n    \"svce\"<blob>=\"%s\"\n", entry.account, entry.service)
		}
		write(ts.Getenv("AGCTL_FAKE_SECURITY_DUMP"), dump.String())
	}
	before, err := codexImportManifest(store)
	ts.Check(err)
	homeBefore := []codexImportEntry(nil)
	if kind != "no-home" {
		homeBefore, err = codexImportManifest(home)
		ts.Check(err)
	}
	ts.SetCmd("codex-doctor-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: codex-doctor-check")
		}
		text := ts.ReadFile("doctor-table.txt") + "\n" + ts.ReadFile("doctor-report.json") + ts.ReadFile("doctor-table-stderr.txt") + ts.ReadFile("doctor-json-stderr.txt")
		for _, needle := range []string{"agctl-test-codex-at-", "agctl-test-codex-rt-", "agctl-test-codex-ak-", "agctl-test-codex-jwt-", "agctl-test-codex-email-", "eyJ", "Bearer ", "bearer ", "agctl-test-codex-multiauth-", "\x1b]0;", "$(id)", "`id`"} {
			if strings.Contains(text, needle) {
				ts.Fatalf("doctor printed forbidden material")
			}
		}
		schemaBytes, err := schemas.FS.ReadFile("codex-doctor.v1.json")
		ts.Check(err)
		schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
		ts.Check(err)
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		ts.Check(compiler.AddResource("codex-doctor.v1.json", schemaDoc))
		schema, err := compiler.Compile("codex-doctor.v1.json")
		ts.Check(err)
		instance, err := jsonschema.UnmarshalJSON(strings.NewReader(ts.ReadFile("doctor-report.json")))
		ts.Check(err)
		ts.Check(schema.Validate(instance))
		var report struct {
			Store struct {
				Mode       string  `json:"mode"`
				Read       string  `json:"read"`
				ConfigNote *string `json:"config_note"`
			} `json:"store"`
			Live struct {
				State    string   `json:"state"`
				AuthMode *string  `json:"auth_mode"`
				ModeBits *string  `json:"mode_bits"`
				Unknown  int      `json:"unknown_member_count"`
				Missing  []string `json:"missing_known_members"`
			} `json:"live"`
			Foreign struct {
				Multi       bool     `json:"multi_auth_present"`
				Switcher    int      `json:"switcher_items"`
				Items       int      `json:"codex_auth_items"`
				Removals    []string `json:"unexplained_removals"`
				Unexplained int      `json:"unexplained_items"`
				Unnameable  int      `json:"unnameable_items"`
			} `json:"foreign"`
			Namespaces []struct {
				Artefacts []string `json:"artefacts"`
				Marker    struct {
					State  string `json:"state"`
					Class  string `json:"class"`
					Digest string `json:"inflight_digest8"`
				} `json:"marker"`
			} `json:"namespaces"`
			Orphans []struct {
				Kind    string `json:"kind"`
				Subject string `json:"subject"`
			} `json:"orphans"`
			Unnameable int      `json:"unnameable_orphans"`
			Audit      []string `json:"audit"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile("doctor-report.json")), &report))
		expect := func(ok bool, reason string) {
			if !ok {
				ts.Fatalf("%s\n%s", reason, text)
			}
		}
		switch kind {
		case "all":
			expect(report.Store.Mode == "keyring" && report.Store.Read == "not read" && report.Live.State == "not read", "store gate")
			expect(report.Foreign.Multi && report.Foreign.Switcher == 1 && report.Foreign.Items == 1, "foreign counts")
			expect(len(report.Audit) == 2, "audit tail")
			expect(strings.Contains(text, "codex session artefacts present") && strings.Contains(text, "profiles not consulted"), "table facts")
		case "shape":
			expect(report.Live.State == "credentials" && report.Live.AuthMode != nil && *report.Live.AuthMode == "chatgpt" && report.Live.ModeBits != nil && *report.Live.ModeBits == "0600" && report.Live.Unknown == 1 && slices.Contains(report.Live.Missing, "bedrock_api_key"), "credential shape")
		case "malformed":
			expect(report.Store.ConfigNote != nil && strings.HasPrefix(*report.Store.ConfigNote, "unparseable config.toml (line "), "config line note")
		case "config-key":
			expect(report.Store.Mode == "file" && report.Store.ConfigNote == nil, "config must parse")
		case "orphans":
			kinds := []string{}
			for _, orphan := range report.Orphans {
				kinds = append(kinds, orphan.Kind)
			}
			expect(slices.Contains(kinds, "namespace without record") && slices.Contains(kinds, "record without auth.json") && slices.Contains(kinds, "stale scratch") && report.Unnameable == 0, "three orphans")
		case "hostile-orphans":
			expect(report.Unnameable == 3 && len(report.Orphans) == 1 && report.Orphans[0].Subject == user+"+"+account, "orphan names filtered")
		case "hostile-log":
			expect(len(report.Audit) == 3 && strings.Contains(report.Audit[0], "agentctl did not write") && strings.Contains(report.Audit[1], "agentctl did not write") && strings.Contains(report.Audit[2], "login_install"), "audit validation")
		case "hostile-provider":
			expect(len(report.Audit) == 2 && strings.Contains(report.Audit[0], "agentctl did not write") && strings.Contains(report.Audit[1], "login_install"), "provider validation")
		case "outcomes":
			expect(len(report.Audit) == 10, "ten-line audit tail")
			for _, outcome := range []string{"applied", "saved_to_pending", "discarded_external", "pending_replayed", "pending_discarded", "login_install", "login_overwrite", "delete", "adopted_external", "ambiguous"} {
				expect(strings.Contains(strings.Join(report.Audit, "\n"), `"outcome":"`+outcome+`"`), "missing outcome "+outcome)
			}
		case "outcomes-gained":
			expect(len(report.Audit) == 1 && strings.Contains(report.Audit[0], "login_keychain_gained"), "gained outcome is rendered")
		case "readonly":
			expect(len(report.Namespaces) == 1 && report.Namespaces[0].Marker.State == "present" && report.Namespaces[0].Marker.Class == "tls" && report.Namespaces[0].Marker.Digest == "0123abcd", "marker facts")
			expect(strings.Contains(text, "stray tmp (rotated grant?)") && strings.Contains(text, "a parked pending write") && strings.Contains(text, "re-arms one refresh send per namespace"), "read-only artifacts")
		case "unexplained":
			expect(len(report.Foreign.Removals) == 0 && report.Foreign.Unexplained == 1 && report.Foreign.Unnameable == 0 && !strings.Contains(text, "cli|00112233abcdefff") && !strings.Contains(text, "delete-generic-password"), "unexplained foreign item")
		case "hostile-account":
			expect(len(report.Foreign.Removals) == 1 && report.Foreign.Removals[0] == `security delete-generic-password -s "Codex Auth" -a "cli|00112233abcdefff"` && report.Foreign.Unnameable == 4, "foreign account filtering")
		case "no-home":
			expect(report.Live.State == "absent" && strings.Contains(text, "codex home"), "missing default home is reported")
		case "clean":
			expect(strings.Contains(text, "owned namespaces\n  none"), "empty namespace section")
			_, err := os.Stat(filepath.Join(store, "codex"))
			expect(os.IsNotExist(err), "doctor created codex tree")
		}
		after, err := codexImportManifest(store)
		ts.Check(err)
		if diff := gocmp.Diff(before, after); diff != "" {
			ts.Fatalf("doctor changed store:\n%s", diff)
		}
		if kind != "no-home" {
			after, err := codexImportManifest(home)
			ts.Check(err)
			if diff := gocmp.Diff(homeBefore, after); diff != "" {
				ts.Fatalf("doctor changed home:\n%s", diff)
			}
		}
	})
}
