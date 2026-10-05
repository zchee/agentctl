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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// unsetenv removes key for the duration of the test; t.Setenv first, so the
// original value is restored when the test ends.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%q) = %v", key, err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	home := t.TempDir()

	tests := map[string]struct {
		cli  string
		env  string
		want string
	}{
		"success: cli wins over env and xdg": {
			cli:  "/from/cli",
			env:  "/from/env",
			want: "/from/cli",
		},
		"success: env wins over xdg": {
			env:  "/from/env",
			want: "/from/env",
		},
		"success: xdg fallback appends the store name": {
			want: filepath.Join(home, ".config", "agctl"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			if tt.env != "" {
				t.Setenv(ConfigDirEnv, tt.env)
			} else {
				unsetenv(t, ConfigDirEnv)
			}

			resolved, err := Resolve(tt.cli)
			if err != nil {
				t.Fatalf("Resolve(%q) = %v", tt.cli, err)
			}
			if diff := gocmp.Diff(tt.want, resolved.ConfigDir()); diff != "" {
				t.Errorf("config dir mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveXDGConfigHome(t *testing.T) {
	home := t.TempDir()

	tests := map[string]struct {
		xdg  string
		want string
	}{
		"success: an absolute XDG_CONFIG_HOME is used": {
			xdg:  "/xdg/config",
			want: "/xdg/config/agctl",
		},
		"success: a relative XDG_CONFIG_HOME falls back to the home directory": {
			xdg:  "relative/config",
			want: filepath.Join(home, ".config", "agctl"),
		},
		"success: an empty XDG_CONFIG_HOME falls back to the home directory": {
			xdg:  "",
			want: filepath.Join(home, ".config", "agctl"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", home)
			unsetenv(t, ConfigDirEnv)
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)

			resolved, err := Resolve("")
			if err != nil {
				t.Fatalf("Resolve(\"\") = %v", err)
			}
			if diff := gocmp.Diff(tt.want, resolved.ConfigDir()); diff != "" {
				t.Errorf("config dir mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveSymlinkedHomeKeepsItsSpelling(t *testing.T) {
	tests := map[string]struct{}{
		"success: a home reached through a symlink is not resolved away": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			real := t.TempDir()
			link := filepath.Join(t.TempDir(), "home-link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatalf("Symlink(%q, %q) = %v", real, link, err)
			}
			t.Setenv("HOME", link)
			t.Setenv("XDG_CONFIG_HOME", "")
			unsetenv(t, ConfigDirEnv)

			resolved, err := Resolve("")
			if err != nil {
				t.Fatalf("Resolve(\"\") = %v", err)
			}
			want := filepath.Join(link, ".config", "agctl")
			if diff := gocmp.Diff(want, resolved.ConfigDir()); diff != "" {
				t.Errorf("config dir mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveReportsAMissingHomeOnlyWhenItIsNeeded(t *testing.T) {
	tests := map[string]struct {
		cli     string
		wantErr bool
	}{
		"error: no override and no home fails": {
			wantErr: true,
		},
		"success: an override never consults the base directory": {
			cli: "/store",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			unsetenv(t, ConfigDirEnv)

			resolved, err := Resolve(tt.cli)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = %v, want an error", tt.cli, resolved.ConfigDir())
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) = %v", tt.cli, err)
			}
			if diff := gocmp.Diff(tt.cli, resolved.ConfigDir()); diff != "" {
				t.Errorf("config dir mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDerivedPathsFollowTheDocumentedLayout(t *testing.T) {
	t.Parallel()

	paths := NewPaths("/store")
	tests := map[string]struct {
		got  string
		want string
	}{
		"success: config file":        {got: paths.ConfigFile(), want: "/store/config.json"},
		"success: config lock":        {got: paths.ConfigLock(), want: "/store/.config.lock"},
		"success: namespace root":     {got: paths.NamespaceRoot(), want: "/store/claude"},
		"success: namespace dir":      {got: paths.NamespaceDir("acct", "org"), want: "/store/claude/acct/org"},
		"success: locks dir":          {got: paths.LocksDir(), want: "/store/claude/.locks"},
		"success: lock path":          {got: paths.LockPath("acct", "org"), want: "/store/claude/.locks/acct.org.lock"},
		"success: cache root":         {got: paths.CacheRoot(), want: "/store/cache"},
		"success: cache dir":          {got: paths.CacheDir(), want: "/store/cache/claude"},
		"success: codex cache dir":    {got: paths.CodexCacheDir(), want: "/store/cache/codex"},
		"success: session root":       {got: paths.SessionRoot(), want: "/store/claude-sessions"},
		"success: session dir":        {got: paths.SessionDir("acct", "org"), want: "/store/claude-sessions/acct/org"},
		"success: codex root":         {got: paths.CodexRoot(), want: "/store/codex"},
		"success: codex locks dir":    {got: paths.CodexLocksDir(), want: "/store/codex/.locks"},
		"success: codex scratch lock": {got: paths.CodexScratchLock(), want: "/store/codex/.locks/scratch.lock"},
		"success: codex state dir":    {got: paths.CodexStateDir(), want: "/store/codex/.state"},
		"success: codex scratch root": {got: paths.CodexScratchRoot(), want: "/store/codex/.scratch"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := gocmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("path mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCodexDerivationsValidateBeforeAnyPathExists(t *testing.T) {
	t.Parallel()

	paths := NewPaths(filepath.Join(t.TempDir(), "agctl"))

	valid := map[string]struct {
		derive func() (string, error)
		want   string
	}{
		"success: codex namespace dir": {
			derive: func() (string, error) { return paths.CodexNamespaceDir("user-abc", "acct-123") },
			want:   filepath.Join(paths.ConfigDir(), "codex", "user-abc", "acct-123"),
		},
		"success: codex lock path": {
			derive: func() (string, error) { return paths.CodexLockPath("user-abc", "acct-123") },
			want:   filepath.Join(paths.ConfigDir(), "codex", ".locks", "user-abc+acct-123.lock"),
		},
		"success: codex refresh state path": {
			derive: func() (string, error) { return paths.CodexRefreshStatePath("user-abc", "acct-123") },
			want:   filepath.Join(paths.ConfigDir(), "codex", ".state", "user-abc+acct-123.refresh"),
		},
	}
	for name, tt := range valid {
		t.Run(name, func(t *testing.T) {
			got, err := tt.derive()
			if err != nil {
				t.Fatalf("derivation failed: %v", err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("path mismatch (-want +got):\n%s", diff)
			}
		})
	}

	bad := [][2]string{
		{"../x", "a"},
		{"a", "b/c"},
		{".", "a"},
		{"a", ".."},
		{".locks", "a"},
		{"a", ".scratch"},
		{".state", "a"},
		{"", "a"},
		{"a", "b+c"},
		{"a\x00", "b"},
	}
	for _, pair := range bad {
		user, acct := pair[0], pair[1]
		if _, err := paths.CodexNamespaceDir(user, acct); err == nil {
			t.Errorf("CodexNamespaceDir(%q, %q) should be refused", user, acct)
		}
		if _, err := paths.CodexLockPath(user, acct); err == nil {
			t.Errorf("CodexLockPath(%q, %q) should be refused", user, acct)
		}
		if _, err := paths.CodexRefreshStatePath(user, acct); err == nil {
			t.Errorf("CodexRefreshStatePath(%q, %q) should be refused", user, acct)
		}
	}
	if _, err := os.Stat(paths.ConfigDir()); !os.IsNotExist(err) {
		t.Errorf("a refused derivation must create nothing, found %v", err)
	}
}

func TestCodexLockNamesCannotCollideAcrossIDPairs(t *testing.T) {
	t.Parallel()

	paths := NewPaths("/store")
	tests := map[string]struct {
		derive func(user, acct string) (string, error)
	}{
		"success: lock names are unambiguous": {
			derive: paths.CodexLockPath,
		},
		"success: refresh marker names are unambiguous": {
			derive: paths.CodexRefreshStatePath,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			left, err := tt.derive("a.b", "c")
			if err != nil {
				t.Fatalf("derive(a.b, c) = %v", err)
			}
			right, err := tt.derive("a", "b.c")
			if err != nil {
				t.Fatalf("derive(a, b.c) = %v", err)
			}
			if left == right {
				t.Errorf("(%q, %q) and (%q, %q) derive the same path %q", "a.b", "c", "a", "b.c", left)
			}
		})
	}
}

func TestScratchLockCanNeverCollideWithANamespaceLock(t *testing.T) {
	t.Parallel()

	// The scratch lock shares a directory with every namespace lock, so the
	// two naming schemes must be unable to meet: a namespace lock always
	// contains "+", which no valid id may contain, so no pair of ids can
	// spell "scratch.lock".
	paths := NewPaths("/store")
	scratch := paths.CodexScratchLock()
	if got := filepath.Base(scratch); got != "scratch.lock" {
		t.Errorf("scratch lock name = %q, want %q", got, "scratch.lock")
	}
	if strings.Contains(filepath.Base(scratch), "+") {
		t.Errorf("the scratch lock must carry no separator: %q", scratch)
	}
	if got, want := filepath.Dir(scratch), paths.CodexLocksDir(); got != want {
		t.Errorf("the scratch lock lives in %q, want %q", got, want)
	}

	if err := ValidateCodexSegment("scratch.lock"); err != nil {
		t.Errorf("a dot mid-id is legal, so %q is a valid id: %v", "scratch.lock", err)
	}
	if err := ValidateCodexSegment("scratch+lock"); err == nil {
		t.Errorf("`+` must be refused inside an id")
	}

	namespace, err := paths.CodexLockPath("user-1", "acct-1")
	if err != nil {
		t.Fatalf("CodexLockPath(user-1, acct-1) = %v", err)
	}
	if namespace == scratch {
		t.Errorf("a namespace lock equals the scratch lock: %q", namespace)
	}
	if !strings.Contains(filepath.Base(namespace), "+") {
		t.Errorf("a namespace lock always carries the separator: %q", namespace)
	}
}

func TestEnsureDirsCreatesEveryLevelAt0700(t *testing.T) {
	t.Parallel()

	paths := NewPaths(filepath.Join(t.TempDir(), "nested", "store"))
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}

	for _, dir := range []string{paths.ConfigDir(), paths.NamespaceRoot(), paths.LocksDir(), paths.CacheRoot(), paths.CacheDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%q) = %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("%q should be a directory", dir)
		}
		if mode := info.Mode().Perm(); mode != DirMode {
			t.Errorf("%q has mode %o, want %o", dir, mode, DirMode)
		}
	}

	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Errorf("EnsureDirs() must be idempotent: %v", err)
	}
	if _, err := os.Stat(paths.CodexRoot()); !os.IsNotExist(err) {
		t.Errorf("no codex/ from a Claude command, found %v", err)
	}
	if _, err := os.Stat(paths.CodexCacheDir()); !os.IsNotExist(err) {
		t.Errorf("no cache/codex from a Claude command, found %v", err)
	}
}

func TestEnsureCodexDirsCreatesOnlyTheCodexTreeAt0700(t *testing.T) {
	t.Parallel()

	paths := NewPaths(filepath.Join(t.TempDir(), "agctl"))
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Fatalf("EnsureCodexDirs() = %v", err)
	}

	for _, dir := range []string{paths.ConfigDir(), paths.CodexRoot(), paths.CodexLocksDir(), paths.CodexStateDir(), paths.CodexScratchRoot(), paths.CacheRoot(), paths.CodexCacheDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%q) = %v", dir, err)
		}
		if mode := info.Mode().Perm(); mode != DirMode {
			t.Errorf("%q has mode %o, want %o", dir, mode, DirMode)
		}
	}

	if _, err := os.Stat(paths.NamespaceRoot()); !os.IsNotExist(err) {
		t.Errorf("no claude/ from the Codex tree, found %v", err)
	}
	if _, err := os.Stat(paths.CacheDir()); !os.IsNotExist(err) {
		t.Errorf("no cache/claude from the Codex tree, found %v", err)
	}
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Errorf("EnsureCodexDirs() must be idempotent: %v", err)
	}
}

func TestEnsureDirsKeepsAnExistingDirectorysMode(t *testing.T) {
	t.Parallel()

	paths := NewPaths(filepath.Join(t.TempDir(), "store"))
	if err := os.MkdirAll(paths.ConfigDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) = %v", paths.ConfigDir(), err)
	}
	if err := os.Chmod(paths.ConfigDir(), 0o755); err != nil {
		t.Fatalf("Chmod(%q) = %v", paths.ConfigDir(), err)
	}
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}

	info, err := os.Stat(paths.ConfigDir())
	if err != nil {
		t.Fatalf("Stat(%q) = %v", paths.ConfigDir(), err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Errorf("an existing mode must not be repaired: got %o, want %o", mode, 0o755)
	}
	for _, dir := range []string{paths.NamespaceRoot(), paths.LocksDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%q) = %v", dir, err)
		}
		if mode := info.Mode().Perm(); mode != DirMode {
			t.Errorf("a new level still gets %o: %q has %o", DirMode, dir, mode)
		}
	}
}

func TestSessionRootIsOutsideNamespaceRoot(t *testing.T) {
	t.Parallel()

	paths := NewPaths("/store")
	if strings.HasPrefix(paths.SessionRoot(), paths.NamespaceRoot()+"/") {
		t.Errorf("the session root %q must not live under the namespace root %q", paths.SessionRoot(), paths.NamespaceRoot())
	}
	if strings.HasPrefix(paths.NamespaceRoot(), paths.SessionRoot()+"/") {
		t.Errorf("the namespace root %q must not live under the session root %q", paths.NamespaceRoot(), paths.SessionRoot())
	}
}

func TestIsUnderNamespaceRootRejectsEscapes(t *testing.T) {
	t.Parallel()

	paths := NewPaths("/store")
	root := paths.NamespaceRoot()

	tests := map[string]struct {
		path string
		want bool
	}{
		"success: a credential file inside a namespace": {
			path: filepath.Join(root, "acct", "org", ".credentials.json"),
			want: true,
		},
		"success: a first-level child": {
			path: filepath.Join(root, "acct"),
			want: true,
		},
		"success: dot and dot-dot fold away when they stay inside": {
			path: "/store/claude/./acct/../acct/org/.credentials.json",
			want: true,
		},
		"error: the root itself is not under the root": {
			path: root,
			want: false,
		},
		"error: a sibling of the root": {
			path: "/store/config.json",
			want: false,
		},
		"error: a dot-dot escape to a sibling": {
			path: "/store/claude/../config.json",
			want: false,
		},
		"error: a dot-dot escape out of the store": {
			path: "/store/claude/acct/../../../etc/passwd",
			want: false,
		},
		"error: an unrelated absolute path": {
			path: "/etc/passwd",
			want: false,
		},
		"error: a relative spelling": {
			path: "claude/acct/org/.credentials.json",
			want: false,
		},
		"error: a prefix that is not a component boundary": {
			path: "/store/claudex/acct",
			want: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := paths.IsUnderNamespaceRoot(tt.path); got != tt.want {
				t.Errorf("IsUnderNamespaceRoot(%q) = %t, want %t", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsUnderSessionRootRejectsEscapes(t *testing.T) {
	t.Parallel()

	paths := NewPaths("/store")
	root := paths.SessionRoot()

	tests := map[string]struct {
		path string
		want bool
	}{
		"success: a file inside a session": {
			path: filepath.Join(root, "acct", "org", "mcp.json"),
			want: true,
		},
		"success: a first-level child": {
			path: filepath.Join(root, "acct"),
			want: true,
		},
		"success: dot and dot-dot fold away when they stay inside": {
			path: "/store/claude-sessions/./acct/../acct/org/mcp.json",
			want: true,
		},
		"error: the root itself": {
			path: root,
			want: false,
		},
		"error: a dot-dot escape to a sibling": {
			path: "/store/claude-sessions/../config.json",
			want: false,
		},
		"error: a dot-dot escape out of the store": {
			path: "/store/claude-sessions/acct/../../../etc/passwd",
			want: false,
		},
		"error: a namespace path": {
			path: filepath.Join(paths.NamespaceRoot(), "acct", "org"),
			want: false,
		},
		"error: a relative spelling": {
			path: "claude-sessions/acct/org/mcp.json",
			want: false,
		},
		"error: a prefix that is not a component boundary": {
			path: "/store/claude-sessionsx/acct",
			want: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := paths.IsUnderSessionRoot(tt.path); got != tt.want {
				t.Errorf("IsUnderSessionRoot(%q) = %t, want %t", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsUnderCodexRootIsLexicalAndStrict(t *testing.T) {
	t.Parallel()

	paths := NewPaths("/store")
	root := paths.CodexRoot()

	tests := map[string]struct {
		path string
		want bool
	}{
		"success: an auth file inside a namespace": {
			path: filepath.Join(root, "user", "acct", "auth.json"),
			want: true,
		},
		"success: a lock file": {
			path: filepath.Join(root, ".locks", "u+a.lock"),
			want: true,
		},
		"success: dot and dot-dot fold away when they stay inside": {
			path: "/store/codex/./user/../user/acct/auth.json",
			want: true,
		},
		"error: the root itself": {
			path: root,
			want: false,
		},
		"error: a claude namespace path": {
			path: "/store/claude/acct/org/.credentials.json",
			want: false,
		},
		"error: a dot-dot escape to the claude tree": {
			path: "/store/codex/../claude/acct",
			want: false,
		},
		"error: a prefix that is not a component boundary": {
			path: "/store/codexx/user",
			want: false,
		},
		"error: a relative spelling": {
			path: "codex/user/acct/auth.json",
			want: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := paths.IsUnderCodexRoot(tt.path); got != tt.want {
				t.Errorf("IsUnderCodexRoot(%q) = %t, want %t", tt.path, got, tt.want)
			}
		})
	}

	if paths.IsUnderNamespaceRoot(filepath.Join(root, "user", "acct", "auth.json")) {
		t.Errorf("the Claude check must refuse a Codex path")
	}
}

func TestValidateSegment(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		segment string
		wantErr bool
	}{
		"success: a uuid":                   {segment: "11111111-1111-4111-8111-111111111111"},
		"success: the unknown organization": {segment: UnknownOrg},
		"success: dots underscores dashes":  {segment: "a.b_c-1"},
		"error: empty":                      {segment: "", wantErr: true},
		"error: dot":                        {segment: ".", wantErr: true},
		"error: dot dot":                    {segment: "..", wantErr: true},
		"error: a separator":                {segment: "a/b", wantErr: true},
		"error: a backslash":                {segment: "a\\b", wantErr: true},
		"error: a space":                    {segment: "a b", wantErr: true},
		"error: a nul byte":                 {segment: "a\x00b", wantErr: true},
		"error: a traversal":                {segment: "../x", wantErr: true},
		"error: a non-ascii rune":           {segment: "café", wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := ValidateSegment(tt.segment)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateSegment(%q) = nil, want an error", tt.segment)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateSegment(%q) = %v", tt.segment, err)
			}
		})
	}
}

func TestValidateCodexSegment(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		segment     string
		wantErr     bool
		wantMessage string
	}{
		"success: mixed case and digits": {segment: "user-AbC_123"},
		"success: a uuid":                {segment: "11111111-1111-4111-8111-111111111111"},
		"success: a dot mid-id":          {segment: "a.b"},
		"error: a leading dot is reserved": {
			segment:     ".locks",
			wantErr:     true,
			wantMessage: "begins with `.`",
		},
		"error: the plain alphabet still applies": {
			segment: "a+b",
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := ValidateCodexSegment(tt.segment)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateCodexSegment(%q) = nil, want an error", tt.segment)
				}
				if tt.wantMessage != "" && !strings.Contains(err.Error(), tt.wantMessage) {
					t.Errorf("error %q should name the reason %q", err, tt.wantMessage)
				}
				return
			}
			if err != nil {
				t.Errorf("ValidateCodexSegment(%q) = %v", tt.segment, err)
			}
		})
	}
}

func TestIsSingleComponent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name string
		want bool
	}{
		"success: a file name":        {name: "auth.json", want: true},
		"success: a dotfile":          {name: ".credentials.json", want: true},
		"success: a lock name":        {name: "a+b.lock", want: true},
		"success: three dots":         {name: "...", want: true},
		"error: empty":                {name: "", want: false},
		"error: dot":                  {name: ".", want: false},
		"error: dot dot":              {name: "..", want: false},
		"error: a separator":          {name: "a/b", want: false},
		"error: a traversal":          {name: "../a", want: false},
		"error: a trailing separator": {name: "a/", want: false},
		"error: an absolute spelling": {name: "/a", want: false},
		"error: a current-dir prefix": {name: "./a", want: false},
		"error: a current-dir suffix": {name: "a/.", want: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := IsSingleComponent(tt.name); got != tt.want {
				t.Errorf("IsSingleComponent(%q) = %t, want %t", tt.name, got, tt.want)
			}
		})
	}
}

func TestConfigDirEnvName(t *testing.T) {
	t.Parallel()

	if diff := gocmp.Diff("AGENTCTL_CONFIG_DIR", ConfigDirEnv); diff != "" {
		t.Errorf("environment variable name mismatch (-want +got):\n%s", diff)
	}
}
