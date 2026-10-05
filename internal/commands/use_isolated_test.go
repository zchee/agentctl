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

func TestRunUseUnavailableModes(t *testing.T) {
	tests := map[string]struct {
		opts cli.ClaudeUseOptions
		want string
	}{
		"error: missing id":             {want: "an account id is required"},
		"error: live not implemented":   {opts: cli.ClaudeUseOptions{Live: true}, want: "not implemented"},
		"error: undo not implemented":   {opts: cli.ClaudeUseOptions{Undo: true}, want: "not implemented"},
		"error: forget not implemented": {opts: cli.ClaudeUseOptions{Forget: "account"}, want: "not implemented"},
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
