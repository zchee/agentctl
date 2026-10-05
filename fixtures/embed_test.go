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

package fixtures_test

import (
	"io/fs"
	"testing"

	"github.com/zchee/agentctl/fixtures"
)

// TestFSContents proves the embedded tree is complete: the three fake
// executable scripts resolve, the documents under each subdirectory are
// present, and no file was silently dropped by the embed patterns.
func TestFSContents(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
	}{
		"success: fake security script embedded":  {path: "fake-security.sh"},
		"success: fake codex script embedded":     {path: "fake-codex.sh"},
		"success: fake tmux script embedded":      {path: "fake-tmux.sh"},
		"success: claude usage document embedded": {path: "claude/usage-2026-09-08.json"},
		"success: codex config document embedded": {path: "codex/config-auto.toml"},
		"success: binary capture embedded":        {path: "rc-screens/capture_invalid.bin"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := fixtures.FS.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", tt.path, err)
			}
			if len(data) == 0 {
				t.Fatalf("ReadFile(%q) returned no bytes", tt.path)
			}
		})
	}
}

// TestFSFileCount pins the total number of embedded files so an accidental
// deletion or an embed pattern that stops matching is caught immediately.
func TestFSFileCount(t *testing.T) {
	t.Parallel()

	const want = 41

	got := 0
	if err := fs.WalkDir(fixtures.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			got++
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if got != want {
		t.Fatalf("embedded file count = %d, want %d", got, want)
	}
}
