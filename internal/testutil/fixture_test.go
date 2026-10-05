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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestNewIsolatesTheTree(t *testing.T) {
	f := New(t)

	tests := map[string]struct {
		path string
	}{
		"success: config directory exists":         {path: f.ConfigDir()},
		"success: home directory exists":           {path: f.Home()},
		"success: bin directory exists":            {path: f.BinDir()},
		"success: keychain items directory exists": {path: f.ItemsDir()},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			info, err := os.Stat(tt.path)
			if err != nil {
				t.Fatalf("stat %q: %v", tt.path, err)
			}
			if !info.IsDir() {
				t.Fatalf("%q is not a directory", tt.path)
			}
		})
	}
}

func TestNewClosesEveryEndpoint(t *testing.T) {
	f := New(t)

	tests := map[string]struct {
		key  string
		want string
	}{
		"success: usage endpoint closed":          {key: "AGENTCTL_CLAUDE_USAGE_URL", want: ClosedEndpoint},
		"success: token endpoint closed":          {key: "AGENTCTL_CLAUDE_TOKEN_URL", want: ClosedEndpoint + "/token"},
		"success: authorize endpoint closed":      {key: "AGENTCTL_CLAUDE_AUTHORIZE_URL", want: ClosedEndpoint + "/authorize"},
		"success: profile endpoint closed":        {key: "AGENTCTL_CLAUDE_PROFILE_URL", want: ClosedEndpoint + ProfilePath},
		"success: second token endpoint closed":   {key: "AGENTCTL_CODEX_TOKEN_URL", want: ClosedEndpoint + "/oauth/token"},
		"success: second usage endpoint closed":   {key: "AGENTCTL_CODEX_USAGE_URL", want: ClosedEndpoint},
		"success: browser suppressed":             {key: "AGENTCTL_NO_BROWSER", want: "1"},
		"success: keychain disabled by default":   {key: "AGENTCTL_KEYCHAIN_BACKEND", want: "none"},
		"success: user pinned":                    {key: "USER", want: KeychainAccount},
		"success: logname pinned":                 {key: "LOGNAME", want: KeychainAccount},
		"success: home points inside the fixture": {key: "HOME", want: f.Home()},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := f.Lookup(tt.key)
			if !ok {
				t.Fatalf("Lookup(%q) found nothing", tt.key)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("Lookup(%q) mismatch (-want +got):\n%s", tt.key, diff)
			}
		})
	}
}

func TestEnvironScrubsInheritedValues(t *testing.T) {
	tests := map[string]struct {
		key   string
		value string
	}{
		"success: vendor config dir removed":        {key: "CLAUDE_CONFIG_DIR", value: "/elsewhere"},
		"success: vendor secure storage removed":    {key: "CLAUDE_SECURESTORAGE_CONFIG_DIR", value: "/elsewhere"},
		"success: vendor token removed":             {key: "CLAUDE_CODE_OAUTH_TOKEN", value: "sk-test-inherited"},
		"success: second vendor home removed":       {key: "CODEX_HOME", value: "/elsewhere"},
		"success: own config dir removed":           {key: "AGENTCTL_CONFIG_DIR", value: "/elsewhere"},
		"success: fault switch removed":             {key: "AGENTCTL_FAULT", value: "boom"},
		"success: log filter removed":               {key: "AGENTCTL_LOG", value: "debug"},
		"success: fake script knob removed":         {key: "AGCTL_FAKE_SECURITY_SLEEP", value: "9"},
		"success: second fake script knob removed":  {key: "AGCTL_FAKE_CODEX_EXIT", value: "9"},
		"success: inherited user loses to the pin":  {key: "USER", value: "developer"},
		"success: inherited home loses to the tree": {key: "HOME", value: "/Users/developer"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			f := New(t)
			entry := tt.key + "=" + tt.value
			for _, got := range f.Environ() {
				if got == entry {
					t.Fatalf("Environ() leaked the inherited %q", entry)
				}
			}
		})
	}
}

func TestEnvironKeepsUnrelatedVariables(t *testing.T) {
	t.Setenv("UNRELATED_TEST_VARIABLE", "kept")
	f := New(t)
	if slices.Contains(f.Environ(), "UNRELATED_TEST_VARIABLE=kept") {
		return
	}
	t.Fatalf("Environ() dropped a variable the scrub does not name")
}

func TestSetReplacesEarlierValues(t *testing.T) {
	f := New(t)
	f.Set("AGENTCTL_CLAUDE_USAGE_URL", "http://127.0.0.1:1")
	f.Set("AGENTCTL_CLAUDE_USAGE_URL", "http://127.0.0.1:2")

	count := 0
	var last string
	for _, entry := range f.Environ() {
		if value, ok := strings.CutPrefix(entry, "AGENTCTL_CLAUDE_USAGE_URL="); ok {
			count++
			last = value
		}
	}
	if count != 1 || last != "http://127.0.0.1:2" {
		t.Fatalf("Environ() carries %d values, last %q; want exactly one, %q", count, last, "http://127.0.0.1:2")
	}
}

func TestEndpointsPointAtOneBase(t *testing.T) {
	f := New(t)
	f.Endpoints("http://127.0.0.1:4545")
	f.CodexEndpoints("http://127.0.0.1:4546")

	tests := map[string]struct {
		key  string
		want string
	}{
		"success: usage takes the base itself": {key: "AGENTCTL_CLAUDE_USAGE_URL", want: "http://127.0.0.1:4545"},
		"success: token takes its path":        {key: "AGENTCTL_CLAUDE_TOKEN_URL", want: "http://127.0.0.1:4545" + TokenPath},
		"success: authorize takes its path":    {key: "AGENTCTL_CLAUDE_AUTHORIZE_URL", want: "http://127.0.0.1:4545/oauth/authorize"},
		"success: profile takes its path":      {key: "AGENTCTL_CLAUDE_PROFILE_URL", want: "http://127.0.0.1:4545" + ProfilePath},
		"success: second token takes its path": {key: "AGENTCTL_CODEX_TOKEN_URL", want: "http://127.0.0.1:4546/oauth/token"},
		"success: second usage takes the base": {key: "AGENTCTL_CODEX_USAGE_URL", want: "http://127.0.0.1:4546"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, _ := f.Lookup(tt.key)
			if got != tt.want {
				t.Fatalf("Lookup(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestRepoRootHoldsTheModule(t *testing.T) {
	root := RepoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("RepoRoot() = %q holds no go.mod: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "testdata", "golden")); err != nil {
		t.Fatalf("RepoRoot() = %q holds no testdata/golden: %v", root, err)
	}
}
