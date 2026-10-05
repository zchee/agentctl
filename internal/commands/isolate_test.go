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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
)

func isolateFixture(t *testing.T) (*config.Paths, *config.AccountRecord, *claude.EnvView) {
	t.Helper()
	home := t.TempDir()
	paths := config.NewPaths(filepath.Join(home, "store"))
	env := claude.EnvWithHome(home)
	if err := os.MkdirAll(claude.LiveStoreDir(&env), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude.ClaudeJSONPath(&env), []byte(`{"theme":"dark","oauthAccount":{"accessToken":"private"},"mcpServers":{"private":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ns := paths.NamespaceDir("account", "organization")
	rec := &config.AccountRecord{AccountUUID: "account", OrganizationUUID: "organization", Kind: config.AccountKindOwned(ns, claude.SHA8(ns))}
	return paths, rec, &env
}

func TestEnsureSessionLayout(t *testing.T) {
	tests := map[string]struct{ fresh, noMCP bool }{
		"success: default":       {},
		"success: fresh context": {fresh: true},
		"success: without mcp":   {noMCP: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, rec, env := isolateFixture(t)
			live := claude.LiveStoreDir(env)
			all := append(SessionTierOne(), SessionTierTwo()...)
			for _, name := range all {
				if err := os.Mkdir(filepath.Join(live, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"history.jsonl", ".credentials.json", "plugins"} {
				if err := os.WriteFile(filepath.Join(live, name), []byte("not shared"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opts := SessionOptions{FreshContext: tt.fresh, NoMCP: tt.noMCP}
			got, err := EnsureSession(t.Context(), paths, rec, opts, env)
			if err != nil {
				t.Fatal(err)
			}
			want := all
			if tt.fresh {
				want = SessionTierOne()
			}
			if diff := gocmp.Diff(want, got.Linked); diff != "" {
				t.Fatalf("linked (-want +got):\n%s", diff)
			}
			for _, name := range want {
				target, err := os.Readlink(filepath.Join(got.Path, name))
				if err != nil {
					t.Fatal(err)
				}
				expected, err := claude.Canonical(filepath.Join(live, name))
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(expected, target); diff != "" {
					t.Fatalf("%s target (-want +got): %s", name, diff)
				}
			}
			for _, name := range []string{"history.jsonl", ".credentials.json", "plugins"} {
				if _, err := os.Lstat(filepath.Join(got.Path, name)); !os.IsNotExist(err) {
					t.Fatalf("unexpected session entry %s: %v", name, err)
				}
			}
			seed, err := os.ReadFile(filepath.Join(got.Path, ".claude.json"))
			if err != nil {
				t.Fatal(err)
			}
			expected := "{\n  \"theme\": \"dark\",\n  \"hasCompletedOnboarding\": true\n}"
			if diff := gocmp.Diff(expected, string(seed)); diff != "" {
				t.Fatalf("seed (-want +got):\n%s", diff)
			}
			for path, mode := range map[string]os.FileMode{got.Path: 0o700, filepath.Join(got.Path, ".claude.json"): 0o600} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Errorf("%s mode %o, want %o", path, info.Mode().Perm(), mode)
				}
			}
			if tt.noMCP {
				if got.MCPConfig != "" {
					t.Errorf("unexpected MCP path %s", got.MCPConfig)
				}
			} else {
				target, err := os.Readlink(got.MCPConfig)
				if err != nil {
					t.Fatal(err)
				}
				expected, err := claude.Canonical(claude.ClaudeJSONPath(env))
				if err != nil {
					t.Fatal(err)
				}
				if target != expected {
					t.Errorf("MCP target %s, want %s", target, expected)
				}
			}
			extended := []byte(`{"theme":"light","projects":{"mine":{}}}`)
			if err := os.WriteFile(filepath.Join(got.Path, ".claude.json"), extended, 0o600); err != nil {
				t.Fatal(err)
			}
			again, err := EnsureSession(t.Context(), paths, rec, opts, env)
			if err != nil {
				t.Fatal(err)
			}
			if len(again.Linked) != 0 || len(again.AlreadyLinked) != len(want) {
				t.Fatalf("not idempotent: %+v", again)
			}
			after, err := os.ReadFile(filepath.Join(got.Path, ".claude.json"))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(extended, after); diff != "" {
				t.Fatalf("seed rewritten: %s", diff)
			}
		})
	}
}

func TestEnsureSessionRefusals(t *testing.T) {
	tests := map[string]struct {
		setup    func(*testing.T, *config.Paths, *config.AccountRecord, *claude.EnvView, *SessionOptions)
		contains string
	}{
		"error: live account": {setup: func(_ *testing.T, _ *config.Paths, r *config.AccountRecord, _ *claude.EnvView, _ *SessionOptions) {
			r.Kind = config.AccountKindLive()
		}, contains: "only an account agentctl owns"},
		"error: relative override": {setup: func(_ *testing.T, _ *config.Paths, _ *config.AccountRecord, _ *claude.EnvView, o *SessionOptions) {
			o.ConfigDir = "relative"
		}, contains: "absolute path"},
		"error: dot override": {setup: func(_ *testing.T, _ *config.Paths, _ *config.AccountRecord, e *claude.EnvView, o *SessionOptions) {
			o.ConfigDir = e.Home + "/./isolated"
		}, contains: "normalized path"},
		"error: parent override": {setup: func(_ *testing.T, _ *config.Paths, _ *config.AccountRecord, e *claude.EnvView, o *SessionOptions) {
			o.ConfigDir = e.Home + "/missing/../.claude"
		}, contains: "normalized path"},
		"error: live override": {setup: func(_ *testing.T, _ *config.Paths, _ *config.AccountRecord, e *claude.EnvView, o *SessionOptions) {
			o.ConfigDir = claude.LiveStoreDir(e)
		}, contains: "live Claude Code configuration directory"},
		"error: symlink live override": {setup: func(t *testing.T, _ *config.Paths, _ *config.AccountRecord, e *claude.EnvView, o *SessionOptions) {
			o.ConfigDir = filepath.Join(e.Home, "alias")
			if err := os.Symlink(claude.LiveStoreDir(e), o.ConfigDir); err != nil {
				t.Fatal(err)
			}
		}, contains: "live Claude Code configuration directory"},
		"error: symlink session": {setup: func(t *testing.T, _ *config.Paths, _ *config.AccountRecord, e *claude.EnvView, o *SessionOptions) {
			o.ConfigDir = filepath.Join(e.Home, "alias")
			target := t.TempDir()
			if err := os.Symlink(target, o.ConfigDir); err != nil {
				t.Fatal(err)
			}
		}, contains: "plain directory"},
		"error: foreign tier entry": {setup: func(t *testing.T, p *config.Paths, r *config.AccountRecord, e *claude.EnvView, _ *SessionOptions) {
			dir := p.SessionDir(r.AccountUUID, r.OrganizationUUID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{filepath.Join(dir, "settings.json"), filepath.Join(claude.LiveStoreDir(e), "settings.json")} {
				if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}, contains: "settings.json"},
		"error: dangling seed": {setup: func(t *testing.T, p *config.Paths, r *config.AccountRecord, e *claude.EnvView, _ *SessionOptions) {
			dir := p.SessionDir(r.AccountUUID, r.OrganizationUUID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(e.Home, "absent"), filepath.Join(dir, ".claude.json")); err != nil {
				t.Fatal(err)
			}
		}, contains: "plain file"},
		"error: malformed live json": {setup: func(t *testing.T, _ *config.Paths, _ *config.AccountRecord, e *claude.EnvView, _ *SessionOptions) {
			if err := os.WriteFile(claude.ClaudeJSONPath(e), []byte(`{"private-secret`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, contains: "not valid JSON"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, rec, env := isolateFixture(t)
			opts := SessionOptions{}
			tt.setup(t, paths, rec, env, &opts)
			_, err := EnsureSession(t.Context(), paths, rec, opts, env)
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("got %v, want error containing %q", err, tt.contains)
			}
			if strings.Contains(err.Error(), "private-secret") {
				t.Fatal("input bytes leaked through error")
			}
		})
	}
}

func TestSessionSeedConfigLockAndPreferredFile(t *testing.T) {
	tests := map[string]struct{ held bool }{"success: free config lock": {}, "success: busy config lock": {held: true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, rec, env := isolateFixture(t)
			preferred := filepath.Join(claude.LiveStoreDir(env), ".config.json")
			if err := os.WriteFile(preferred, []byte(`{"hasCompletedOnboarding":false,"editorMode":"vim"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			lock := preferred + ".lock"
			if tt.held {
				if err := os.Mkdir(lock, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			session, err := EnsureSession(t.Context(), paths, rec, SessionOptions{}, env)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(session.Path, ".claude.json"))
			if err != nil {
				t.Fatal(err)
			}
			want := "{\n  \"hasCompletedOnboarding\": false,\n  \"editorMode\": \"vim\"\n}"
			if diff := gocmp.Diff(want, string(data)); diff != "" {
				t.Fatalf("seed (-want +got): %s", diff)
			}
			_, err = os.Stat(lock)
			if tt.held && err != nil {
				t.Fatalf("removed peer lock: %v", err)
			}
			if !tt.held && !os.IsNotExist(err) {
				t.Fatalf("left config lock: %v", err)
			}
		})
	}
}

func TestSessionPolicy(t *testing.T) {
	tests := map[string]struct {
		list func() []string
		want []string
	}{
		"success: preferences":              {SessionTierOne, []string{"settings.json", "CLAUDE.md", "skills"}},
		"success: history directories":      {SessionTierTwo, []string{"projects", "shell-snapshots", "file-history", "sessions", "session-env"}},
		"success: independent history file": {NeverLinked, []string{"history.jsonl"}},
		"success: account scoped values":    {NeverSeeded, []string{"oauthAccount", "userID", "machineID", "cachedUsageUtilization", "overageCreditGrantCache", "passesEligibilityCache", "s1mAccessCache", "s1mNonSubscriberAccessCache", "customApiKeyResponses", "mcpServers", "projects", "modelAccessCache", "orgModelDefaultCache", "cachedExtraUsageDisabledReason", "additionalModelOptionsCache", "additionalModelOptionsAnsweredAt", "additionalModelCostsCache", "autoCompactWindowsCache", "clientDataCacheSlots"}},
		"success: seed keys":                {SessionSeedKeys, []string{"hasCompletedOnboarding", "lastOnboardingVersion", "lastReleaseNotesSeen", "hasSeenAutoDefaultNotice", "hasCompletedClaudeInChromeOnboarding", "theme", "preferredNotifChannel", "editorMode", "autoUpdates", "autoUpdatesProtectedForNative", "installMethod", "bypassPermissionsModeAccepted", "hasAcknowledgedCostThreshold", "shiftEnterKeyBindingInstalled", "verbose"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := tt.list()
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatal(diff)
			}
			got[0] = "changed"
			if diff := gocmp.Diff(tt.want, tt.list()); diff != "" {
				t.Fatalf("shared mutable policy: %s", diff)
			}
		})
	}
	for _, name := range SessionSeedKeys() {
		if slices.Contains(NeverSeeded(), name) {
			t.Fatalf("seed policy includes forbidden key %s", name)
		}
	}
}

func TestSessionMissingDirectories(t *testing.T) {
	paths, rec, env := isolateFixture(t)
	if err := os.WriteFile(filepath.Join(claude.LiveStoreDir(env), "projects"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureSession(t.Context(), paths, rec, SessionOptions{}, env)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Missing, "projects") {
		t.Fatalf("non-directory projects was not reported missing: %+v", got)
	}
	if len(got.Linked) != 0 {
		t.Fatalf("unexpected links: %v", got.Linked)
	}
}
