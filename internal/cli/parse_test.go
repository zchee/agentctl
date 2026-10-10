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

package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// execute assembles a fresh command tree around h and runs it on args,
// returning the CLI so a test can also inspect whether dispatch began.
func execute(t *testing.T, h Handlers, args ...string) (*CLI, error) {
	t.Helper()

	c := New(h)
	root := c.Root()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return c, root.ExecuteContext(t.Context())
}

// mustReject runs args through an empty handler set and fails unless the
// command line was refused before dispatch, which is the usage-error
// contract that exits 2.
func mustReject(t *testing.T, args ...string) string {
	t.Helper()

	c, err := execute(t, Handlers{}, args...)
	if err == nil {
		t.Fatalf("expected `%s` to be rejected, but it parsed", strings.Join(args, " "))
	}
	if c.Dispatched() {
		t.Fatalf("`%s` was rejected after dispatch, so it would not exit 2: %v", strings.Join(args, " "), err)
	}
	return err.Error()
}

func TestClaudeStatusDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()

	var got ClaudeStatusOptions
	h := Handlers{ClaudeStatus: func(_ context.Context, _ Globals, opts ClaudeStatusOptions) error {
		got = opts
		return nil
	}}
	if _, err := execute(t, h, "claude", "status"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeStatusOptions{Timeout: 10 * time.Second}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeStatusAcceptsEveryFlagAndRepeatsAccount(t *testing.T) {
	t.Parallel()

	var got ClaudeStatusOptions
	h := Handlers{ClaudeStatus: func(_ context.Context, _ Globals, opts ClaudeStatusOptions) error {
		got = opts
		return nil
	}}
	_, err := execute(t, h, "claude", "status", "--json", "--raw", "--refresh", "--no-cache", "--all", "--by-identity", "--account", "acct-a", "--account", "acct-b/org-1", "--timeout", "5m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeStatusOptions{
		JSON:       true,
		Raw:        true,
		Refresh:    true,
		NoCache:    true,
		All:        true,
		ByIdentity: true,
		Accounts:   []string{"acct-a", "acct-b/org-1"},
		Timeout:    300 * time.Second,
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("flags mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeStatusRejectsAnUnparseableTimeout(t *testing.T) {
	t.Parallel()

	message := mustReject(t, "claude", "status", "--timeout", "10 fortnights")
	if message == "" {
		t.Fatal("the rejection should be explained")
	}
}

func TestClaudeWatchIntervalDefaultsToThreeHundredSeconds(t *testing.T) {
	t.Parallel()

	var got ClaudeWatchOptions
	h := Handlers{ClaudeWatch: func(_ context.Context, _ Globals, opts ClaudeWatchOptions) error {
		got = opts
		return nil
	}}
	if _, err := execute(t, h, "claude", "watch"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Interval != 300*time.Second {
		t.Fatalf("default interval = %v, want 300s", got.Interval)
	}
}

func TestClaudeWatchIntervalBelowTheFloorIsRejectedNamingTheFloor(t *testing.T) {
	t.Parallel()

	for _, below := range []string{"30s", "0", "1s", "59s", "59"} {
		message := mustReject(t, "claude", "watch", "--interval", below)
		if !strings.Contains(message, "60") {
			t.Fatalf("`%s` must be refused naming 60: %s", below, message)
		}
	}
}

func TestClaudeWatchIntervalAtOrAboveTheFloorIsAccepted(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input string
		want  time.Duration
	}{
		"success: exactly the floor":             {input: "60s", want: 60 * time.Second},
		"success: bare seconds at the floor":     {input: "60", want: 60 * time.Second},
		"success: one minute spelled as minutes": {input: "1m", want: 60 * time.Second},
		"success: well above the floor":          {input: "10m", want: 600 * time.Second},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got ClaudeWatchOptions
			h := Handlers{ClaudeWatch: func(_ context.Context, _ Globals, opts ClaudeWatchOptions) error {
				got = opts
				return nil
			}}
			if _, err := execute(t, h, "claude", "watch", "--interval", tt.input); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Interval != tt.want {
				t.Fatalf("interval = %v, want %v", got.Interval, tt.want)
			}
		})
	}
}

func TestClaudeLoginParsesItsFlags(t *testing.T) {
	t.Parallel()

	var got ClaudeLoginOptions
	h := Handlers{ClaudeLogin: func(_ context.Context, _ Globals, opts ClaudeLoginOptions) error {
		got = opts
		return nil
	}}

	if _, err := execute(t, h, "claude", "login"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff := gocmp.Diff(ClaudeLoginOptions{}, got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}

	if _, err := execute(t, h, "claude", "login", "--manual", "--label", "work", "--no-duplicate"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeLoginOptions{Manual: true, Label: "work", NoDuplicate: true}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("flags mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeAccountsSubcommandsAllParse(t *testing.T) {
	t.Parallel()

	var (
		gotList     ClaudeAccountsListOptions
		gotShow     ClaudeAccountsShowOptions
		gotRemove   ClaudeAccountsRemoveOptions
		gotRelocate ClaudeAccountsRelocateOptions
		gotForget   ClaudeAccountsForgetOptions
		gotUnforget ClaudeAccountsUnforgetOptions
	)
	h := Handlers{
		ClaudeAccountsList: func(_ context.Context, _ Globals, opts ClaudeAccountsListOptions) error {
			gotList = opts
			return nil
		},
		ClaudeAccountsShow: func(_ context.Context, _ Globals, opts ClaudeAccountsShowOptions) error {
			gotShow = opts
			return nil
		},
		ClaudeAccountsRemove: func(_ context.Context, _ Globals, opts ClaudeAccountsRemoveOptions) error {
			gotRemove = opts
			return nil
		},
		ClaudeAccountsRelocate: func(_ context.Context, _ Globals, opts ClaudeAccountsRelocateOptions) error {
			gotRelocate = opts
			return nil
		},
		ClaudeAccountsForget: func(_ context.Context, _ Globals, opts ClaudeAccountsForgetOptions) error {
			gotForget = opts
			return nil
		},
		ClaudeAccountsUnforget: func(_ context.Context, _ Globals, opts ClaudeAccountsUnforgetOptions) error {
			gotUnforget = opts
			return nil
		},
	}

	if _, err := execute(t, h, "claude", "accounts", "list", "--all"); err != nil {
		t.Fatalf("accounts list: %v", err)
	}
	if !gotList.All {
		t.Fatal("accounts list --all must set All")
	}

	if _, err := execute(t, h, "claude", "accounts", "show", "acct-1"); err != nil {
		t.Fatalf("accounts show: %v", err)
	}
	if gotShow.ID != "acct-1" {
		t.Fatalf("accounts show id = %q, want acct-1", gotShow.ID)
	}

	if _, err := execute(t, h, "claude", "accounts", "remove", "acct-1", "--delete-secret", "--yes"); err != nil {
		t.Fatalf("accounts remove: %v", err)
	}
	wantRemove := ClaudeAccountsRemoveOptions{ID: "acct-1", DeleteSecret: true, Yes: true}
	if diff := gocmp.Diff(wantRemove, gotRemove); diff != "" {
		t.Fatalf("accounts remove mismatch (-want +got):\n%s", diff)
	}

	if _, err := execute(t, h, "claude", "accounts", "remove", "acct-1"); err != nil {
		t.Fatalf("accounts remove bare: %v", err)
	}
	if gotRemove.DeleteSecret || gotRemove.Yes {
		t.Fatal("delete-secret and yes must be opt-in")
	}

	if _, err := execute(t, h, "claude", "accounts", "relocate", "acct-1", "--yes"); err != nil {
		t.Fatalf("accounts relocate: %v", err)
	}
	wantRelocate := ClaudeAccountsRelocateOptions{ID: "acct-1", Yes: true}
	if diff := gocmp.Diff(wantRelocate, gotRelocate); diff != "" {
		t.Fatalf("accounts relocate mismatch (-want +got):\n%s", diff)
	}

	if _, err := execute(t, h, "claude", "accounts", "forget", "Claude Code-credentials"); err != nil {
		t.Fatalf("accounts forget: %v", err)
	}
	if gotForget.Service != "Claude Code-credentials" {
		t.Fatalf("accounts forget service = %q", gotForget.Service)
	}

	if _, err := execute(t, h, "claude", "accounts", "unforget", "Claude Code-credentials"); err != nil {
		t.Fatalf("accounts unforget: %v", err)
	}
	if gotUnforget.Service != "Claude Code-credentials" {
		t.Fatalf("accounts unforget service = %q", gotUnforget.Service)
	}
}

func TestClaudeImportParsesItsSourceAndRepeatsConfigDir(t *testing.T) {
	t.Parallel()

	var got ClaudeImportOptions
	h := Handlers{ClaudeImport: func(_ context.Context, _ Globals, opts ClaudeImportOptions) error {
		got = opts
		return nil
	}}

	if _, err := execute(t, h, "claude", "import", "--from", "keychain", "--dry-run"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantDry := ClaudeImportOptions{From: ImportSourceKeychain, DryRun: true}
	if diff := gocmp.Diff(wantDry, got); diff != "" {
		t.Fatalf("import --dry-run mismatch (-want +got):\n%s", diff)
	}

	if _, err := execute(t, h, "claude", "import", "--from", "keychain", "--claude-config-dir", "/one", "--claude-config-dir", "/two"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeImportOptions{From: ImportSourceKeychain, ClaudeConfigDirs: []string{"/one", "/two"}}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("import mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeImportRequiresASource(t *testing.T) {
	t.Parallel()

	message := mustReject(t, "claude", "import")
	if !strings.Contains(message, "--from") {
		t.Fatalf("the error should name the missing flag: %s", message)
	}
}

func TestClaudeImportRejectsAnUnknownSource(t *testing.T) {
	t.Parallel()

	message := mustReject(t, "claude", "import", "--from", "sqlite")
	if !strings.Contains(message, "keychain") {
		t.Fatalf("the error should name the accepted source: %s", message)
	}
}

func TestClaudeDoctorParsesItsFlags(t *testing.T) {
	t.Parallel()

	var got ClaudeDoctorOptions
	h := Handlers{ClaudeDoctor: func(_ context.Context, _ Globals, opts ClaudeDoctorOptions) error {
		got = opts
		return nil
	}}

	if _, err := execute(t, h, "claude", "doctor"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff := gocmp.Diff(ClaudeDoctorOptions{}, got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}

	if _, err := execute(t, h, "claude", "doctor", "--remove-stale", "/tmp/ns/.oauth_refresh.lock", "--yes"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeDoctorOptions{RemoveStale: "/tmp/ns/.oauth_refresh.lock", Yes: true}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("flags mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeUseBareIDParsesWithEveryDefaultOff(t *testing.T) {
	t.Parallel()

	var got ClaudeUseOptions
	h := Handlers{ClaudeUse: func(_ context.Context, _ Globals, opts ClaudeUseOptions) error {
		got = opts
		return nil
	}}
	if _, err := execute(t, h, "claude", "use", "acct-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeUseOptions{ID: "acct-1"}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeUseAcceptsEveryFlagTogetherWithAnID(t *testing.T) {
	t.Parallel()

	var got ClaudeUseOptions
	h := Handlers{ClaudeUse: func(_ context.Context, _ Globals, opts ClaudeUseOptions) error {
		got = opts
		return nil
	}}
	_, err := execute(t, h, "claude", "use", "acct-1", "--claude-config-dir", "/tmp/session", "--fresh-context", "--no-mcp", "--yes", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeUseOptions{
		ID:              "acct-1",
		ClaudeConfigDir: "/tmp/session",
		FreshContext:    true,
		NoMCP:           true,
		Yes:             true,
		JSON:            true,
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("flags mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeUseUndoAndForgetParseWithoutAnID(t *testing.T) {
	t.Parallel()

	var got ClaudeUseOptions
	h := Handlers{ClaudeUse: func(_ context.Context, _ Globals, opts ClaudeUseOptions) error {
		got = opts
		return nil
	}}

	if _, err := execute(t, h, "claude", "use", "--undo", "--yes"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "" || !got.Undo || !got.Yes {
		t.Fatalf("use --undo --yes parsed as %+v", got)
	}

	if _, err := execute(t, h, "claude", "use", "--forget", "acct-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "" || got.Forget != "acct-1" {
		t.Fatalf("use --forget parsed as %+v", got)
	}
}

func TestClaudeUseLiveConflictsWithNewOnlyUndoAndForget(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"claude", "use", "acct-1", "--live", "--new-only"},
		{"claude", "use", "--live", "--undo"},
		{"claude", "use", "--live", "--forget", "acct-1"},
	} {
		mustReject(t, args...)
	}
}

func TestClaudeUseUndoAndForgetConflictWithAnIDAndWithEachOther(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"claude", "use", "acct-1", "--undo"},
		{"claude", "use", "acct-1", "--forget", "acct-1"},
		{"claude", "use", "--undo", "--forget", "acct-1"},
	} {
		mustReject(t, args...)
	}
}

func TestClaudeUseRestartRemoteControlRequiresALivePass(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		flags    []string
		accepted bool
	}{
		"success: with --live":                      {flags: []string{"--live", "--restart-remote-control"}, accepted: true},
		"success: with --live and an id":            {flags: []string{"--live", "account", "--restart-remote-control"}, accepted: true},
		"success: with --undo":                      {flags: []string{"--undo", "--restart-remote-control"}, accepted: true},
		"success: --live --yes without the restart": {flags: []string{"--live", "--yes"}, accepted: true},
		"error: alone":                              {flags: []string{"--restart-remote-control"}},
		"error: with an isolated session id":        {flags: []string{"account", "--restart-remote-control"}},
		"error: with --forget":                      {flags: []string{"--forget", "account", "--restart-remote-control"}},
		"success: --live --yes with the restart":    {flags: []string{"--live", "--yes", "--restart-remote-control"}, accepted: true},
		"success: --undo --yes with the restart":    {flags: []string{"--undo", "--yes", "--restart-remote-control"}, accepted: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			args := append([]string{"claude", "use"}, tt.flags...)
			if tt.accepted {
				h := Handlers{ClaudeUse: func(_ context.Context, _ Globals, _ ClaudeUseOptions) error { return nil }}
				if _, err := execute(t, h, args...); err != nil {
					t.Fatalf("expected `%s` to parse: %v", strings.Join(args, " "), err)
				}
				return
			}
			mustReject(t, args...)
		})
	}
}

func TestClaudeExecRequiresATrailingCommand(t *testing.T) {
	t.Parallel()

	mustReject(t, "claude", "exec", "acct-1")
	mustReject(t, "claude", "exec", "acct-1", "--")
}

func TestClaudeExecParsesTheIDAndTheTrailingCommand(t *testing.T) {
	t.Parallel()

	var got ClaudeExecOptions
	h := Handlers{ClaudeExec: func(_ context.Context, _ Globals, opts ClaudeExecOptions) error {
		got = opts
		return nil
	}}
	_, err := execute(t, h, "claude", "exec", "acct-1", "--claude-config-dir", "/tmp/session", "--fresh-context", "--no-mcp", "--", "claude", "--resume")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeExecOptions{
		ID:              "acct-1",
		ClaudeConfigDir: "/tmp/session",
		FreshContext:    true,
		NoMCP:           true,
		Command:         []string{"claude", "--resume"},
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("exec mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeEnvDefaultsToZsh(t *testing.T) {
	t.Parallel()

	var got ClaudeEnvOptions
	h := Handlers{ClaudeEnv: func(_ context.Context, _ Globals, opts ClaudeEnvOptions) error {
		got = opts
		return nil
	}}
	if _, err := execute(t, h, "claude", "env", "acct-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ClaudeEnvOptions{ID: "acct-1", Shell: ShellZsh}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}
}

func TestClaudeEnvAcceptsEveryDocumentedShell(t *testing.T) {
	t.Parallel()

	for flag, want := range map[string]Shell{"zsh": ShellZsh, "bash": ShellBash, "fish": ShellFish} {
		var got ClaudeEnvOptions
		h := Handlers{ClaudeEnv: func(_ context.Context, _ Globals, opts ClaudeEnvOptions) error {
			got = opts
			return nil
		}}
		if _, err := execute(t, h, "claude", "env", "acct-1", "--shell", flag); err != nil {
			t.Fatalf("--shell %s: %v", flag, err)
		}
		if got.Shell != want {
			t.Fatalf("--shell %s parsed as %q", flag, got.Shell)
		}
	}
}

func TestClaudeEnvRejectsAnUnknownShell(t *testing.T) {
	t.Parallel()

	message := mustReject(t, "claude", "env", "acct-1", "--shell", "powershell")
	if !strings.Contains(message, "zsh") {
		t.Fatalf("the error should name the accepted shells: %s", message)
	}
}

func TestCodexStatusParsesItsFlags(t *testing.T) {
	t.Parallel()

	var got CodexStatusOptions
	h := Handlers{CodexStatus: func(_ context.Context, _ Globals, opts CodexStatusOptions) error {
		got = opts
		return nil
	}}

	if _, err := execute(t, h, "codex", "status"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff := gocmp.Diff(CodexStatusOptions{Timeout: 10 * time.Second}, got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}

	_, err := execute(t, h, "codex", "status", "--json", "--raw", "--refresh", "--no-cache", "--all", "--account", "a@b", "--timeout", "1m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := CodexStatusOptions{
		JSON:     true,
		Raw:      true,
		Refresh:  true,
		NoCache:  true,
		All:      true,
		Accounts: []string{"a@b"},
		Timeout:  60 * time.Second,
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("flags mismatch (-want +got):\n%s", diff)
	}
}

func TestCodexAccountsSetRequiresARefreshMode(t *testing.T) {
	t.Parallel()

	message := mustReject(t, "codex", "accounts", "set", "acct-1")
	if !strings.Contains(message, "--refresh") {
		t.Fatalf("the error should name the missing flag: %s", message)
	}

	var got CodexAccountsSetOptions
	h := Handlers{CodexAccountsSet: func(_ context.Context, _ Globals, opts CodexAccountsSetOptions) error {
		got = opts
		return nil
	}}
	for flag, want := range map[string]RefreshMode{"auto": RefreshModeAuto, "never": RefreshModeNever} {
		if _, err := execute(t, h, "codex", "accounts", "set", "acct-1", "--refresh", flag); err != nil {
			t.Fatalf("--refresh %s: %v", flag, err)
		}
		if got.ID != "acct-1" || got.Refresh != want {
			t.Fatalf("--refresh %s parsed as %+v", flag, got)
		}
	}

	mustReject(t, "codex", "accounts", "set", "acct-1", "--refresh", "sometimes")
}

func TestCodexAccountsRefreshFlagsConflict(t *testing.T) {
	t.Parallel()

	var got CodexAccountsRefreshOptions
	h := Handlers{CodexAccountsRefresh: func(_ context.Context, _ Globals, opts CodexAccountsRefreshOptions) error {
		got = opts
		return nil
	}}
	if _, err := execute(t, h, "codex", "accounts", "refresh", "acct-1", "--resend", "--yes"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := CodexAccountsRefreshOptions{ID: "acct-1", Resend: true, Yes: true}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("refresh mismatch (-want +got):\n%s", diff)
	}

	mustReject(t, "codex", "accounts", "refresh", "acct-1", "--resend", "--reset-floor")
}

func TestCodexImportParsesItsSource(t *testing.T) {
	t.Parallel()

	var got CodexImportOptions
	h := Handlers{CodexImport: func(_ context.Context, _ Globals, opts CodexImportOptions) error {
		got = opts
		return nil
	}}
	_, err := execute(t, h, "codex", "import", "--from", "codex-home", "--codex-home", "/tmp/home", "--dry-run")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := CodexImportOptions{From: CodexImportSourceCodexHome, CodexHome: "/tmp/home", DryRun: true}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("import mismatch (-want +got):\n%s", diff)
	}

	mustReject(t, "codex", "import")
}

func TestTopLevelConfigDirIsAcceptedBeforeTheSubcommand(t *testing.T) {
	t.Parallel()

	var got Globals
	h := Handlers{ClaudeStatus: func(_ context.Context, globals Globals, _ ClaudeStatusOptions) error {
		got = globals
		return nil
	}}
	if _, err := execute(t, h, "--config-dir", "/custom/store", "claude", "status"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ConfigDir != "/custom/store" {
		t.Fatalf("ConfigDir = %q, want /custom/store", got.ConfigDir)
	}
}

func TestConfigDirIsGlobalAndAcceptedAfterTheSubcommand(t *testing.T) {
	t.Parallel()

	var got Globals
	h := Handlers{
		ClaudeStatus: func(_ context.Context, globals Globals, _ ClaudeStatusOptions) error {
			got = globals
			return nil
		},
		ClaudeAccountsList: func(_ context.Context, globals Globals, _ ClaudeAccountsListOptions) error {
			got = globals
			return nil
		},
	}

	if _, err := execute(t, h, "claude", "status", "--config-dir", "/custom/store"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ConfigDir != "/custom/store" {
		t.Fatalf("ConfigDir = %q, want /custom/store", got.ConfigDir)
	}

	if _, err := execute(t, h, "claude", "accounts", "list", "--config-dir", "/custom/store"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ConfigDir != "/custom/store" {
		t.Fatalf("nested ConfigDir = %q, want /custom/store", got.ConfigDir)
	}
}

func TestConfigDirFallsBackToTheEnvironment(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("AGENTCTL_CONFIG_DIR", "/from/env")

	var got Globals
	h := Handlers{ClaudeDoctor: func(_ context.Context, globals Globals, _ ClaudeDoctorOptions) error {
		got = globals
		return nil
	}}

	if _, err := execute(t, h, "claude", "doctor"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ConfigDir != "/from/env" {
		t.Fatalf("ConfigDir = %q, want the environment fallback", got.ConfigDir)
	}

	// The flag wins over the variable.
	if _, err := execute(t, h, "claude", "doctor", "--config-dir", "/from/flag"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ConfigDir != "/from/flag" {
		t.Fatalf("ConfigDir = %q, want the flag to win", got.ConfigDir)
	}
}

func TestConfigDirDefaultsToEmpty(t *testing.T) {
	// t.Setenv forbids t.Parallel; clearing the variable pins the default.
	t.Setenv("AGENTCTL_CONFIG_DIR", "")

	var got Globals
	h := Handlers{ClaudeDoctor: func(_ context.Context, globals Globals, _ ClaudeDoctorOptions) error {
		got = globals
		return nil
	}}
	if _, err := execute(t, h, "claude", "doctor"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ConfigDir != "" {
		t.Fatalf("ConfigDir = %q, want empty so path resolution applies its own precedence", got.ConfigDir)
	}
}

func TestUnknownSubcommandsAreRejected(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"claude", "bogus"},
		{"bogus"},
		{"claude", "accounts", "bogus"},
	} {
		mustReject(t, args...)
	}
}

func TestASubcommandIsRequired(t *testing.T) {
	t.Parallel()

	mustReject(t)
	mustReject(t, "claude")
	mustReject(t, "codex")
	mustReject(t, "claude", "accounts")
}

func TestANilHandlerFailsLoudly(t *testing.T) {
	t.Parallel()

	c, err := execute(t, Handlers{}, "claude", "status")
	if err == nil {
		t.Fatal("a nil handler must fail loudly instead of exiting clean")
	}
	if !c.Dispatched() {
		t.Fatal("the failure must come after dispatch, so it is not a usage error")
	}
	notImplemented, ok := errors.AsType[*NotImplementedError](err)
	if !ok {
		t.Fatalf("expected a NotImplementedError, got %#v", err)
	}
	if notImplemented.Command != "claude status" {
		t.Fatalf("Command = %q, want %q", notImplemented.Command, "claude status")
	}
	if notImplemented.Error() != "claude status: not implemented" {
		t.Fatalf("message = %q", notImplemented.Error())
	}
}

func TestAGroupNamesTheUnknownSubcommandRatherThanDemandingOne(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args []string
		want string
	}{
		"error: an unknown claude subcommand is named": {
			args: []string{"claude", "bogus"},
			want: `unknown command "bogus" for "agentctl claude"`,
		},
		"error: an unknown accounts subcommand is named": {
			args: []string{"claude", "accounts", "bogus"},
			want: `unknown command "bogus" for "agentctl claude accounts"`,
		},
		"error: a bare group still asks for a subcommand": {
			args: []string{"claude"},
			want: "a subcommand is required; run `agentctl claude --help` for the list",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			message := mustReject(t, tt.args...)
			if message != tt.want {
				t.Errorf("rejection = %q, want %q", message, tt.want)
			}
		})
	}
}
