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

// Package testutil is the shared test harness: one isolated binary per test.
//
// Everything here exists to make one promise cheap to keep: no test ever
// reaches the developer's real home directory, keychain or vendor account.
// Every command the harness builds is cut off from all three at once:
//
//   - the configuration directory and HOME point into a temporary directory;
//   - CLAUDE_CONFIG_DIR, CLAUDE_SECURESTORAGE_CONFIG_DIR,
//     CLAUDE_CODE_OAUTH_TOKEN and CODEX_HOME are removed, so an inherited
//     value cannot point the binary back at the real machine;
//   - the endpoint overrides default to an unroutable loopback port, so a
//     regression that fetched anyway fails loudly here rather than quietly
//     reaching a vendor;
//   - the keychain is either disabled outright or replaced by the fake
//     security script, never /usr/bin/security.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// repoRoot resolves the repository root from this file's compiled-in path,
// so helpers find testdata and fixtures regardless of the working directory
// a test - or a re-executed test binary - runs with.
var repoRoot = sync.OnceValues(func() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrNotExist
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", err
	}
	return root, nil
})

// RepoRoot returns the repository root directory.
func RepoRoot(tb testing.TB) string {
	tb.Helper()
	root, err := repoRoot()
	if err != nil {
		tb.Fatalf("resolve repository root: %v", err)
	}
	return root
}
