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
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
)

func TestResolveAccount(t *testing.T) {
	email, label := "owner@example.invalid", "personal"
	rows := []config.CodexAccountRecord{
		{ChatGPTUserID: "user-a", ChatGPTAccountID: "account-a", Email: &email, Label: &label},
		{ChatGPTUserID: "user-a", ChatGPTAccountID: "account-b", Email: &email},
	}
	tests := map[string]struct {
		selector  string
		want      string
		wantError string
	}{
		"success: exact pair wins": {selector: "user-a/account-a", want: "user-a/account-a"},
		"success: account":         {selector: "account-b", want: "user-a/account-b"},
		"success: label":           {selector: "personal", want: "user-a/account-a"},
		"error: ambiguous user":    {selector: "user-a", wantError: "matches 2 rows; use one of: user-a/account-a, user-a/account-b"},
		"error: ambiguous email":   {selector: email, wantError: "matches 2 rows"},
		"error: unknown":           {selector: "absent", wantError: "no account matches `absent`"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveAccount(rows, test.selector)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("selector %q: error %v, want %q", test.selector, err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, AccountKey(got)); diff != "" {
				t.Fatalf("selector result (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAccountsRegistryOnlyCommands(t *testing.T) {
	tests := map[string]struct {
		kind      config.CodexKind
		mode      cli.RefreshMode
		wantError string
	}{
		"success: owned disables refresh": {kind: config.CodexKindOwned("/stored", config.RefreshAuto), mode: cli.RefreshModeNever},
		"success: owned enables refresh":  {kind: config.CodexKindOwned("/stored", config.RefreshNever), mode: cli.RefreshModeAuto},
		"error: live has no policy":       {kind: config.CodexKindLive(), mode: cli.RefreshModeNever, wantError: "did not create"},
		"error: imported has no policy":   {kind: config.CodexKindHomeReadOnly("/their-home"), mode: cli.RefreshModeNever, wantError: "did not create"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, err := config.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			email := "owner@example.invalid"
			row := config.CodexAccountRecord{ChatGPTUserID: "user-a", ChatGPTAccountID: "account-a", Email: &email, Kind: test.kind, CreatedAt: "2026-09-17T00:00:00Z"}
			if err := config.UpdateRegistry(t.Context(), paths, func(registry *config.Registry) { registry.CodexAccounts = []config.CodexAccountRecord{row} }); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			accounts := Accounts{Paths: paths, Out: &out}
			err = accounts.Set(t.Context(), cli.CodexAccountsSetOptions{ID: email, Refresh: test.mode})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Set: %v, want %q", err, test.wantError)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				registry, err := config.LoadRegistry(t.Context(), paths)
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(config.RefreshPolicy(test.mode), registry.CodexAccounts[0].Kind.Owned.Refresh); diff != "" {
					t.Fatalf("policy (-want +got):\n%s", diff)
				}
			}
			if _, err := os.Lstat(paths.CodexRoot()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("registry-only Set created the Codex root: %v", err)
			}
			if err := accounts.Forget(t.Context(), email, true); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := accounts.List(t.Context(), cli.CodexAccountsListOptions{}); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff("no Codex accounts shown; 1 forgotten (`--all` shows them)\n", out.String()); diff != "" {
				t.Fatalf("hidden list (-want +got):\n%s", diff)
			}
			out.Reset()
			if err := accounts.List(t.Context(), cli.CodexAccountsListOptions{All: true}); err != nil {
				t.Fatal(err)
			}
			want := "user-a/account-a  " + accountKind(test.kind) + "  owner@example.invalid  (forgotten)\n"
			if diff := gocmp.Diff(want, out.String()); diff != "" {
				t.Fatalf("full list (-want +got):\n%s", diff)
			}
			if err := accounts.Forget(t.Context(), email, false); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := accounts.Show(t.Context(), cli.CodexAccountsShowOptions{ID: email}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "hidden:") {
				t.Fatalf("unforgotten row remains hidden:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "id:       user-a/account-a\n") {
				t.Fatalf("missing canonical ID:\n%s", out.String())
			}
			if _, err := os.Lstat(paths.CodexRoot()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("registry-only operations created the Codex root: %v", err)
			}
		})
	}
}
