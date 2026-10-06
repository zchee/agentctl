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
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
)

func init() { registerScriptCmd("codex-invariance-fixture", codexInvarianceFixture) }

func codexInvarianceFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 0 {
		ts.Fatalf("usage: codex-invariance-fixture")
	}
	config := ts.Getenv("AGENTCTL_CONFIG_DIR")
	namespace := filepath.Join(config, "claude", Acct, Org)
	ts.Check(os.MkdirAll(namespace, 0o700))
	ts.Check(os.WriteFile(filepath.Join(namespace, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-owned","refreshToken":"sk-ant-ort01-owned","expiresAt":0,"scopes":["user:inference","user:profile"],"subscriptionType":"max","tokenAccount":{"uuid":"11111111-2222-3333-4444-555555555555","emailAddress":"owner@example.com","organizationUuid":"66666666-7777-8888-9999-000000000000","organizationName":"Acme"}}}`), 0o600))
	claude := map[string]any{"account_uuid": Acct, "organization_uuid": Org, "email": Email, "org_name": "Acme", "kind": map[string]any{"kind": "owned", "export_spelling": namespace, "export_sha8": Sha8(namespace)}, "forgotten": false, "created_at": "2026-09-08T00:00:00Z"}
	ts.SetCmd("codex-invariance-rows", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: codex-invariance-rows <none|two>")
		}
		document := map[string]any{"version": 1, "accounts": []any{claude}, "forgotten_services": []string{}}
		switch args[0] {
		case "none":
		case "two":
			document["version"] = 2
			document["codex_accounts"] = []any{
				map[string]any{"chatgpt_user_id": "user-01", "chatgpt_account_id": "acct-01", "email": Email, "plan_type": "plus", "kind": map[string]any{"kind": "live"}, "forgotten": false, "created_at": "2026-09-17T00:00:00Z"},
				map[string]any{"chatgpt_user_id": "user-02", "chatgpt_account_id": "acct-02", "email": "second@example.com", "plan_type": "plus", "kind": map[string]any{"kind": "owned", "export_spelling": filepath.Join(config, "codex", "user-02", "acct-02"), "refresh": "auto"}, "forgotten": false, "created_at": "2026-09-17T00:00:00Z"},
			}
		default:
			ts.Fatalf("unknown row set %q", args[0])
		}
		body, err := json.Marshal(document)
		ts.Check(err)
		ts.Check(os.WriteFile(filepath.Join(config, "config.json"), body, 0o600))
	})
	ts.SetCmd("codex-invariance-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: codex-invariance-check <before> <during> <after>")
		}
		for _, name := range []string{"status-json", "status-table", "list", "show", "doctor", "undo"} {
			for _, stream := range []string{"code", "out", "err"} {
				read := func(prefix string) string {
					text := ts.ReadFile(prefix + "-" + name + "." + stream)
					if strings.Contains(text, "unaudited") {
						ts.Fatalf("%s %s dropped an audit receipt", name, prefix)
					}
					return normalizeCodexTranscript(text)
				}
				before := read(args[0])
				for _, prefix := range args[1:] {
					if diff := gocmp.Diff(before, read(prefix)); diff != "" {
						ts.Fatalf("%s %s changed between %s and %s (-before +after):\n%s", name, stream, args[0], prefix, diff)
					}
				}
			}
		}
	})
}

var codexTranscriptPID = regexp.MustCompile(`pid [0-9]`)

func normalizeCodexTranscript(text string) string {
	if text == "" {
		return ""
	}
	var result strings.Builder
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `"generated_at":`) {
			line = `  "generated_at": <per-run>`
		} else if match := codexTranscriptPID.FindStringIndex(line); match != nil {
			line = line[:match[0]] + "<per-run>"
		}
		result.WriteString(line)
		result.WriteByte('\n')
	}
	return result.String()
}
