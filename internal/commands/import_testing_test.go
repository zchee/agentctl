//go:build agentctl_testing

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
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestImportRun(t *testing.T) {
	tests := map[string]struct {
		dryRun    bool
		mode      string
		wantError bool
	}{
		"success: registry only and idempotent": {},
		"success: dry run never creates store":  {dryRun: true},
		"error: locked keychain":                {mode: "locked", wantError: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := testutil.New(t).WithKeychain()
			const service = "Claude Code-credentials-12345678"
			f.KeychainItem(service, `{"claudeAiOauth":{"accessToken":"sk-ant-test-only","expiresAt":4102444800000,"tokenAccount":{"uuid":"account","organizationUuid":"org"}}}`).Dump(service)
			for _, pair := range f.Environ() {
				key, value, _ := strings.Cut(pair, "=")
				t.Setenv(key, value)
			}
			t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
			if tt.mode != "" {
				t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT", "1")
				t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_STDERR", "User interaction is not allowed.")
			}
			paths, err := config.Resolve(f.Scratch("new-store"))
			if err != nil {
				t.Fatal(err)
			}
			env := claude.EnvWithHome(f.Home())
			var out bytes.Buffer
			im := Import{Paths: paths, Reader: secret.NewReader(), Env: &env, Out: &out}
			opts := cli.ClaudeImportOptions{From: cli.ImportSourceKeychain, DryRun: tt.dryRun}
			before := f.KeychainItems()
			err = im.Run(t.Context(), opts)
			if (err != nil) != tt.wantError {
				t.Fatalf("Run error = %v, wantError = %v", err, tt.wantError)
			}
			if tt.dryRun || tt.wantError {
				if _, err := os.Stat(paths.ConfigFile()); !os.IsNotExist(err) {
					t.Fatalf("config unexpectedly exists: %v", err)
				}
			} else {
				first, err := os.ReadFile(paths.ConfigFile())
				if err != nil {
					t.Fatal(err)
				}
				out.Reset()
				if err := im.Run(t.Context(), opts); err != nil {
					t.Fatal(err)
				}
				second, err := os.ReadFile(paths.ConfigFile())
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(string(first), string(second)); diff != "" {
					t.Fatalf("repeat changed registry:\n%s", diff)
				}
				if !strings.Contains(out.String(), "imported 0") {
					t.Fatalf("repeat output: %s", out.String())
				}
			}
			f.AssertKeychainReadOnly()
			if diff := gocmp.Diff(before, f.KeychainItems()); diff != "" {
				t.Fatalf("keychain changed:\n%s", diff)
			}
			if strings.Contains(out.String(), "sk-ant") {
				t.Fatal("output exposed credential")
			}
		})
	}
}
