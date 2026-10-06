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
	"io"
	"os"
	"strings"
	"testing"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	provider "github.com/zchee/agentctl/internal/provider/codex"
)

func TestLoginConfirmation(t *testing.T) {
	tests := map[string]struct {
		answer                 string
		terminal, brokenOutput bool
		want                   string
	}{
		"success: yes authorizes replacement":                        {answer: " YES \n", terminal: true},
		"success: undeliverable question does not strand credential": {answer: "y\n", terminal: true, brokenOutput: true},
		"error: no discards the new grant":                           {answer: "no\n", terminal: true, want: "the new grant was discarded"},
		"error: nonterminal has no yes escape":                       {want: "accounts remove user-0001/account-0001"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = master.Close() }()
			defer func() { _ = slave.Close() }()
			var output bytes.Buffer
			var out io.Writer = &output
			if tt.brokenOutput {
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				_ = reader.Close()
				defer func() { _ = writer.Close() }()
				out = writer
			}
			if tt.terminal {
				if _, err := io.WriteString(master, tt.answer); err != nil {
					t.Fatal(err)
				}
			}
			prompt := LoginTerminal{In: slave, Out: out}
			err = loginConfirmOverwrite(t.Context(), "user-0001/account-0001", tt.terminal, prompt)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("confirmation error = %v, want %s", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "--yes") {
				t.Fatalf("login advertised unsupported flag: %v", err)
			}
		})
	}
}

func TestLoginAlreadyOwnedUsesOnlyRegistry(t *testing.T) {
	tests := map[string]struct {
		kind      config.CodexKind
		forgotten bool
		want      string
	}{
		"success: owned row asks":                    {kind: config.CodexKindOwned("/unused", config.RefreshAuto), want: "user-0001/account-0001"},
		"success: forgotten owned row still asks":    {kind: config.CodexKindOwned("/unused", config.RefreshAuto), forgotten: true, want: "user-0001/account-0001"},
		"success: read-only row is not an overwrite": {kind: config.CodexKindHomeReadOnly("/unused")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, err := config.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := config.UpdateRegistry(t.Context(), paths, func(r *config.Registry) {
				r.CodexAccounts = []config.CodexAccountRecord{{ChatGPTUserID: "user-0001", ChatGPTAccountID: "account-0001", Kind: tt.kind, Forgotten: tt.forgotten, CreatedAt: "2026-09-22T00:00:00Z"}}
			}); err != nil {
				t.Fatal(err)
			}
			got, err := loginAlreadyOwned(t.Context(), paths, "user-0001", "account-0001")
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestLoginRecordPreservesUnspecifiedLabelAndCreationTime(t *testing.T) {
	tests := map[string]struct {
		label       string
		noRefresh   bool
		wantLabel   string
		wantRefresh config.RefreshPolicy
	}{
		"success: absent label is retained":            {wantLabel: "existing", wantRefresh: config.RefreshAuto},
		"success: explicit label replaces prior label": {label: "replacement", noRefresh: true, wantLabel: "replacement", wantRefresh: config.RefreshNever},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, err := config.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			before := config.CodexAccountRecord{ChatGPTUserID: "user-0001", ChatGPTAccountID: "account-0001", Label: new("existing"), Kind: config.CodexKindHomeReadOnly("/unused"), Forgotten: true, CreatedAt: "2026-09-22T00:00:00Z"}
			if err := config.UpdateRegistry(t.Context(), paths, func(r *config.Registry) { r.CodexAccounts = []config.CodexAccountRecord{before} }); err != nil {
				t.Fatal(err)
			}
			command := Login{Paths: paths}
			identity := provider.Identity{UserID: before.ChatGPTUserID, AccountID: before.ChatGPTAccountID, Email: new("changed@example.invalid"), Plan: new("team")}
			if err := command.record(t.Context(), identity, cli.CodexLoginOptions{Label: tt.label, NoRefresh: tt.noRefresh}); err != nil {
				t.Fatal(err)
			}
			registry, err := config.LoadRegistry(t.Context(), paths)
			if err != nil {
				t.Fatal(err)
			}
			if len(registry.CodexAccounts) != 1 {
				t.Fatalf("rows=%d", len(registry.CodexAccounts))
			}
			row := registry.CodexAccounts[0]
			if row.Kind.Owned == nil || row.Kind.Owned.Refresh != tt.wantRefresh || row.Forgotten || row.Label == nil || *row.Label != tt.wantLabel || row.CreatedAt != before.CreatedAt {
				t.Fatalf("record metadata differs: %+v", row)
			}
			if diff := gocmp.Diff(identity.Email, row.Email); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(identity.Plan, row.PlanType); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
