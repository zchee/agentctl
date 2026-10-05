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
	"os"
	"strings"
)

// Provider names one of the vendors agentctl reads usage for, where the
// choice decides a path or a label and nothing more, such as a per-vendor
// cache directory.
type Provider string

const (
	// Claude is Anthropic's Claude.
	Claude Provider = "claude"
	// Codex is OpenAI's Codex CLI.
	Codex Provider = "codex"
)

// ClaudeUserAgentEnv is the environment variable that replaces the default
// User-Agent for Anthropic.
//
// Production-visible on purpose: if Anthropic ever starts refusing the
// honest agent, the user can put Claude Code's back without waiting for a
// release.
const ClaudeUserAgentEnv = "AGENTCTL_CLAUDE_USER_AGENT"

// CodexUserAgentEnv is the environment variable that replaces the default
// User-Agent for OpenAI.
const CodexUserAgentEnv = "AGENTCTL_CODEX_USER_AGENT"

// version is the release identifier the build stamps in with the linker's
// -X flag; a local build reports (devel).
var version = "(devel)"

// userAgentDefault is the User-Agent sent when no override is set, for
// every provider.
//
// Honest, not mimicked: a client that lies about who it is cannot be
// rate-limited, deprecated or excluded separately from the product it is
// pretending to be. One value for both providers, because it is one client.
func userAgentDefault() string {
	return "agentctl/" + version
}

// userAgentEnv returns which environment variable overrides one provider's
// User-Agent. An unknown provider has no override variable, so its agent is
// always the default.
func userAgentEnv(p Provider) string {
	switch p {
	case Claude:
		return ClaudeUserAgentEnv
	case Codex:
		return CodexUserAgentEnv
	default:
		return ""
	}
}

// UserAgent returns the User-Agent for one provider, override included.
func UserAgent(p Provider) string {
	return userAgentOrDefault(os.Getenv(userAgentEnv(p)))
}

// userAgentOrDefault is [UserAgent] without the environment, which is the
// whole of its rule, split out so the rule is tested by calling it rather
// than by mutating the process environment.
//
// A blank override yields the default: a blank string would otherwise
// produce a header that some proxies drop and others reject. So does one
// with a byte outside visible ASCII and space, because the HTTP transport
// refuses to send an invalid header value, and an override that fails every
// request is worse than the default it replaced. Refusing CR and LF here
// also keeps an override from smuggling a second header line in.
func userAgentOrDefault(override string) string {
	if strings.TrimSpace(override) == "" {
		return userAgentDefault()
	}
	for i := range len(override) {
		if b := override[i]; b != ' ' && (b < '!' || b > '~') {
			return userAgentDefault()
		}
	}
	return override
}
