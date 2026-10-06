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

package testutil

import (
	"os/exec"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestCodexFixtureRejectsHomeOverride(t *testing.T) {
	tests := map[string]struct{ value string }{
		"error: empty explicit home":    {},
		"error: explicit external home": {value: "/not-a-fixture-home"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newCodexFixture(t)
			var failure any
			func() { defer func() { failure = recover() }(); fixture.set("CODEX_HOME", test.value) }()
			if failure == nil || !strings.Contains(failure.(string), "CODEX_HOME") {
				t.Fatalf("explicit home was not rejected: %v", failure)
			}
		})
	}
}

func TestCodexFixtureSandboxReachesChild(t *testing.T) {
	tests := map[string]struct{ key, value string }{
		"success: default sandbox":      {},
		"success: safe script variable": {key: "CODEX_FIXTURE_VALUE", value: "from-script"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", "/parent-home")
			t.Setenv("CODEX_HOME", "/parent-codex-home")
			t.Setenv("AGENTCTL_CONFIG_DIR", "/parent-config")
			t.Setenv("AGENTCTL_CODEX_USAGE_URL", "https://example.invalid/real-usage")
			t.Setenv("AGENTCTL_CODEX_TOKEN_URL", "https://example.invalid/real-token")
			fixture := newCodexFixture(t)
			if test.key != "" {
				fixture.set(test.key, test.value)
			}
			child := fixture.apply(exec.CommandContext(t.Context(), "/usr/bin/env"))
			output, err := child.Output()
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"HOME": fixture.inner.Home(), "AGENTCTL_CODEX_BIN": fixture.inner.CodexBin(), "AGENTCTL_CONFIG_DIR": fixture.inner.ConfigDir(), "AGENTCTL_CODEX_USAGE_URL": ClosedEndpoint, "AGENTCTL_CODEX_TOKEN_URL": ClosedEndpoint + "/oauth/token", "AGENTCTL_KEYCHAIN_BACKEND": "none"}
			if test.key != "" {
				want[test.key] = test.value
			}
			got := map[string]string{}
			for line := range strings.SplitSeq(string(output), "\n") {
				key, value, ok := strings.Cut(line, "=")
				if key == "CODEX_HOME" {
					t.Fatal("an explicit Codex home reached the sandboxed child")
				}
				if _, needed := want[key]; ok && needed {
					got[key] = value
				}
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Fatalf("sandbox child environment (-want +got):\n%s", diff)
			}
		})
	}
}
