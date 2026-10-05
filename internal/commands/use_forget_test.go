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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

func TestForgetSession(t *testing.T) {
	tests := map[string]struct {
		shape      string
		yes        bool
		answer     string
		foreign    bool
		wantCode   int
		wantExists bool
		wantText   string
	}{
		"success: removes session only":                  {shape: "directory", yes: true},
		"success: removes symlink only":                  {shape: "symlink", yes: true},
		"success: no session needs no consent":           {shape: "missing", wantText: "nothing to forget"},
		"success: dangling link is absent":               {shape: "dangling", yes: true, wantExists: true, wantText: "nothing to forget"},
		"success: terminal confirmation":                 {shape: "directory", answer: "yes\n"},
		"error: terminal declined":                       {shape: "directory", answer: "n\n", wantCode: 2, wantExists: true, wantText: "cancelled; nothing was removed"},
		"error: piped consent":                           {shape: "directory", wantCode: 2, wantExists: true, wantText: "--yes"},
		"error: regular file is not a session directory": {shape: "file", yes: true, wantCode: 1, wantExists: true, wantText: "could not remove"},
		"error: non-owned account":                       {shape: "directory", yes: true, foreign: true, wantCode: 1, wantExists: true, wantText: "only an account agentctl owns"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, rec, _ := isolateFixture(t)
			path := paths.SessionDir(rec.AccountUUID, rec.OrganizationUUID)
			outside := t.TempDir()
			write := func(path, text string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			preserved := map[string]string{
				filepath.Join(paths.NamespaceDir(rec.AccountUUID, rec.OrganizationUUID), ".credentials.json"): "stored credential",
				paths.LockPath(rec.AccountUUID, rec.OrganizationUUID):                                         "lock inode",
				filepath.Join(outside, "nested", "keep"):                                                      "outside data",
			}
			for path, text := range preserved {
				write(path, text)
			}
			lockBefore, err := os.Stat(paths.LockPath(rec.AccountUUID, rec.OrganizationUUID))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			switch tt.shape {
			case "directory":
				write(filepath.Join(path, ".claude.json"), "session seed")
				if err := os.Symlink(outside, filepath.Join(path, "projects")); err != nil {
					t.Fatal(err)
				}
			case "symlink", "dangling":
				target := outside
				if tt.shape == "dangling" {
					target = filepath.Join(outside, "absent")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "file":
				write(path, "foreign file")
			}
			if tt.foreign {
				rec.Kind = config.AccountKindLive()
			}
			var out bytes.Buffer
			prompt := TerminalPrompt{Out: &out}
			if tt.answer != "" {
				master, slave, err := pty.Open()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
				if _, err := master.WriteString(tt.answer); err != nil {
					t.Fatal(err)
				}
				prompt.In = slave
			}
			err = ForgetSession(t.Context(), paths, rec, prompt, tt.yes)
			if diff := gocmp.Diff(tt.wantCode, errs.ExitCode(err)); diff != "" {
				t.Fatalf("exit code (-want +got): %s; error: %v", diff, err)
			}
			text := out.String()
			if err != nil {
				text += err.Error()
			}
			if !strings.Contains(text, tt.wantText) {
				t.Errorf("output %q does not contain %q", text, tt.wantText)
			}
			_, statErr := os.Lstat(path)
			if diff := gocmp.Diff(tt.wantExists, statErr == nil); diff != "" {
				t.Errorf("session exists (-want +got): %s; stat: %v", diff, statErr)
			}
			for path, want := range preserved {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(want, string(got)); diff != "" {
					t.Errorf("preserved %s (-want +got): %s", path, diff)
				}
			}
			lockAfter, err := os.Stat(paths.LockPath(rec.AccountUUID, rec.OrganizationUUID))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(lockBefore, lockAfter) {
				t.Error("namespace lock inode was replaced")
			}
		})
	}
}
