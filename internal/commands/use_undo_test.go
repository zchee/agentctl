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
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/secret"
)

func TestRunUndoSelection(t *testing.T) {
	tests := map[string]struct {
		unreadable, invalidRegistry bool
		writes                      int
		outcome                     secret.KeychainWriteOutcome
		wantError                   string
	}{
		"success: missing audit log is a no-op":                   {},
		"success: failed writes are not reversible":               {writes: 3, outcome: secret.WriteFailed},
		"error: unreadable line outside a recent tail refuses":    {unreadable: true, writes: 400, outcome: secret.WriteFailed, wantError: "line 1 could not be read"},
		"error: registry is loaded even with no reversible entry": {invalidRegistry: true, wantError: "config"},
		"error: selected namespace no longer has an owned record": {writes: 1, outcome: secret.WriteApplied, wantError: "no account agentctl currently owns"},
		"error: unknown write outcome is still selected":          {writes: 1, outcome: secret.WriteUnknown, wantError: "no account agentctl currently owns"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tt.unreadable {
				if err := os.WriteFile(secret.AuditLogPath(paths), []byte("{broken\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.invalidRegistry {
				if err := os.WriteFile(paths.ConfigFile(), []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for range tt.writes {
				event := &secret.WriteEvent{Target: secret.NamespaceTarget("01234567"), FromDigest8: new("12345678"), ToDigest8: "87654321", Outcome: tt.outcome}
				if _, err := secret.AuditAppend(t.Context(), paths, secret.NewAuditEntry(event)); err != nil {
					t.Fatal(err)
				}
			}
			before, readErr := os.ReadFile(secret.AuditLogPath(paths))
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			var out, stderr bytes.Buffer
			process := SessionProcess{Out: &out, Err: &stderr}
			err := process.RunUndo(t.Context(), cli.Globals{ConfigDir: paths.ConfigDir()}, cli.ClaudeUseOptions{Undo: true, Yes: true})
			if tt.wantError != "" {
				if err == nil || errs.ExitCode(err) != 1 || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want exit 1 containing %q", err, tt.wantError)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff("there is no swap to undo: the audit log records no reversible write\n", out.String()); diff != "" {
					t.Errorf("output (-want +got): %s", diff)
				}
			}
			after, afterErr := os.ReadFile(secret.AuditLogPath(paths))
			if afterErr != nil && !os.IsNotExist(afterErr) {
				t.Fatal(afterErr)
			}
			if diff := gocmp.Diff(before, after); diff != "" {
				t.Errorf("audit changed (-want +got): %s", diff)
			}
			if os.IsNotExist(readErr) != os.IsNotExist(afterErr) {
				t.Errorf("audit presence changed: before=%v after=%v", readErr, afterErr)
			}
		})
	}
}
