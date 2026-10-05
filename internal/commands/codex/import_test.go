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

func TestImportReadOnly(t *testing.T) {
	tests := map[string]struct {
		mode                string
		dry, sealed         bool
		poison              bool
		wantError, wantLine string
	}{
		"success: metadata only":                       {wantLine: "read-only, from"},
		"success: sealed home":                         {sealed: true, wantLine: "read-only, from"},
		"success: dry run":                             {dry: true, wantLine: "--dry-run: nothing was written"},
		"success: auto unavailable uses file":          {mode: "auto", wantLine: "auto (file in effect)"},
		"error: keyring does not open poisoned auth":   {mode: "keyring", poison: true, wantError: "nothing to import"},
		"error: ephemeral does not open poisoned auth": {mode: "ephemeral", poison: true, wantError: "nothing to import"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "source")
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile("../../../fixtures/codex/auth-codex-format.json")
			if err != nil {
				t.Fatal(err)
			}
			if tt.poison {
				data = []byte("{not JSON")
			}
			auth := filepath.Join(home, "auth.json")
			if err := os.WriteFile(auth, data, 0o600); err != nil {
				t.Fatal(err)
			}
			toml := []byte("# default\n")
			if tt.mode != "" {
				toml = []byte("cli_auth_credentials_store = \"" + tt.mode + "\"\n")
			}
			configPath := filepath.Join(home, "config.toml")
			if err := os.WriteFile(configPath, toml, 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.sealed {
				if err := os.Chmod(auth, 0o400); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(configPath, 0o400); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(home, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(home, 0o700); err != nil {
						t.Error(err)
					}
				})
			}
			before, err := os.Stat(auth)
			if err != nil {
				t.Fatal(err)
			}
			paths, err := config.Resolve(filepath.Join(root, "store"))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			command := Import{Paths: paths, Env: provider.Env{}, Reader: secret.DisabledReader{}, Out: &output}
			err = command.Run(t.Context(), cli.CodexImportOptions{From: cli.CodexImportSourceCodexHome, CodexHome: home, DryRun: tt.dry})
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error=%v, want %q", err, tt.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tt.wantLine != "" && !strings.Contains(output.String(), tt.wantLine) {
				t.Fatalf("output=%q, want %q", output.String(), tt.wantLine)
			}
			after, err := os.Stat(auth)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(auth)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(data, got); diff != "" {
				t.Fatalf("credential changed (-before +after):\n%s", diff)
			}
			if before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
				t.Fatal("source metadata changed")
			}
			if tt.dry || tt.wantError != "" {
				if _, err := os.Stat(paths.ConfigDir()); !os.IsNotExist(err) {
					t.Fatalf("dry/refused import created store: %v", err)
				}
				return
			}
			registry, err := config.LoadRegistry(t.Context(), paths)
			if err != nil {
				t.Fatal(err)
			}
			if len(registry.CodexAccounts) != 1 {
				t.Fatalf("records=%d, want 1", len(registry.CodexAccounts))
			}
			record := registry.CodexAccounts[0]
			canonical, err := filepath.EvalSymlinks(home)
			if err != nil {
				t.Fatal(err)
			}
			if record.Kind.HomeReadOnly == nil || record.Kind.HomeReadOnly.Dir != canonical || record.ChatGPTUserID != "user-0001" || record.Email == nil || *record.Email != "codex-user@example.invalid" {
				t.Fatalf("metadata=%+v", record)
			}
			for _, path := range []string{"codex/.locks", "codex/.scratch", "codex/.state"} {
				if _, err := os.Stat(filepath.Join(paths.ConfigDir(), path)); !os.IsNotExist(err) {
					t.Fatalf("import created %s", path)
				}
			}
			first, err := os.ReadFile(paths.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			meta, err := os.Stat(paths.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			output.Reset()
			if err := command.Run(t.Context(), cli.CodexImportOptions{From: cli.CodexImportSourceCodexHome, CodexHome: home}); err != nil {
				t.Fatal(err)
			}
			second, err := os.ReadFile(paths.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			next, err := os.Stat(paths.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(first, second); diff != "" {
				t.Fatalf("repeat changed registry:\n%s", diff)
			}
			if !next.ModTime().Equal(meta.ModTime()) || !strings.Contains(output.String(), "already recorded") {
				t.Fatal("repeat rewrote registry or omitted already-recorded report")
			}
		})
	}
}

func TestRecordImportOnce(t *testing.T) {
	tests := map[string]struct{ kind config.CodexKind }{
		"success: owned row preserved":    {config.CodexKindOwned("/owned", config.RefreshNever)},
		"success: live row preserved":     {config.CodexKindLive()},
		"success: readonly row preserved": {config.CodexKindHomeReadOnly("/first")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			identity := &provider.Identity{UserID: "user-0001", AccountID: "11111111-2222-4333-8444-555555555555"}
			original := config.CodexAccountRecord{ChatGPTUserID: identity.UserID, ChatGPTAccountID: identity.AccountID, Kind: tt.kind, Forgotten: true, Label: new("retain"), CreatedAt: "unchanged"}
			registry := &config.Registry{CodexAccounts: []config.CodexAccountRecord{original}}
			recordImportOnce(registry, identity, "/new")
			if diff := gocmp.Diff([]config.CodexAccountRecord{original}, registry.CodexAccounts); diff != "" {
				t.Fatalf("duplicate recheck changed row:\n%s", diff)
			}
		})
	}
}
