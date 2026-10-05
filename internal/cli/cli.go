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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// version is the release identifier the build stamps in with the linker's
// -X flag; a local build reports (devel).
var version = "(devel)"

// Globals carries the options every subcommand shares.
type Globals struct {
	// ConfigDir is the configuration directory named by --config-dir, or
	// by AGENTCTL_CONFIG_DIR when the flag is not given. Empty when
	// neither is set, which lets the configuration package apply its own
	// resolution order instead of baking a default in here.
	ConfigDir string
}

// NotImplementedError reports a subcommand that parsed but has no handler
// wired in. A nil Handlers field yields this error on purpose: the command
// surface is reviewed as one piece while the implementations arrive
// independently, and an unwired command must fail loudly with exit status 1
// rather than exit 0 having done nothing.
type NotImplementedError struct {
	// Command is the space-joined command path, such as "claude status".
	Command string
}

// Error names the command so the caller's "agentctl: " prefix completes the
// sentence.
func (e *NotImplementedError) Error() string {
	return e.Command + ": not implemented"
}

// Handlers holds one function per subcommand. The command tree parses the
// command line into the typed options struct and calls the matching field;
// a nil field makes the subcommand fail with a NotImplementedError.
type Handlers struct {
	ClaudeStatus           func(ctx context.Context, globals Globals, opts ClaudeStatusOptions) error
	ClaudeWatch            func(ctx context.Context, globals Globals, opts ClaudeWatchOptions) error
	ClaudeLogin            func(ctx context.Context, globals Globals, opts ClaudeLoginOptions) error
	ClaudeAccountsList     func(ctx context.Context, globals Globals, opts ClaudeAccountsListOptions) error
	ClaudeAccountsShow     func(ctx context.Context, globals Globals, opts ClaudeAccountsShowOptions) error
	ClaudeAccountsRemove   func(ctx context.Context, globals Globals, opts ClaudeAccountsRemoveOptions) error
	ClaudeAccountsRelocate func(ctx context.Context, globals Globals, opts ClaudeAccountsRelocateOptions) error
	ClaudeAccountsForget   func(ctx context.Context, globals Globals, opts ClaudeAccountsForgetOptions) error
	ClaudeAccountsUnforget func(ctx context.Context, globals Globals, opts ClaudeAccountsUnforgetOptions) error
	ClaudeImport           func(ctx context.Context, globals Globals, opts ClaudeImportOptions) error
	ClaudeDoctor           func(ctx context.Context, globals Globals, opts ClaudeDoctorOptions) error
	ClaudeUse              func(ctx context.Context, globals Globals, opts ClaudeUseOptions) error
	ClaudeExec             func(ctx context.Context, globals Globals, opts ClaudeExecOptions) error
	ClaudeEnv              func(ctx context.Context, globals Globals, opts ClaudeEnvOptions) error

	CodexStatus           func(ctx context.Context, globals Globals, opts CodexStatusOptions) error
	CodexWatch            func(ctx context.Context, globals Globals, opts CodexWatchOptions) error
	CodexLogin            func(ctx context.Context, globals Globals, opts CodexLoginOptions) error
	CodexAccountsList     func(ctx context.Context, globals Globals, opts CodexAccountsListOptions) error
	CodexAccountsShow     func(ctx context.Context, globals Globals, opts CodexAccountsShowOptions) error
	CodexAccountsRemove   func(ctx context.Context, globals Globals, opts CodexAccountsRemoveOptions) error
	CodexAccountsForget   func(ctx context.Context, globals Globals, opts CodexAccountsForgetOptions) error
	CodexAccountsUnforget func(ctx context.Context, globals Globals, opts CodexAccountsUnforgetOptions) error
	CodexAccountsSet      func(ctx context.Context, globals Globals, opts CodexAccountsSetOptions) error
	CodexAccountsRefresh  func(ctx context.Context, globals Globals, opts CodexAccountsRefreshOptions) error
	CodexImport           func(ctx context.Context, globals Globals, opts CodexImportOptions) error
	CodexDoctor           func(ctx context.Context, globals Globals, opts CodexDoctorOptions) error
}

// CLI is the assembled agentctl command tree.
type CLI struct {
	handlers   Handlers
	root       *cobra.Command
	configDir  string
	dispatched bool
}

// New assembles the command tree, dispatching each subcommand through h.
func New(h Handlers) *CLI {
	c := &CLI{handlers: h}

	root := &cobra.Command{
		Use:           "agentctl",
		Short:         "Manage AI coding agents.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errors.New("a subcommand is required; run `agentctl --help` for the list")
		},
	}
	root.PersistentFlags().StringVar(&c.configDir, "config-dir", "", "Use this agentctl configuration directory instead of the default.")
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(c.newClaudeCmd(), c.newCodexCmd(), c.newCompletionsCmd())

	c.root = root
	return c
}

// Root exposes the underlying root command, so the caller can set the
// context, arguments and output streams before executing it.
func (c *CLI) Root() *cobra.Command {
	return c.root
}

// Dispatched reports whether a subcommand's dispatch began. An error from
// an execution that never dispatched is a command-line usage error, which
// exits 2; an error after dispatch follows the application's own exit
// mapping.
func (c *CLI) Dispatched() bool {
	return c.dispatched
}

// globals resolves the shared options at dispatch time, so the environment
// fallback is read only when the flag was not given.
func (c *CLI) globals() Globals {
	if c.configDir != "" {
		return Globals{ConfigDir: c.configDir}
	}
	return Globals{ConfigDir: os.Getenv("AGENTCTL_CONFIG_DIR")}
}

// dispatch marks the usage-error window closed and routes to h, failing
// loudly when no handler is wired in.
func dispatch[T any](c *CLI, cmd *cobra.Command, h func(context.Context, Globals, T) error, opts T) error {
	c.dispatched = true
	if h == nil {
		name := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
		return &NotImplementedError{Command: name}
	}
	return h(cmd.Context(), c.globals(), opts)
}

// groupCommand builds a parent command that only routes to subcommands and
// rejects a bare invocation as a usage error, the way the rest of the
// surface treats a missing required argument. An unknown subcommand is
// named in the refusal: "a subcommand is required" would blame the user
// for leaving out what they in fact misspelled.
func groupCommand(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return errors.New("a subcommand is required; run `" + cmd.CommandPath() + " --help` for the list")
		},
	}
}
