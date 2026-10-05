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
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestRunUseIsolated(t *testing.T) {
	tests := map[string]struct {
		json, noMCP, newOnly, yes bool
		code                      int
	}{
		"success: default":              {},
		"success: json with mcp":        {json: true},
		"success: json without mcp":     {json: true, noMCP: true},
		"success: explicit new only":    {newOnly: true, yes: true},
		"error: child status preserved": {code: 37},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := testutil.New(t)
			f.WriteRegistry([]any{f.OwnedRecord("account", "org")})
			if err := os.WriteFile(filepath.Join(f.Home(), ".claude.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.BinDir(), "claude"), []byte("#!/bin/sh\nprintf 'launched\\n'\nprintf '%s\\n' \"$@\" > \"$ARG_LOG\"\nexit \"$CHILD_CODE\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", f.Home())
			t.Setenv("PATH", f.BinDir())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("ARG_LOG", f.Scratch("argv"))
			t.Setenv("CHILD_CODE", "0")
			if tt.code != 0 {
				t.Setenv("CHILD_CODE", "37")
			}
			var out bytes.Buffer
			err := (SessionProcess{Out: &out}).RunUse(t.Context(), cli.Globals{ConfigDir: f.ConfigDir()}, cli.ClaudeUseOptions{ID: "account", JSON: tt.json, NoMCP: tt.noMCP, NewOnly: tt.newOnly, Yes: tt.yes})
			if got := errs.ExitCode(err); got != tt.code {
				t.Fatalf("exit=%d, want %d: %v", got, tt.code, err)
			}
			if !strings.HasSuffix(out.String(), "launched\n") {
				t.Fatalf("child output missing: %s", out.String())
			}
			prefix := strings.TrimSuffix(out.String(), "launched\n")
			if tt.json {
				var doc map[string]any
				if err := json.Unmarshal([]byte(prefix), &doc); err != nil {
					t.Fatal(err)
				}
				session := filepath.Join(f.ConfigDir(), "claude-sessions", "account", "org")
				var mcp any
				if !tt.noMCP {
					mcp = filepath.Join(session, "mcp.json")
				}
				want := map[string]any{"securestorage_dir": f.NamespaceDir("account", "org"), "config_dir": session, "session_path": session, "mcp_config": mcp}
				if diff := gocmp.Diff(want, doc); diff != "" {
					t.Fatal(diff)
				}
			} else if prefix != "" {
				t.Fatalf("unsolicited report: %s", prefix)
			}
		})
	}
}

func TestRunUseForgetsBeforeOtherModes(t *testing.T) {
	tests := map[string]struct {
		opts cli.ClaudeUseOptions
	}{
		"success: forget dispatch":         {opts: cli.ClaudeUseOptions{Forget: "account", Yes: true}},
		"success: forget takes precedence": {opts: cli.ClaudeUseOptions{Forget: "account", Yes: true, Live: true, Undo: true, RestartRemoteControl: true}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			fixture.WriteRegistry([]any{fixture.OwnedRecord("account", "org")})
			session := filepath.Join(fixture.ConfigDir(), "claude-sessions", "account", "org")
			if err := os.MkdirAll(session, 0o700); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := (SessionProcess{Out: &out}).RunUse(t.Context(), cli.Globals{ConfigDir: fixture.ConfigDir()}, test.opts); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(session); !os.IsNotExist(err) {
				t.Fatalf("session was not removed: %v", err)
			}
		})
	}
}

func TestRunUseLiveDispatch(t *testing.T) {
	tests := map[string]struct {
		id   string
		want string
	}{
		"error: live missing id": {want: "give it an id"},
		"error: live unknown id": {id: "missing", want: "missing"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			fixture.WriteRegistry([]any{})
			var out, diagnostic bytes.Buffer
			err := (SessionProcess{Out: &out, Err: &diagnostic}).RunUse(t.Context(), cli.Globals{ConfigDir: fixture.ConfigDir()}, cli.ClaudeUseOptions{Live: true, ID: test.id})
			if err == nil || strings.Contains(err.Error(), "not implemented") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unexpected live dispatch error: %v", err)
			}
		})
	}
}

func TestRunUseUndoDispatch(t *testing.T) {
	tests := map[string]struct {
		live bool
	}{
		"success: undo with no entry": {},
		"success: undo before live":   {live: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			fixture.WriteRegistry([]any{})
			var out, diagnostic bytes.Buffer
			err := (SessionProcess{Out: &out, Err: &diagnostic}).RunUse(t.Context(), cli.Globals{ConfigDir: fixture.ConfigDir()}, cli.ClaudeUseOptions{Undo: true, Live: test.live})
			if err != nil || !strings.Contains(out.String(), "there is no swap to undo") {
				t.Fatalf("undo was not dispatched: %v; output=%s", err, out.String())
			}
		})
	}
}

func TestRunUseUnavailableModes(t *testing.T) {
	tests := map[string]struct {
		opts cli.ClaudeUseOptions
		want string
	}{
		"error: missing id":                     {want: "an account id is required"},
		"error: remote control not implemented": {opts: cli.ClaudeUseOptions{RestartRemoteControl: true}, want: "not implemented"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := (SessionProcess{}).RunUse(t.Context(), cli.Globals{}, tt.opts)
			if errs.ExitCode(err) != 1 || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}
