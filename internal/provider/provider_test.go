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

package provider

import (
	"strings"
	"testing"
)

func TestUserAgentDefaultNamesThisClientAndAVersion(t *testing.T) {
	t.Parallel()

	got := userAgentDefault()
	if got != "agentctl/"+version {
		t.Fatalf("the default agent must be the binary name and the stamped version: %q", got)
	}
	if !strings.HasPrefix(got, "agentctl/") || len(got) == len("agentctl/") {
		t.Fatalf("the header a server sees must name this client and a version: %q", got)
	}
}

func TestEachProviderHasItsOwnUserAgentOverride(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		provider Provider
		want     string
	}{
		"success: claude override variable": {
			provider: Claude,
			want:     "AGENTCTL_CLAUDE_USER_AGENT",
		},
		"success: codex override variable": {
			provider: Codex,
			want:     "AGENTCTL_CODEX_USER_AGENT",
		},
		"success: an unknown provider has no override variable": {
			provider: Provider("unknown"),
			want:     "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := userAgentEnv(tt.provider); got != tt.want {
				t.Fatalf("userAgentEnv(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestAnOverrideThatIsNotAHeaderValueYieldsTheDefault(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		override string
		want     string
	}{
		"success: a plain product token is used as given": {
			override: "codex_cli_rs/0.1",
			want:     "codex_cli_rs/0.1",
		},
		"success: spaces and visible ASCII are a usable agent": {
			override: "Mozilla/5.0 (Macintosh; arm64) agentctl/1.0",
			want:     "Mozilla/5.0 (Macintosh; arm64) agentctl/1.0",
		},
		"error: an empty override yields the default rather than an empty header": {
			override: "",
			want:     userAgentDefault(),
		},
		"error: a whitespace-only override yields the default": {
			override: "   \t ",
			want:     userAgentDefault(),
		},
		"error: a control byte yields the default": {
			override: "agentctl\a",
			want:     userAgentDefault(),
		},
		"error: CR/LF cannot smuggle a second header line": {
			override: "agentctl\r\nX-Injected: 1",
			want:     userAgentDefault(),
		},
		"error: a tab yields the default": {
			override: "agentctl\t1.0",
			want:     userAgentDefault(),
		},
		"error: a DEL byte yields the default": {
			override: "agentctl\x7f",
			want:     userAgentDefault(),
		},
		"error: non-ASCII yields the default": {
			override: "agentctl/é",
			want:     userAgentDefault(),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := userAgentOrDefault(tt.override); got != tt.want {
				t.Fatalf("userAgentOrDefault(%q) = %q, want %q", tt.override, got, tt.want)
			}
		})
	}
}

func TestUserAgentReadsEachProvidersOwnEnvironmentVariable(t *testing.T) {
	tests := map[string]struct {
		envName  string
		envValue string
		provider Provider
		want     string
	}{
		"success: the claude override replaces the claude agent": {
			envName:  ClaudeUserAgentEnv,
			envValue: "claude-cli/2.0.0 (external, cli)",
			provider: Claude,
			want:     "claude-cli/2.0.0 (external, cli)",
		},
		"success: the codex override replaces the codex agent": {
			envName:  CodexUserAgentEnv,
			envValue: "codex_cli_rs/0.1",
			provider: Codex,
			want:     "codex_cli_rs/0.1",
		},
		"success: the claude override does not leak onto codex": {
			envName:  ClaudeUserAgentEnv,
			envValue: "claude-cli/2.0.0",
			provider: Codex,
			want:     userAgentDefault(),
		},
		"error: a blank override falls back to the default": {
			envName:  ClaudeUserAgentEnv,
			envValue: "   ",
			provider: Claude,
			want:     userAgentDefault(),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tt.envName, tt.envValue)
			if got := UserAgent(tt.provider); got != tt.want {
				t.Fatalf("UserAgent(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}
