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
	"os"
	"strings"
	"testing"
)

// generate runs `completions <shell>` on a fresh tree and returns stdout.
func generate(t *testing.T, shell string) string {
	t.Helper()

	c := New(Handlers{})
	root := c.Root()
	root.SetArgs([]string{"completions", shell})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("completions %s: %v", shell, err)
	}
	return out.String()
}

func TestCompletionsEveryShellMentionsTheBinary(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		shell    string
		contains string
	}{
		"success: bash defines the completion entry point": {
			shell:    "bash",
			contains: "agentctl",
		},
		"success: zsh registers the compdef function": {
			shell:    "zsh",
			contains: "agentctl",
		},
		"success: fish completes the command by name": {
			shell:    "fish",
			contains: "complete -c agentctl",
		},
		"success: powershell registers the argument completer": {
			shell:    "powershell",
			contains: "agentctl",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			script := generate(t, tt.shell)
			if script == "" {
				t.Fatalf("completions %s produced no output", tt.shell)
			}
			if !strings.Contains(script, tt.contains) {
				t.Fatalf("completions %s misses %q", tt.shell, tt.contains)
			}
		})
	}
}

func TestCompletionsZshStartsWithTheCompdefHeader(t *testing.T) {
	t.Parallel()

	script := generate(t, "zsh")
	first, _, _ := strings.Cut(script, "\n")
	if first != "#compdef agentctl" {
		t.Fatalf("first line = %q, want %q", first, "#compdef agentctl")
	}
}

func TestCompletionsRejectsAnUnknownShell(t *testing.T) {
	t.Parallel()

	message := mustReject(t, "completions", "tcsh")
	if !strings.Contains(message, "invalid value") {
		t.Fatalf("the rejection should read like a value error: %s", message)
	}
}

func TestCompletionsRejectsElvishNamingTheSupportedShells(t *testing.T) {
	t.Parallel()

	message := mustReject(t, "completions", "elvish")
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		if !strings.Contains(message, shell) {
			t.Fatalf("the rejection must name %s: %s", shell, message)
		}
	}
}

func TestCompletionsRequiresExactlyOneShell(t *testing.T) {
	t.Parallel()

	mustReject(t, "completions")
	mustReject(t, "completions", "bash", "zsh")
}

func TestCompletionsTreatsAClosedReaderAsSuccess(t *testing.T) {
	t.Parallel()

	// A reader that closes early, as `completions bash | head -1` does, is
	// a normal way to consume the script. The pipe's read end is closed
	// before the single write, so the write fails with a broken pipe on
	// the spot; the command must swallow exactly that failure.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("closing the read end: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	c := New(Handlers{})
	root := c.Root()
	root.SetArgs([]string{"completions", "bash"})
	root.SetOut(writer)
	root.SetErr(io.Discard)
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("a closed reader must not fail the run: %v", err)
	}
}
