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
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	provider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/secret"
)

func TestDoctorReadOnly(t *testing.T) {
	tests := map[string]struct {
		mode, auth, state string
		missing, explicit bool
	}{
		"success: absent default home":                       {missing: true, state: "absent"},
		"success: explicit missing home is a report":         {missing: true, explicit: true, state: "no home"},
		"success: keyring gate ignores malformed credential": {mode: "keyring", auth: "{not JSON", state: "not read"},
		"success: malformed file is unusable":                {auth: "{not JSON", state: "unusable"},
		"success: absent auth is reported":                   {state: "absent"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home", ".codex")
			store := filepath.Join(root, "missing-store")
			if !tt.missing {
				if err := os.MkdirAll(home, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.mode != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("cli_auth_credentials_store = '"+tt.mode+"'\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var before os.FileInfo
			if tt.auth != "" {
				if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(tt.auth), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.Stat(filepath.Join(home, "auth.json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			paths, err := config.Resolve(store)
			if err != nil {
				t.Fatal(err)
			}
			env := provider.Env{Home: filepath.Dir(home)}
			if tt.explicit {
				env.CodexHome = home
			}
			var out bytes.Buffer
			command := Doctor{Paths: paths, Env: env, Reader: secret.DisabledReader{}, Present: []string{"CODEX_API_KEY"}, Out: &out}
			if err := command.Run(t.Context(), cli.CodexDoctorOptions{JSON: true}); err != nil {
				t.Fatal(err)
			}
			var report doctorReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.state, report.Live.State); diff != "" {
				t.Fatalf("live state:\n%s", diff)
			}
			if diff := gocmp.Diff([]doctorEnv{{Name: "CODEX_API_KEY", Present: true}, {Name: "CODEX_ACCESS_TOKEN"}, {Name: "CODEX_REFRESH_TOKEN_URL_OVERRIDE"}, {Name: "CODEX_APP_SERVER_LOGIN_CLIENT_ID"}}, report.Environment); diff != "" {
				t.Fatalf("presence only:\n%s", diff)
			}
			if _, err := os.Stat(store); !os.IsNotExist(err) {
				t.Fatalf("doctor created a store: %v", err)
			}
			if before != nil {
				after, err := os.Stat(filepath.Join(home, "auth.json"))
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(home, "auth.json"))
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(tt.auth, string(data)); diff != "" {
					t.Fatalf("auth bytes changed:\n%s", diff)
				}
				if !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
					t.Fatal("auth metadata changed")
				}
			}
			out.Reset()
			if err := command.Run(t.Context(), cli.CodexDoctorOptions{}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "owned namespaces\n  none") {
				t.Fatalf("table missing empty namespaces:\n%s", out.String())
			}
		})
	}
}

func TestDoctorForeignAttribution(t *testing.T) {
	tests := map[string]struct {
		account                           string
		caused                            []string
		unexplained, unnameable, commands int
	}{
		"success: unexplained item is counted only":     {account: "cli|00112233abcdefff", unexplained: 1},
		"success: attributed item offers one command":   {account: "cli|00112233abcdefff", caused: []string{"cli|00112233abcdefff"}, commands: 1},
		"success: attributed malformed name is counted": {account: "cli|$(id)", caused: []string{"cli|$(id)"}, unnameable: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			found := doctorForeignSection(nil, doctorListings{codexAuth: []secret.ServiceEntry{{Service: provider.KeyringService, Account: tt.account}}}, tt.caused)
			if diff := gocmp.Diff([]int{tt.unexplained, tt.unnameable, tt.commands}, []int{found.UnexplainedItems, found.UnnameableItems, len(found.UnexplainedRemovals)}); diff != "" {
				t.Fatalf("foreign attribution:\n%s", diff)
			}
			data, err := json.Marshal(found)
			if err != nil {
				t.Fatal(err)
			}
			if tt.commands == 0 && strings.Contains(string(data), tt.account) {
				t.Fatal("an unclaimed or unnameable account was printed")
			}
		})
	}
}
