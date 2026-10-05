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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
)

func TestAccountsRemove(t *testing.T) {
	tests := map[string]struct {
		orphan, foreign bool
		wantError       string
	}{
		"success: missing credential without audit":                 {},
		"success: orphan marker removed and audited":                {orphan: true},
		"error: foreign namespace preserves all files and registry": {orphan: true, foreign: true, wantError: "nothing was removed"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, err := config.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			namespace, err := paths.CodexNamespaceDir("user-a", "account-a")
			if err != nil {
				t.Fatal(err)
			}
			row := config.CodexAccountRecord{ChatGPTUserID: "user-a", ChatGPTAccountID: "account-a", Kind: config.CodexKindOwned(namespace, config.RefreshAuto), CreatedAt: "2026-09-17T00:00:00Z"}
			if err := config.UpdateRegistry(t.Context(), paths, func(registry *config.Registry) { registry.CodexAccounts = []config.CodexAccountRecord{row} }); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(paths.CodexRoot(), ".state", "user-a+account-a.refresh")
			if test.orphan {
				if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.foreign {
				if err := os.MkdirAll(filepath.Join(namespace, "sessions"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(namespace, "auth.json"), []byte(`{"OPENAI_API_KEY":"agentctl-test-codex-ak-0001"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			accounts := Accounts{Paths: paths, Out: &out}
			err = accounts.Remove(t.Context(), cli.CodexAccountsRemoveOptions{ID: "user-a/account-a", DeleteSecret: true, Yes: true}, commands.TerminalPrompt{Out: &out})
			registry, loadErr := config.LoadRegistry(t.Context(), paths)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("removal=%v, want %q", err, test.wantError)
				}
				if diff := gocmp.Diff([]config.CodexAccountRecord{row}, registry.CodexAccounts); diff != "" {
					t.Fatalf("refused registry (-want +got):\n%s", diff)
				}
				for _, path := range []string{marker, filepath.Join(namespace, "auth.json"), filepath.Join(namespace, "sessions")} {
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("refused removal touched %s: %v", path, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(registry.CodexAccounts) != 0 {
				t.Fatalf("row remains after removal: %+v", registry.CodexAccounts)
			}
			for _, path := range []string{namespace, marker} {
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("missing namespace/marker was retained or created at %s: %v", path, err)
				}
			}
			audit, err := os.ReadFile(filepath.Join(paths.CodexRoot(), "writes.jsonl"))
			if test.orphan {
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(audit), "\n") != 1 || !strings.Contains(string(audit), `"delete"`) {
					t.Fatalf("orphan audit=%s", audit)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("nothing-to-delete produced audit=%s err=%v", audit, err)
			}
		})
	}
}
