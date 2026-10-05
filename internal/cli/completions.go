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
	"errors"
	"fmt"
	"slices"
	"syscall"

	"github.com/spf13/cobra"
)

// newCompletionsCmd builds the `completions` command. The script is
// generated from the same command definition the binary parses, so it can
// never drift from the real flag set the way a hand-written script would.
func (c *CLI) newCompletionsCmd() *cobra.Command {
	shells := []string{"bash", "zsh", "fish", "powershell"}
	return &cobra.Command{
		Use:   "completions {bash|zsh|fish|powershell}",
		Short: "Print a shell completion script for agentctl to stdout.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return errors.New("expected exactly one shell name: bash, zsh, fish, or powershell")
			}
			if !slices.Contains(shells, args[0]) {
				return fmt.Errorf("invalid value %q for the shell argument: supported shells are bash, zsh, fish, and powershell", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c.dispatched = true
			return writeCompletions(cmd, args[0])
		},
	}
}

// writeCompletions renders the script for shell into a buffer first and
// copies it to the command's output in one write. The generators have no
// way to distinguish a reader that closed its end early, which is an
// ordinary way to consume a completion script (`agentctl completions zsh |
// head -1`); buffering first means the only fallible write is the one this
// function controls, so a broken pipe can be recognised and treated as a
// normal, silent end of output rather than a failure.
func writeCompletions(cmd *cobra.Command, shell string) error {
	root := cmd.Root()
	var script bytes.Buffer
	var err error
	switch shell {
	case "bash":
		err = root.GenBashCompletionV2(&script, true)
	case "zsh":
		err = root.GenZshCompletion(&script)
	case "fish":
		err = root.GenFishCompletion(&script, true)
	case "powershell":
		err = root.GenPowerShellCompletionWithDesc(&script)
	}
	if err != nil {
		return fmt.Errorf("could not generate the completion script: %w", err)
	}

	if _, err := cmd.OutOrStdout().Write(script.Bytes()); err != nil && !errors.Is(err, syscall.EPIPE) {
		return fmt.Errorf("could not write the completion script: %w", err)
	}
	return nil
}
