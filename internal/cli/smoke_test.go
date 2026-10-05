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

package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// executeCapturing runs args on a fresh, handlerless tree and returns the
// captured output stream together with the execution error, so the help
// and version surfaces can be checked the way a user meets them.
func executeCapturing(t *testing.T, args ...string) (string, error) {
	t.Helper()

	c := New(Handlers{})
	root := c.Root()
	root.SetArgs(args)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

func TestHelpExitsZero(t *testing.T) {
	t.Parallel()

	out, err := executeCapturing(t, "--help")
	if err != nil {
		t.Fatalf("--help must succeed: %v", err)
	}
	if out == "" {
		t.Fatal("--help must print the usage")
	}
}

func TestVersionExitsZero(t *testing.T) {
	t.Parallel()

	out, err := executeCapturing(t, "--version")
	if err != nil {
		t.Fatalf("--version must succeed: %v", err)
	}
	if !strings.Contains(out, "agentctl") {
		t.Fatalf("--version must name the binary: %q", out)
	}
}

func TestTopLevelHelpListsTheCommandGroups(t *testing.T) {
	t.Parallel()

	out, err := executeCapturing(t, "--help")
	if err != nil {
		t.Fatalf("--help must succeed: %v", err)
	}
	for _, command := range []string{"claude", "codex", "completions", "--config-dir"} {
		if !strings.Contains(out, command) {
			t.Fatalf("top-level help misses %q:\n%s", command, out)
		}
	}
}

func TestClaudeHelpListsEverySubcommand(t *testing.T) {
	t.Parallel()

	out, err := executeCapturing(t, "claude", "--help")
	if err != nil {
		t.Fatalf("claude --help must succeed: %v", err)
	}
	for _, command := range []string{"status", "watch", "login", "accounts", "import", "doctor", "use", "exec", "env"} {
		if !strings.Contains(out, command) {
			t.Fatalf("claude help misses %q:\n%s", command, out)
		}
	}
}

func TestCodexHelpListsEverySubcommand(t *testing.T) {
	t.Parallel()

	out, err := executeCapturing(t, "codex", "--help")
	if err != nil {
		t.Fatalf("codex --help must succeed: %v", err)
	}
	for _, command := range []string{"status", "watch", "login", "accounts", "import", "doctor"} {
		if !strings.Contains(out, command) {
			t.Fatalf("codex help misses %q:\n%s", command, out)
		}
	}
}

func TestWatchRefusesAnIntervalBelowThePollingFloor(t *testing.T) {
	t.Parallel()

	// The polling floor is enforced by the parser, before the process
	// goes anywhere near raw mode, so the refusal is a usage error.
	message := mustReject(t, "claude", "watch", "--interval", "30s")
	if !strings.Contains(message, "60s floor") {
		t.Fatalf("the refusal must name the 60s floor: %s", message)
	}
}

func TestAnUnknownSubcommandIsRejected(t *testing.T) {
	t.Parallel()

	mustReject(t, "claude", "bogus")
}
