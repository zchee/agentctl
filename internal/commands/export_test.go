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
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestSessionProcess(t *testing.T) {
	tests := map[string]struct {
		script string
		code   int
	}{
		"success: zero":            {"exit 0", 0},
		"success: exit forwarding": {"exit 37", 37},
		"success: term death":      {"kill -TERM $$", 143},
		"success: hup death":       {"kill -HUP $$", 129},
		"success: int death":       {"kill -INT $$", 130},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			code, err := (SessionProcess{}).Exec(t.Context(), ExportSpec{}, []string{"/bin/sh", "-c", tt.script})
			if err != nil {
				t.Fatal(err)
			}
			if code != tt.code {
				t.Fatalf("exit=%d, want %d", code, tt.code)
			}
		})
	}
}

func TestSessionProcessEnvironmentAndArguments(t *testing.T) {
	tests := map[string]struct {
		name string
		mcp  bool
	}{
		"success: claude gets mcp":           {"claude", true},
		"success: other command gets no mcp": {"other", true},
		"success: claude without mcp":        {"claude", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.name)
			script := "#!/bin/sh\nprintf '%s\\n' \"$CLAUDE_SECURESTORAGE_CONFIG_DIR\" \"$CLAUDE_CONFIG_DIR\" \"${CLAUDE_CODE_OAUTH_TOKEN-unset}\" \"$UNCHANGED_SESSION_TEST\" \"$@\"\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "private")
			t.Setenv("CLAUDE_CONFIG_DIR", "inherited")
			t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "inherited")
			t.Setenv("UNCHANGED_SESSION_TEST", "unchanged")
			spec := ExportSpec{SecureStorageDir: "/namespace", ConfigDir: "/session"}
			if tt.mcp {
				spec.MCPConfig = "/session/mcp.json"
			}
			var out bytes.Buffer
			code, err := (SessionProcess{Out: &out}).Exec(t.Context(), spec, []string{path, "$(printf should-not-expand)", "with spaces"})
			if err != nil || code != 0 {
				t.Fatalf("exit=%d, err=%v", code, err)
			}
			want := "/namespace\n/session\nunset\nunchanged\n$(printf should-not-expand)\nwith spaces\n"
			if tt.name == "claude" && tt.mcp {
				want += "--mcp-config\n/session/mcp.json\n"
			}
			if diff := gocmp.Diff(want, out.String()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestSessionProcessCancellationAndStartFailure(t *testing.T) {
	tests := map[string]struct {
		argv      []string
		cancelled bool
	}{
		"error: empty argv":        {},
		"error: cannot start":      {argv: []string{"/nonexistent/session-executable"}},
		"error: already cancelled": {argv: []string{"/bin/sleep", "30"}, cancelled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelled {
				cancel()
			}
			if _, err := (SessionProcess{}).Exec(ctx, ExportSpec{}, tt.argv); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := (SessionProcess{}).Exec(ctx, ExportSpec{}, []string{"/bin/sleep", "30"}); err == nil {
		t.Fatal("expected cancellation")
	}
}
