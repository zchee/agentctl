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
	"os/exec"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
)

func TestRenderEnv(t *testing.T) {
	posix := "export CLAUDE_SECURESTORAGE_CONFIG_DIR='/isolated/namespace'\nexport CLAUDE_CONFIG_DIR='/session'\n# CLAUDE_CODE_OAUTH_TOKEN would bypass this session's stored credential\nunset CLAUDE_CODE_OAUTH_TOKEN"
	fish := "set -gx CLAUDE_SECURESTORAGE_CONFIG_DIR '/isolated/namespace'\nset -gx CLAUDE_CONFIG_DIR '/session'\n# CLAUDE_CODE_OAUTH_TOKEN would bypass this session's stored credential\nset -e CLAUDE_CODE_OAUTH_TOKEN"
	posixMCP := "\nalias claude='claude --mcp-config '\\''/session/mcp.json'\\'''\n# an alias only reaches an interactive shell; a script started from one will not inherit it"
	fishMCP := "\nfunction claude\n    command claude --mcp-config '/session/mcp.json' $argv\nend\n# a fish function only reaches an interactive shell; a script started from one will not inherit it"
	tests := map[string]struct {
		shell     cli.Shell
		mcp, want string
	}{
		"success: bash":             {cli.ShellBash, "/session/mcp.json", posix + posixMCP},
		"success: zsh":              {cli.ShellZsh, "/session/mcp.json", posix + posixMCP},
		"success: fish":             {cli.ShellFish, "/session/mcp.json", fish + fishMCP},
		"success: bash without mcp": {cli.ShellBash, "", posix},
		"success: zsh without mcp":  {cli.ShellZsh, "", posix},
		"success: fish without mcp": {cli.ShellFish, "", fish},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := RenderEnv(ExportSpec{SecureStorageDir: "/isolated/namespace", ConfigDir: "/session", MCPConfig: tt.mcp}, tt.shell)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestSessionExport(t *testing.T) {
	tests := map[string]struct {
		alter func(*config.AccountRecord)
		fail  bool
	}{
		"success: owned":        {alter: func(*config.AccountRecord) {}},
		"error: live":           {alter: func(r *config.AccountRecord) { r.Kind = config.AccountKindLive() }, fail: true},
		"error: empty spelling": {alter: func(r *config.AccountRecord) { r.Kind.Owned.ExportSpelling = "" }, fail: true},
		"error: empty hash":     {alter: func(r *config.AccountRecord) { r.Kind.Owned.ExportSHA8 = "" }, fail: true},
		"error: drifted hash":   {alter: func(r *config.AccountRecord) { r.Kind.Owned.ExportSHA8 = "deadbeef" }, fail: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, rec, _ := isolateFixture(t)
			tt.alter(rec)
			got, err := SessionExport(rec, SessionDir{Path: "/session", MCPConfig: "/session/mcp.json"})
			if (err != nil) != tt.fail {
				t.Fatalf("error=%v, want failure=%v", err, tt.fail)
			}
			if !tt.fail {
				if diff := gocmp.Diff(ExportSpec{SecureStorageDir: rec.Kind.Owned.ExportSpelling, ConfigDir: "/session", MCPConfig: "/session/mcp.json"}, got); diff != "" {
					t.Fatal(diff)
				}
			}
		})
	}
}

func TestEnvQuotingInRealShell(t *testing.T) {
	tests := map[string]struct{ shell string }{"success: bash": {shell: "/bin/bash"}, "success: zsh": {shell: "/bin/zsh"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			spec := ExportSpec{SecureStorageDir: "/path with ' quotes/$(printf expanded)/`printf expanded`", ConfigDir: "/session\\with ' spaces", MCPConfig: "/mcp/$(printf expanded)/`printf expanded`/'/mcp.json"}
			script := "shopt -s expand_aliases 2>/dev/null || true\nfunction claude() { printf '%s\\n' \"$@\"; }\n" + RenderEnv(spec, cli.ShellBash) + "\nprintf '%s\\n' \"$CLAUDE_SECURESTORAGE_CONFIG_DIR\" \"$CLAUDE_CONFIG_DIR\" \"${CLAUDE_CODE_OAUTH_TOKEN-unset}\"\neval 'claude user-argument'\n"
			cmd := exec.CommandContext(t.Context(), tt.shell, "-c", script)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "CLAUDE_CODE_OAUTH_TOKEN=should-be-removed"}
			got, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("shell: %v: %s", err, got)
			}
			want := strings.Join([]string{spec.SecureStorageDir, spec.ConfigDir, "unset", "--mcp-config", spec.MCPConfig, "user-argument", ""}, "\n")
			if diff := gocmp.Diff(want, string(got)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
