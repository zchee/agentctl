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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestAccountsRemove(t *testing.T) {
	tests := map[string]struct {
		deleteSecret, yes, readOnly bool
		code                        int
		output                      string
	}{
		"success: removing only the record retains credentials":  {output: "Its stored credentials are still on disk"},
		"success: explicit deletion removes all namespace files": {deleteSecret: true, yes: true, output: "and deleted its stored credentials"},
		"error: piped input is not consent":                      {deleteSecret: true, code: 2},
		"error: read-only record is refused":                     {deleteSecret: true, yes: true, readOnly: true, code: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := seedStore(t)
			record := store.fixture.OwnedRecord(testutil.Acct, testutil.Org)
			if tt.readOnly {
				record = store.fixture.ConfigDirRecord(testutil.Acct, testutil.Org, claude.LiveService+"-11112222")
			}
			store.fixture.WriteRegistry([]any{record})
			blob := store.fixture.Blob("sk-ant-oat01-remove", "sk-ant-ort01-remove", testutil.FreshAt())
			store.fixture.WriteCredentials(testutil.Acct, testutil.Org, blob)
			ns := store.paths.NamespaceDir(testutil.Acct, testutil.Org)
			for _, file := range []string{secret.PendingFile, secret.PendingMetaFile, secret.CredentialsFile + ".tmp.12345678"} {
				if err := os.WriteFile(filepath.Join(ns, file), []byte("private"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			err := store.accounts(nil, &out).Remove(t.Context(), cli.ClaudeAccountsRemoveOptions{ID: testutil.Acct, DeleteSecret: tt.deleteSecret, Yes: tt.yes}, TerminalPrompt{Out: &out})
			if got := errs.ExitCode(err); got != tt.code {
				t.Fatalf("exit %d, want %d; err=%v", got, tt.code, err)
			}
			registry, loadErr := config.LoadRegistry(t.Context(), store.paths)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			wantRecords := 0
			if tt.code != 0 {
				wantRecords = 1
			}
			if len(registry.Accounts) != wantRecords {
				t.Fatalf("records %d, want %d", len(registry.Accounts), wantRecords)
			}
			_, statErr := os.Stat(ns)
			removed := errors.Is(statErr, os.ErrNotExist)
			if removed != (tt.deleteSecret && tt.code == 0) {
				t.Fatalf("namespace removed=%v, err=%v", removed, statErr)
			}
			if removed {
				if _, err := os.Stat(store.paths.LockPath(testutil.Acct, testutil.Org)); err != nil {
					t.Fatalf("lock must survive deletion: %v", err)
				}
			}
			if !strings.Contains(out.String(), tt.output) || strings.Contains(out.String(), "sk-ant-") {
				t.Fatalf("unexpected output: %s", out.String())
			}
		})
	}
}

func TestAccountsForget(t *testing.T) {
	tests := map[string]struct {
		service  string
		recorded bool
		code     int
	}{
		"success: unclaimed service": {service: claude.LiveService + "-11112222"},
		"success: recorded service":  {service: claude.LiveService + "-11112222", recorded: true},
		"error: live service":        {service: claude.LiveService, code: 1},
		"error: foreign service":     {service: "claude-switcher:person@example.invalid", code: 1},
		"error: unrelated service":   {service: "another service", code: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := seedStore(t)
			if tt.recorded {
				store.fixture.WriteRegistry([]any{store.fixture.ConfigDirRecord(testutil.Acct, testutil.Org, tt.service)})
			}
			var out bytes.Buffer
			command := store.accounts(nil, &out)
			if err := command.Forget(t.Context(), tt.service, true); errs.ExitCode(err) != tt.code {
				t.Fatalf("Forget=%v", err)
			}
			if tt.code != 0 {
				return
			}
			registry, err := config.LoadRegistry(t.Context(), store.paths)
			if err != nil {
				t.Fatal(err)
			}
			if tt.recorded {
				if !registry.Accounts[0].Forgotten {
					t.Fatal("record not hidden")
				}
			} else if diff := gocmp.Diff([]string{tt.service}, registry.ForgottenServices); diff != "" {
				t.Fatal(diff)
			}
			out.Reset()
			if err := command.Forget(t.Context(), tt.service, true); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "already hidden") {
				t.Fatalf("repeat output=%s", out.String())
			}
			out.Reset()
			if err := command.Forget(t.Context(), tt.service, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "reported again") {
				t.Fatalf("unforget output=%s", out.String())
			}
			registry, err = config.LoadRegistry(t.Context(), store.paths)
			if err != nil {
				t.Fatal(err)
			}
			if tt.recorded && registry.Accounts[0].Forgotten || len(registry.ForgottenServices) != 0 {
				t.Fatal("record still hidden")
			}
		})
	}
}
