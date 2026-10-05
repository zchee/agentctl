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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func writeCodexFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHomeResolution(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	writeCodexFile(t, file, []byte("x"))
	canonical, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		env        Env
		want, kind string
	}{"success: default": {Env{Home: root}, filepath.Join(root, ".codex"), ""}, "success: linked explicit": {Env{CodexHome: link}, canonical, ""}, "error: missing": {Env{CodexHome: filepath.Join(root, "missing")}, "", "missing"}, "error: file": {Env{CodexHome: file}, "", "not_directory"}, "error: no home": {Env{}, "", "no_home"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, err := ResolveHome(tt.env)
			if tt.kind != "" {
				failure, ok := errors.AsType[*HomeError](err)
				if !ok || failure.Kind != tt.kind {
					t.Fatalf("home error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, home.Dir); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestHomeConfig(t *testing.T) {
	tests := map[string]struct {
		fixture string
		store   StoreMode
		note    *ConfigNote
	}{"success: missing": {"", StoreFile, nil}, "success: file": {"config-file.toml", StoreFile, nil}, "success: unrelated key": {"config-mcp-key.toml", StoreFile, nil}, "success: keyring": {"config-keyring.toml", StoreKeyring, nil}, "success: auto": {"config-auto.toml", StoreAuto, nil}, "success: ephemeral": {"config-ephemeral.toml", StoreEphemeral, nil}, "error: malformed": {"config-malformed.toml", StoreFile, &ConfigNote{Kind: "unparseable", Line: new(4)}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if tt.fixture != "" {
				writeCodexFile(t, filepath.Join(home, "config.toml"), codexFixture(t, tt.fixture))
			}
			got := LoadConfig(t.Context(), home)
			if diff := gocmp.Diff(tt.store, got.Store); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(tt.note, got.Note); diff != "" {
				t.Fatal(diff)
			}
			if strings.Contains(fmt.Sprintf("%+v", got), "agctl-test-codex-") {
				t.Fatal("configuration exposed a key")
			}
			if tt.store == StoreAuto && (got.BaseURL == nil || *got.BaseURL != "https://chatgpt.example.invalid/backend-api/") {
				t.Fatalf("base URL=%v", got.BaseURL)
			}
		})
	}
}

func TestConfigSanitization(t *testing.T) {
	tests := map[string]struct {
		body  string
		store StoreMode
		url   *string
	}{"success: unknown mode": {"cli_auth_credentials_store='secrets'", StoreMode("secrets"), nil}, "success: wrong mode type": {"cli_auth_credentials_store=7", StoreMode("<unrecognised>"), nil}, "success: mode is top level": {"[profiles.x]\ncli_auth_credentials_store='keyring'", StoreFile, nil}, "success: userinfo refused": {"chatgpt_base_url='https://u:secret@h.invalid/'", StoreFile, nil}, "success: scheme refused": {"chatgpt_base_url='file:///etc/passwd'", StoreFile, nil}, "success: endpoint kept": {"chatgpt_base_url='http://127.0.0.1:9/x?token=secret#secret'", StoreFile, new("http://127.0.0.1:9/x")}, "success: empty query removed": {"chatgpt_base_url='https://h.invalid/v1?'", StoreFile, new("https://h.invalid/v1")}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := parseConfig([]byte(tt.body))
			if diff := gocmp.Diff(tt.store, got.Store); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(tt.url, got.BaseURL); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestReadOnlyHomeFiles(t *testing.T) {
	tests := map[string]struct {
		kind    string
		prepare func(*testing.T, string)
	}{"error: binary config": {"unparseable", func(t *testing.T, path string) { writeCodexFile(t, path, []byte{255, 254}) }}, "error: config directory": {"unreadable", func(t *testing.T, path string) {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}}, "error: config fifo": {"unreadable", func(t *testing.T, path string) {
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			tt.prepare(t, filepath.Join(home, "config.toml"))
			got := LoadConfig(t.Context(), home)
			if got.Note == nil || got.Note.Kind != tt.kind {
				t.Fatalf("config=%+v", got)
			}
		})
	}
	home := t.TempDir()
	account := KeyringAccount(home)
	if !IsHomeAccount(account) || len(account) != 20 {
		t.Fatalf("keyring account=%q", account)
	}
	for _, value := range []string{"cli|0123456789abcdef'", "cli|0123456789ABCDEF", "other"} {
		if IsHomeAccount(value) {
			t.Fatalf("accepted %q", value)
		}
	}
	if ReadonlyNoFollow&(unix.O_CREAT|unix.O_WRONLY|unix.O_RDWR|unix.O_TRUNC|unix.O_APPEND|unix.O_EXCL) != 0 {
		t.Fatal("mutable open flags")
	}
	missing := filepath.Join(home, "missing")
	if file, err := OpenReadonlyNoFollow(t.Context(), missing); err == nil {
		_ = file.Close()
		t.Fatal("missing file opened")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created a file")
	}
}

func TestFileInEffect(t *testing.T) {
	tests := map[string]struct {
		mode  StoreMode
		probe KeyringProbe
		reads bool
		calls int
		note  string
	}{"success: file": {StoreFile, KeyringPresent, true, 0, ""}, "success: keyring": {StoreKeyring, KeyringAbsent, false, 0, ""}, "success: ephemeral": {StoreEphemeral, KeyringAbsent, false, 0, ""}, "success: unknown": {StoreMode("x"), KeyringAbsent, false, 0, ""}, "success: auto present": {StoreAuto, KeyringPresent, false, 1, ""}, "success: auto absent": {StoreAuto, KeyringAbsent, true, 1, "auto (file in effect)"}, "success: auto failed probe": {StoreAuto, KeyringUnknown, true, 1, "auto (file in effect)"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			calls := 0
			read, note := FileInEffect(tt.mode, func() KeyringProbe { calls++; return tt.probe })
			if read != tt.reads || calls != tt.calls || note != tt.note {
				t.Fatalf("read=%t calls=%d note=%q", read, calls, note)
			}
		})
	}
}

func TestDaemonEvidence(t *testing.T) {
	tests := map[string]struct {
		body string
		kind DaemonKind
		old  bool
	}{"success: own pid": {fmt.Sprintf(`{"pid":%d,"future":[1,2,3]}`, os.Getpid()), DaemonAlive, false}, "success: zero pid": {`{"pid":0}`, DaemonArtefact, false}, "success: dead pid": {`{"pid":4294967294}`, DaemonArtefact, false}, "success: recycled": {fmt.Sprintf(`{"pid":%d}`, os.Getpid()), DaemonRecycled, true}, "error: torn record": {`{"pid":`, DaemonUnreadable, false}, "error: nonobject": {`12`, DaemonUnreadable, false}, "error: empty": {``, DaemonUnreadable, false}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, leaf := range []string{"app-server.pid", "daemon.pid"} {
				home := t.TempDir()
				path := filepath.Join(home, "app-server-daemon", leaf)
				writeCodexFile(t, path, []byte(tt.body))
				if tt.old {
					at := time.Unix(0, 0)
					if err := os.Chtimes(path, at, at); err != nil {
						t.Fatal(err)
					}
				}
				if got := DaemonEvidence(t.Context(), home); got.Kind != tt.kind {
					t.Fatalf("%s evidence=%+v want %s", leaf, got, tt.kind)
				}
			}
		})
	}
}

func TestDaemonEvidenceStrengthAndWriteOrder(t *testing.T) {
	home := t.TempDir()
	if got := DaemonEvidence(t.Context(), home); got.Kind != DaemonNone {
		t.Fatal(got)
	}
	dir := filepath.Join(home, "app-server-daemon")
	writeCodexFile(t, filepath.Join(dir, "daemon.lock"), nil)
	if got := DaemonEvidence(t.Context(), home); got.Kind != DaemonArtefact {
		t.Fatal(got)
	}
	legacy := filepath.Join(dir, "app-server.pid")
	current := filepath.Join(dir, "daemon.pid")
	writeCodexFile(t, legacy, fmt.Appendf(nil, `{"pid":%d}`, os.Getpid()))
	writeCodexFile(t, current, fmt.Appendf(nil, `{"pid":%d}`, os.Getppid()))
	info, err := os.Stat(legacy)
	if err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(5 * time.Second)
	if err := os.Chtimes(current, later, later); err != nil {
		t.Fatal(err)
	}
	if got := DaemonEvidence(t.Context(), home); got.Kind != DaemonAlive || got.PID != uint32(os.Getppid()) {
		t.Fatal(got)
	}
	writeCodexFile(t, current, []byte("{"))
	if got := DaemonEvidence(t.Context(), home); got.Kind != DaemonAlive {
		t.Fatal(got)
	}
}
