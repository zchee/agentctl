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

package claude

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/secret"
)

func TestDiscoveryCredentialReplacementAndGrowth(t *testing.T) {
	tests := map[string]struct {
		change func(*testing.T, string)
	}{
		"error: replacement with a symlink is refused": {change: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Rename(path, path+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".saved", path); err != nil {
				t.Fatal(err)
			}
		}},
		"error: a file grown beyond the limit is refused": {change: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Truncate(path, secret.MaxCredentialsBytes+1); err != nil {
				t.Fatal(err)
			}
		}},
		"error: a replaced namespace cannot redirect the read": {change: func(t *testing.T, path string) {
			t.Helper()
			dir := filepath.Dir(path)
			if err := os.Rename(dir, dir+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dir+".saved", dir); err != nil {
				t.Fatal(err)
			}
		}},
		"error: a FIFO replacement is refused without a writer": {change: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _ := testStore(t)
			path := writeCredentialsFile(t, paths, "account", "organization", otherBlob)
			if got := resolveFile(paths, filepath.Dir(path)); got.credentials == nil {
				t.Fatalf("initial credential read = %v", got.outcome)
			}
			tt.change(t, path)
			got := resolveFile(paths, filepath.Dir(path))
			if got.credentials != nil || got.outcome.Kind != secret.OutcomeTransient {
				t.Fatalf("changed file outcome = %v, credentials present = %v", got.outcome, got.credentials != nil)
			}
		})
	}
}
