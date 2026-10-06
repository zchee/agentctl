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
	"time"

	"github.com/rogpeppe/go-internal/testscript"
)

func init() {
	registerScriptCmd("codex-refresh-marker", codexRefreshMarker)
	registerScriptCmd("codex-refresh-policy", codexRefreshPolicy)
}

func codexRefreshMarker(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: codex-refresh-marker <unknown|spent|terminal>")
	}
	state := map[string]any{"schema": 1, "floor_min": 60, "did_not_help": 0}
	switch args[0] {
	case "unknown", "spent":
		since := time.Now().Add(-61 * time.Minute).UTC().Format(time.RFC3339Nano)
		state["inflight"] = map[string]any{"sent_digest8": "0123abcd", "sent_at": since}
		state["last_sent_at"], state["ambiguous_since"], state["class"], state["resent"] = since, since, "ambiguous", args[0] == "spent"
	case "terminal":
		state["floor_min"], state["did_not_help"] = 240, 3
	default:
		ts.Fatalf("unknown refresh marker fixture %q", args[0])
	}
	path := ts.Getenv("C_MARKER")
	if path == "" {
		ts.Fatalf("codex-status-fixture must run before codex-refresh-marker")
	}
	ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
	body, err := json.Marshal(state)
	ts.Check(err)
	ts.Check(os.WriteFile(path, body, 0o600))
}

func codexRefreshPolicy(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: codex-refresh-policy <auto|never>")
	}
	var registry struct {
		Accounts []struct {
			Kind struct {
				Refresh string `json:"refresh"`
			} `json:"kind"`
		} `json:"codex_accounts"`
	}
	ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(ts.Getenv("C_CONFIG"), "config.json"))), &registry))
	if len(registry.Accounts) != 1 || registry.Accounts[0].Kind.Refresh != args[0] {
		ts.Fatalf("registry refresh policy differs from %q", args[0])
	}
}
