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
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestAllowedLoginEnv(t *testing.T) {
	tests := map[string]struct{ parent, want []string }{
		"success: exact allowlist drops decoys": {[]string{"HOME=/home", "PATH=/bin", "TMPDIR=/tmp", "LANG=C", "TERM=xterm", "AWS_SECRET_ACCESS_KEY=decoy", "CODEX_API_KEY=decoy", "USER=user", "LOGNAME=user", "SHELL=/bin/sh", "CODEX_HOME=/live"}, []string{"HOME=/home", "PATH=/bin", "TMPDIR=/tmp", "LANG=C", "TERM=xterm", "CODEX_HOME=/scratch"}},
		"success: ordinary values stay exact":   {[]string{"HOME=/AGENTCTL_FAKE_CODEX_home", "LANG=AGENTCTL_FAKE_CODEX_locale"}, []string{"HOME=/AGENTCTL_FAKE_CODEX_home", "LANG=AGENTCTL_FAKE_CODEX_locale", "CODEX_HOME=/scratch"}},
		"success: locale prefix only":           {[]string{"LC_ALL=C", "LC_CTYPE=C", "LC_=no", "LCD_BRIGHTNESS=40", "MY_LC_THING=no"}, []string{"LC_ALL=C", "LC_CTYPE=C", "CODEX_HOME=/scratch"}},
		"success: proxies preserve exact cases": {[]string{"HTTP_PROXY=upper", "http_proxy=lower", "HTTPS_PROXY=upper", "https_proxy=lower", "ALL_PROXY=upper", "all_proxy=lower", "NO_PROXY=upper", "no_proxy=lower", "SSL_CERT_FILE=/cert", "SSL_CERT_DIR=/certs", "Http_Proxy=no"}, []string{"HTTP_PROXY=upper", "http_proxy=lower", "HTTPS_PROXY=upper", "https_proxy=lower", "ALL_PROXY=upper", "all_proxy=lower", "NO_PROXY=upper", "no_proxy=lower", "SSL_CERT_FILE=/cert", "SSL_CERT_DIR=/certs", "CODEX_HOME=/scratch"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, AllowedLoginEnv(tt.parent, "/scratch")); diff != "" {
				t.Fatalf("child environment (-want +got):\n%s", diff)
			}
		})
	}
}

func newTestScratch(t *testing.T) *LoginScratch {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := OpenScratchRoot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := NewLoginScratch(t.Context(), fd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scratch.Discard(nil) })
	return scratch
}

func TestScratchRootAndNames(t *testing.T) {
	tests := map[string]struct {
		uid      uint32
		mode     uint32
		expected string
	}{
		"success: owned private root": {10, 0o40700, ""},
		"error: foreign owner":        {11, 0o40700, "is owned by another user"},
		"error: permissive root":      {10, 0o40755, "is mode 0755, which lets other users in (it must be 0700)"},
		"error: group bit":            {10, 0o40740, "is mode 0740, which lets other users in (it must be 0700)"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := scratchRootVerdict(tt.uid, tt.mode, 10); got != tt.expected {
				t.Fatalf("root verdict = %q, want %q", got, tt.expected)
			}
		})
	}
	for _, name := range []string{ScratchPrefix + "0000dead", ScratchPrefix + "0badf00d"} {
		if !IsScratchName(name) {
			t.Fatalf("scratch name rejected: %s", name)
		}
	}
	for _, name := range []string{ScratchPrefix + "NOTHEX00", ScratchPrefix + "abc", ScratchPrefix + "0000dead-extra", "my-notes"} {
		if IsScratchName(name) {
			t.Fatalf("foreign name accepted: %s", name)
		}
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenScratchRoot(t.Context(), root); err == nil {
		t.Fatal("permissive root accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenScratchRoot(t.Context(), link); err == nil {
		t.Fatal("symlinked root accepted")
	}
}

func TestLoginScratchCleanup(t *testing.T) {
	tests := map[string]struct {
		setup func(*testing.T, *LoginScratch, string)
	}{
		"success: normal residue and nested symlink": {func(t *testing.T, s *LoginScratch, live string) {
			if err := os.MkdirAll(filepath.Join(s.Path(), "tmp/arg0"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(live, filepath.Join(s.Path(), "tmp/arg0/link")); err != nil {
				t.Fatal(err)
			}
		}},
		"success: sealed leaf": {func(t *testing.T, s *LoginScratch, _ string) {
			if err := os.Chmod(s.Path(), 0o500); err != nil {
				t.Fatal(err)
			}
		}},
		"success: unreadable nested directory": {func(t *testing.T, s *LoginScratch, _ string) {
			hidden := filepath.Join(s.Path(), "hidden")
			if err := os.Mkdir(hidden, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hidden, "inside"), []byte("residue"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(hidden, 0); err != nil {
				t.Fatal(err)
			}
		}},
		"success: credential directory": {func(t *testing.T, s *LoginScratch, _ string) {
			if err := os.Remove(filepath.Join(s.Path(), "auth.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(s.Path(), "auth.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := newTestScratch(t)
			live := t.TempDir()
			precious := filepath.Join(live, "auth.json")
			if err := os.WriteFile(precious, []byte("live"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.Path(), "auth.json"), []byte("scratch"), 0o600); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, s, live)
			var out bytes.Buffer
			s.Discard(&out)
			if _, err := os.Lstat(s.Path()); !os.IsNotExist(err) {
				t.Fatalf("scratch remains: %v", err)
			}
			got, err := os.ReadFile(precious)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "live" {
				t.Fatal("cleanup changed live credential")
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected cleanup report: %s", out.String())
			}
		})
	}
}

func TestScratchReplacedBySymlink(t *testing.T) {
	s := newTestScratch(t)
	live := t.TempDir()
	precious := filepath.Join(live, "auth.json")
	if err := os.WriteFile(precious, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(s.Path(), s.Path()+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(live, s.Path()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.Discard(&out)
	got, err := os.ReadFile(precious)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "live" {
		t.Fatal("cleanup followed replacement symlink")
	}
	if !strings.Contains(out.String(), "remove it by hand") {
		t.Fatalf("replacement leaf removal failure not reported: %s", out.String())
	}
}

func TestSurveyLoginScratch(t *testing.T) {
	tests := map[string]struct {
		kind                         string
		held, odd, truncated, daemon bool
	}{
		"success: normal lock residue":        {kind: "regular"},
		"error: exclusive holder":             {kind: "held", held: true},
		"error: shared holder":                {kind: "shared", held: true},
		"error: fifo is odd not held":         {kind: "fifo", odd: true},
		"error: directory lock not descended": {kind: "directory", odd: true},
		"success: symlink lock ignored":       {kind: "symlink"},
		"error: unreadable lock incomplete":   {kind: "unreadable", truncated: true},
		"error: daemon directory":             {kind: "daemon", daemon: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			lock := filepath.Join(root, ".lock")
			switch tt.kind {
			case "directory":
				if err := os.Mkdir(lock, 0o700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(lock, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("/missing", lock); err != nil {
					t.Fatal(err)
				}
			case "daemon":
				if err := os.Mkdir(filepath.Join(root, "app-server-daemon"), 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(lock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if tt.kind == "unreadable" {
					if err := os.Chmod(lock, 0); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = os.Chmod(lock, 0o600) }()
				}
				if tt.held {
					file, err := os.Open(lock)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = file.Close() }()
					mode := unix.LOCK_EX
					if tt.kind == "shared" {
						mode = unix.LOCK_SH
					}
					if err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := SurveyLoginScratch(t.Context(), root)
			if (len(got.HeldLocks) > 0) != tt.held || (len(got.OddLocks) > 0) != tt.odd || got.Truncated != tt.truncated || got.DaemonDir != tt.daemon {
				t.Fatalf("survey = %+v, expected held=%v odd=%v truncated=%v daemon=%v", got, tt.held, tt.odd, tt.truncated, tt.daemon)
			}
		})
	}
}

func TestSurveyBounds(t *testing.T) {
	tests := map[string]struct{ entries, depth int }{"error: entry bound": {entries: 4097}, "error: depth bound": {depth: 10}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			p := root
			for range tt.depth {
				p = filepath.Join(p, "d")
			}
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatal(err)
			}
			for i := range tt.entries {
				if err := os.WriteFile(filepath.Join(p, time.Unix(int64(i), 0).Format("150405")), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if !SurveyLoginScratch(t.Context(), root).Truncated {
				t.Fatal("incomplete survey claimed clean")
			}
		})
	}
}

func TestSweepLoginScratch(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		age       time.Duration
		directory bool
		removed   bool
	}{
		ScratchPrefix + "0000dead": {16 * time.Minute, true, true},
		ScratchPrefix + "0000beef": {9 * time.Minute, true, false},
		ScratchPrefix + "NOTHEX00": {time.Hour, true, false},
		ScratchPrefix + "0000abcd": {time.Hour, false, false},
		"my-notes":                 {time.Hour, true, false},
	}
	for name, tt := range tests {
		p := filepath.Join(root, name)
		if tt.directory {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-tt.age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := OpenScratchRoot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()
	var out bytes.Buffer
	SweepLoginScratch(t.Context(), fd, 15*time.Minute, &out)
	for name, tt := range tests {
		_, err := os.Lstat(filepath.Join(root, name))
		if os.IsNotExist(err) != tt.removed {
			t.Fatalf("%s removed=%v, expected %v", name, os.IsNotExist(err), tt.removed)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected sweep report: %s", out.String())
	}
	names, err := scratchEntries(int(fd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names, ScratchPrefix+"0000dead") {
		t.Fatal("aged scratch still listed")
	}
}
