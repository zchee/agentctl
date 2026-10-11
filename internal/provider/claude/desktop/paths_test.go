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

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

// realTempDir resolves the temporary directory, because on macOS it sits
// behind the /var symbolic link and the layout refuses link components.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

func TestPaths(t *testing.T) {
	t.Parallel()

	got := Paths("/Users/someone")
	want := Layout{
		Home:        "/Users/someone",
		Root:        "/Users/someone/Library/Application Support/agentctl/claude-desktop",
		ProfilesDir: "/Users/someone/Library/Application Support/agentctl/claude-desktop/profiles",
		Journal:     "/Users/someone/Library/Application Support/agentctl/claude-desktop/journal.jsonl",
		Lock:        "/Users/someone/Library/Application Support/agentctl/claude-desktop/lock",
		DefaultData: "/Users/someone/Library/Application Support/Claude",
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("Paths mismatch (-want +got):\n%s", diff)
	}
	if p := got.Profile("fresh-0123456789abcdef"); p != want.ProfilesDir+"/fresh-0123456789abcdef" {
		t.Fatalf("Profile = %q", p)
	}
	// The desktop store and the data path share one parent, so a rename
	// between them never crosses a file system boundary by construction.
	if filepath.Dir(filepath.Dir(got.Root)) != filepath.Dir(got.DefaultData) {
		t.Fatalf("Root %q and DefaultData %q do not share Application Support", got.Root, got.DefaultData)
	}
}

// TestPathsAreOutsideTheAgentctlStore resolves the store the way every
// command does and proves no desktop path lies under it: two stores must
// see one lock and one journal for the one data path.
func TestPathsAreOutsideTheAgentctlStore(t *testing.T) {
	home := realTempDir(t)
	tests := map[string]struct {
		env map[string]string
		cli string
	}{
		"success: default store under the home directory": {env: map[string]string{"HOME": home}},
		"success: XDG store":                              {env: map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, "xdg")}},
		"success: explicit store under Application Support": {
			env: map[string]string{"HOME": home},
			cli: filepath.Join(home, "Library", "Application Support", "agctl-store"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_CONFIG_DIR", "")
			_ = os.Unsetenv("AGENTCTL_CONFIG_DIR")
			t.Setenv("XDG_CONFIG_HOME", "")
			_ = os.Unsetenv("XDG_CONFIG_HOME")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			store, err := config.Resolve(tt.cli)
			if err != nil {
				t.Fatalf("config.Resolve: %v", err)
			}
			layout := Paths(home)
			for _, p := range []string{layout.Root, layout.ProfilesDir, layout.Journal, layout.Lock, layout.DefaultData} {
				if p == store.ConfigDir() || strings.HasPrefix(p, store.ConfigDir()+string(filepath.Separator)) {
					t.Fatalf("%q lies under the agentctl store %q", p, store.ConfigDir())
				}
			}
		})
	}
}

func TestParseProfileName(t *testing.T) {
	t.Parallel()

	const op = "0123456789abcdef"
	tests := map[string]struct {
		name    string
		want    ProfileName
		wantErr bool
	}{
		"success: account uuid":           {name: "552d914f-1111-2222-3333-444444444444", want: ProfileName{Kind: NameAccount, UUID: "552d914f-1111-2222-3333-444444444444"}},
		"success: fresh outgoing":         {name: "fresh-" + op, want: ProfileName{Kind: NameFresh, OpID: op}},
		"success: fresh parked target":    {name: "fresh-" + op + "-target", want: ProfileName{Kind: NameFreshTarget, OpID: op}},
		"success: unknown outgoing":       {name: "unknown-" + op, want: ProfileName{Kind: NameUnknown, OpID: op}},
		"success: stray":                  {name: "stray-" + op, want: ProfileName{Kind: NameStray, OpID: op}},
		"error: upper-case uuid":          {name: "552D914F-1111-2222-3333-444444444444", wantErr: true},
		"error: short op id":              {name: "fresh-0123", wantErr: true},
		"error: upper-case op id":         {name: "fresh-0123456789ABCDEF", wantErr: true},
		"error: unknown prefix":           {name: "other-" + op, wantErr: true},
		"error: separator":                {name: "fresh-" + op + "/x", wantErr: true},
		"error: parent reference":         {name: "..", wantErr: true},
		"error: empty":                    {name: "", wantErr: true},
		"error: target suffix on unknown": {name: "unknown-" + op + "-target", wantErr: true},
		"error: trailing text after uuid": {name: "552d914f-1111-2222-3333-444444444444x", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseProfileName(tt.name)
			if tt.wantErr {
				if !errors.Is(err, ErrBadProfileName) {
					t.Fatalf("ParseProfileName(%q) = %+v, %v; want ErrBadProfileName", tt.name, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProfileName(%q): %v", tt.name, err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("ParseProfileName(%q) mismatch (-want +got):\n%s", tt.name, diff)
			}
		})
	}
	// Every name this package generates parses back to its kind.
	for gen, kind := range map[string]NameKind{
		AccountName("552D914F-1111-2222-3333-444444444444"): NameAccount,
		FreshName(op):       NameFresh,
		FreshTargetName(op): NameFreshTarget,
		UnknownName(op):     NameUnknown,
		StrayName(op):       NameStray,
	} {
		got, err := ParseProfileName(gen)
		if err != nil || got.Kind != kind {
			t.Fatalf("generated name %q parses as %+v, %v; want kind %s", gen, got, err, kind)
		}
	}
	id, err := NewOpID()
	if err != nil || !ValidOpID(id) {
		t.Fatalf("NewOpID = %q, %v", id, err)
	}
}

func TestParentChainHasNoSymlink(t *testing.T) {
	t.Parallel()

	base := realTempDir(t)
	if err := os.MkdirAll(filepath.Join(base, "real", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real", "inner"), filepath.Join(base, "real", "leaflink")); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		path      string
		component string
		wantErr   bool
	}{
		"success: real chain":                     {path: filepath.Join(base, "real", "inner")},
		"success: missing tail ends the walk":     {path: filepath.Join(base, "real", "missing", "deeper")},
		"error: link in the middle":               {path: filepath.Join(base, "link", "inner"), component: filepath.Join(base, "link"), wantErr: true},
		"error: link as the leaf":                 {path: filepath.Join(base, "real", "leaflink"), component: filepath.Join(base, "real", "leaflink"), wantErr: true},
		"error: relative path":                    {path: "relative/path", wantErr: true},
		"error: link reached through a .. step":   {path: filepath.Join(base, "real", "..", "link"), component: filepath.Join(base, "link"), wantErr: true},
		"success: trailing separator is the same": {path: filepath.Join(base, "real") + "/"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ParentChainHasNoSymlink(tt.path)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ParentChainHasNoSymlink(%q): %v", tt.path, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParentChainHasNoSymlink(%q) = nil, want an error", tt.path)
			}
			if tt.component == "" {
				return
			}
			linkErr, ok := errors.AsType[*SymlinkComponentError](err)
			if !ok || linkErr.Component != tt.component {
				t.Fatalf("ParentChainHasNoSymlink(%q) = %v; want the component %q named", tt.path, err, tt.component)
			}
		})
	}
}

func TestEnsureStore(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		prepare func(t *testing.T, home string)
		wantErr bool
	}{
		"success: creates root and profiles at 0700": {},
		"success: existing store is accepted": {
			prepare: func(t *testing.T, home string) {
				if err := os.MkdirAll(Paths(home).ProfilesDir, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		"success: an existing group- and world-readable store is narrowed to 0700": {
			prepare: func(t *testing.T, home string) {
				l := Paths(home)
				if err := os.MkdirAll(l.ProfilesDir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, dir := range []string{l.Root, l.ProfilesDir} {
					if err := os.Chmod(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		"success: an existing world-writable profiles directory is narrowed to 0700": {
			prepare: func(t *testing.T, home string) {
				l := Paths(home)
				if err := os.MkdirAll(l.ProfilesDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(l.ProfilesDir, 0o777); err != nil {
					t.Fatal(err)
				}
			},
		},
		"error: root is a symbolic link": {
			prepare: func(t *testing.T, home string) {
				l := Paths(home)
				elsewhere := filepath.Join(home, "elsewhere")
				if err := os.MkdirAll(elsewhere, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(l.Root), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, l.Root); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		"error: profiles is a file": {
			prepare: func(t *testing.T, home string) {
				l := Paths(home)
				if err := os.MkdirAll(l.Root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(l.ProfilesDir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := realTempDir(t)
			if tt.prepare != nil {
				tt.prepare(t, home)
			}
			err := Paths(home).EnsureStore()
			if tt.wantErr {
				if err == nil {
					t.Fatal("EnsureStore succeeded; want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureStore: %v", err)
			}
			for _, dir := range []string{Paths(home).Root, Paths(home).ProfilesDir} {
				info, err := os.Lstat(dir)
				if err != nil {
					t.Fatal(err)
				}
				if !info.IsDir() || info.Mode().Perm() != 0o700 {
					t.Fatalf("%s: mode %v, want a 0700 directory", dir, info.Mode())
				}
			}
		})
	}
}
