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
	"errors"
	"time"

	"github.com/spf13/cobra"
)

// ImportSource names where `claude import` reads accounts from. One
// source, still spelled as a value rather than as a bare flag: a command
// line that already says which source it read does not change shape when a
// second one arrives.
type ImportSource string

// ImportSourceKeychain reads Claude Code credential services discovered in
// the login keychain.
const ImportSourceKeychain ImportSource = "keychain"

// Shell names which login shell `claude env` prints for.
type Shell string

const (
	// ShellZsh prints export, unset and alias lines.
	ShellZsh Shell = "zsh"

	// ShellBash is identical to ShellZsh: both read the same syntax.
	ShellBash Shell = "bash"

	// ShellFish prints set -gx and set -e lines and a function.
	ShellFish Shell = "fish"
)

// ClaudeStatusOptions carries the parsed flags of `claude status`.
type ClaudeStatusOptions struct {
	JSON       bool
	Raw        bool
	Refresh    bool
	NoCache    bool
	All        bool
	ByIdentity bool
	Accounts   []string
	Timeout    time.Duration
}

// ClaudeWatchOptions carries the parsed flags of `claude watch`.
type ClaudeWatchOptions struct {
	Interval time.Duration
}

// ClaudeLoginOptions carries the parsed flags of `claude login`.
type ClaudeLoginOptions struct {
	Manual      bool
	Label       string
	NoDuplicate bool
}

// ClaudeAccountsListOptions carries the parsed flags of `claude accounts list`.
type ClaudeAccountsListOptions struct {
	All bool
}

// ClaudeAccountsShowOptions carries the parsed arguments of `claude accounts show`.
type ClaudeAccountsShowOptions struct {
	ID string
}

// ClaudeAccountsRemoveOptions carries the parsed arguments of `claude accounts remove`.
type ClaudeAccountsRemoveOptions struct {
	ID           string
	DeleteSecret bool
	Yes          bool
}

// ClaudeAccountsRelocateOptions carries the parsed arguments of `claude accounts relocate`.
type ClaudeAccountsRelocateOptions struct {
	ID  string
	Yes bool
}

// ClaudeAccountsForgetOptions carries the parsed arguments of `claude accounts forget`.
type ClaudeAccountsForgetOptions struct {
	Service string
}

// ClaudeAccountsUnforgetOptions carries the parsed arguments of `claude accounts unforget`.
type ClaudeAccountsUnforgetOptions struct {
	Service string
}

// ClaudeImportOptions carries the parsed flags of `claude import`.
type ClaudeImportOptions struct {
	From             ImportSource
	ClaudeConfigDirs []string
	DryRun           bool
}

// ClaudeDoctorOptions carries the parsed flags of `claude doctor`.
type ClaudeDoctorOptions struct {
	RemoveStale string
	Yes         bool
}

// ClaudeUseOptions carries the parsed flags of `claude use`. Three shapes
// share this struct, distinguished by which fields are set: an isolated
// session for one account, an undo of the most recent live swap, and a
// forget of an earlier session directory.
type ClaudeUseOptions struct {
	ID                   string
	Live                 bool
	RestartRemoteControl bool
	NewOnly              bool
	Undo                 bool
	Forget               string
	ClaudeConfigDir      string
	FreshContext         bool
	NoMCP                bool
	Yes                  bool
	JSON                 bool
}

// ClaudeExecOptions carries the parsed arguments of `claude exec`.
type ClaudeExecOptions struct {
	ID              string
	ClaudeConfigDir string
	FreshContext    bool
	NoMCP           bool
	Command         []string
}

// ClaudeEnvOptions carries the parsed arguments of `claude env`.
type ClaudeEnvOptions struct {
	ID              string
	ClaudeConfigDir string
	FreshContext    bool
	NoMCP           bool
	Shell           Shell
}

// newClaudeCmd builds the `claude` command group.
func (c *CLI) newClaudeCmd() *cobra.Command {
	cmd := groupCommand("claude", "Work with Claude subscription accounts.")
	cmd.AddCommand(
		c.newClaudeStatusCmd(),
		c.newClaudeWatchCmd(),
		c.newClaudeLoginCmd(),
		c.newClaudeAccountsCmd(),
		c.newClaudeImportCmd(),
		c.newClaudeDoctorCmd(),
		c.newClaudeUseCmd(),
		c.newClaudeExecCmd(),
		c.newClaudeEnvCmd(),
	)
	return cmd
}

func (c *CLI) newClaudeStatusCmd() *cobra.Command {
	opts := ClaudeStatusOptions{Timeout: HTTPTimeoutDefault}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show subscription usage for every known account.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeStatus, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.JSON, "json", false, "Emit the report as JSON instead of a table.")
	f.BoolVar(&opts.Raw, "raw", false, "Include the untouched upstream response body in the output.")
	f.BoolVar(&opts.Refresh, "refresh", false, "Refresh expired credentials even when a cached value would do.")
	f.BoolVar(&opts.NoCache, "no-cache", false, "Ignore the on-disk usage cache for this run.")
	f.BoolVar(&opts.All, "all", false, "Show rows that are hidden by default, such as stale siblings.")
	f.BoolVar(&opts.ByIdentity, "by-identity", false, "Fold the live credential into the row of the account that owns it, and add a Kind column saying so. Table only; --json is unaffected.")
	f.StringArrayVar(&opts.Accounts, "account", nil, "Limit the report to this account; repeat to name several.")
	f.Var(newDurationValue(&opts.Timeout, ParseDuration), "timeout", "Per-request HTTP timeout, such as 10s or 5m.")
	return cmd
}

func (c *CLI) newClaudeWatchCmd() *cobra.Command {
	opts := ClaudeWatchOptions{Interval: WatchDefault}
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Watch subscription usage in a terminal UI.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeWatch, opts)
		},
	}
	cmd.Flags().Var(newDurationValue(&opts.Interval, ParseWatchInterval), "interval", "How often to refetch usage; must be at least 60s.")
	return cmd
}

func (c *CLI) newClaudeLoginCmd() *cobra.Command {
	var opts ClaudeLoginOptions
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to an Anthropic account and store its credentials.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeLogin, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.Manual, "manual", false, "Paste the code#state value by hand instead of using a loopback redirect.")
	f.StringVar(&opts.Label, "label", "", "Give the resulting account a human-readable label.")
	f.BoolVar(&opts.NoDuplicate, "no-duplicate", false, "Refuse, instead of minting a second session, when the account authorized is the one Claude Code is already signed in as.")
	return cmd
}

func (c *CLI) newClaudeAccountsCmd() *cobra.Command {
	cmd := groupCommand("accounts", "Inspect and manage the accounts agentctl knows about.")

	var listOpts ClaudeAccountsListOptions
	list := &cobra.Command{
		Use:   "list",
		Short: "List known accounts.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeAccountsList, listOpts)
		},
	}
	list.Flags().BoolVar(&listOpts.All, "all", false, "Include rows that are hidden by default.")

	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one account in full.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeAccountsShow, ClaudeAccountsShowOptions{ID: args[0]})
		},
	}

	var removeOpts ClaudeAccountsRemoveOptions
	remove := &cobra.Command{
		Use:   "remove <id>",
		Short: "Forget an account, optionally deleting its stored credentials.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			removeOpts.ID = args[0]
			return dispatch(c, cmd, c.handlers.ClaudeAccountsRemove, removeOpts)
		},
	}
	remove.Flags().BoolVar(&removeOpts.DeleteSecret, "delete-secret", false, "Also delete the credential file agentctl wrote for this account.")
	remove.Flags().BoolVar(&removeOpts.Yes, "yes", false, "Do not prompt for confirmation.")

	var relocateOpts ClaudeAccountsRelocateOptions
	relocate := &cobra.Command{
		Use:   "relocate <id>",
		Short: "Move an account's namespace to its real organization directory.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			relocateOpts.ID = args[0]
			return dispatch(c, cmd, c.handlers.ClaudeAccountsRelocate, relocateOpts)
		},
	}
	relocate.Flags().BoolVar(&relocateOpts.Yes, "yes", false, "Do not prompt for confirmation.")

	forget := &cobra.Command{
		Use:   "forget <service>",
		Short: "Hide a discovered keychain service from reports.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeAccountsForget, ClaudeAccountsForgetOptions{Service: args[0]})
		},
	}

	unforget := &cobra.Command{
		Use:   "unforget <service>",
		Short: "Stop hiding a previously forgotten keychain service.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeAccountsUnforget, ClaudeAccountsUnforgetOptions{Service: args[0]})
		},
	}

	cmd.AddCommand(list, show, remove, relocate, forget, unforget)
	return cmd
}

func (c *CLI) newClaudeImportCmd() *cobra.Command {
	var opts ClaudeImportOptions
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Record accounts that other Claude Code config directories hold.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.From == "" {
				return errors.New("the required flag --from was not provided")
			}
			return dispatch(c, cmd, c.handlers.ClaudeImport, opts)
		},
	}
	f := cmd.Flags()
	f.Var(newEnumValue(&opts.From, string(ImportSourceKeychain)), "from", "Which source to import from.")
	f.StringArrayVar(&opts.ClaudeConfigDirs, "claude-config-dir", nil, "A Claude Code config directory to scan; repeat to name several.")
	f.BoolVar(&opts.DryRun, "dry-run", false, "Report what would be imported without changing anything.")
	return cmd
}

func (c *CLI) newClaudeDoctorCmd() *cobra.Command {
	var opts ClaudeDoctorOptions
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report on store health, locks, and stray files.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.ClaudeDoctor, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.RemoveStale, "remove-stale", "", "Remove one stale Claude Code lock artefact, named by absolute path.")
	f.BoolVar(&opts.Yes, "yes", false, "Do not prompt for confirmation.")
	return cmd
}

func (c *CLI) newClaudeUseCmd() *cobra.Command {
	var opts ClaudeUseOptions
	cmd := &cobra.Command{
		Use:   "use [<id>]",
		Short: "Start (or manage) an isolated Claude Code session for one account.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.ID = args[0]
				if opts.Undo {
					return errors.New("an account id cannot be combined with --undo")
				}
				if opts.Forget != "" {
					return errors.New("an account id cannot be combined with --forget")
				}
			}
			if opts.RestartRemoteControl && !opts.Live && !opts.Undo {
				return errors.New("--restart-remote-control requires --live or --undo")
			}
			return dispatch(c, cmd, c.handlers.ClaudeUse, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.Live, "live", false, "Hot-swap the live Claude Code credential instead of starting an isolated session.")
	f.BoolVar(&opts.RestartRemoteControl, "restart-remote-control", false, "After a live swap, ask each running Claude Code session that had Remote Control on and has the agentctl Remote Control mod to start Remote Control again. The swap is refused, with nothing written, if such a session does not answer.")
	f.BoolVar(&opts.NewOnly, "new-only", false, "Accepted as a synonym for the default (isolated-session) behaviour.")
	f.BoolVar(&opts.Undo, "undo", false, "Undo the most recent --live swap.")
	f.StringVar(&opts.Forget, "forget", "", "Remove the session directory created by an earlier use <id>.")
	f.StringVar(&opts.ClaudeConfigDir, "claude-config-dir", "", "Use this directory as the session's Claude Code config directory instead of a generated one. Must be an absolute path.")
	f.BoolVar(&opts.FreshContext, "fresh-context", false, "Do not symlink the directories that hold resumable work.")
	f.BoolVar(&opts.NoMCP, "no-mcp", false, "Omit the MCP symlink, the --mcp-config flag and the shell alias together.")
	f.BoolVar(&opts.Yes, "yes", false, "Do not prompt for confirmation.")
	f.BoolVar(&opts.JSON, "json", false, "Print the session's details as JSON before launching.")
	cmd.MarkFlagsMutuallyExclusive("live", "new-only")
	cmd.MarkFlagsMutuallyExclusive("live", "undo")
	cmd.MarkFlagsMutuallyExclusive("live", "forget")
	cmd.MarkFlagsMutuallyExclusive("undo", "forget")
	cmd.MarkFlagsMutuallyExclusive("restart-remote-control", "forget")
	return cmd
}

func (c *CLI) newClaudeExecCmd() *cobra.Command {
	var opts ClaudeExecOptions
	cmd := &cobra.Command{
		Use:   "exec <id> -- <command>...",
		Short: "Run one command with an account's credentials, without a shell.",
		Args: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash < 0 {
				return errors.New("the command to run must follow a -- separator")
			}
			if dash != 1 {
				return errors.New("exactly one account id must come before the -- separator")
			}
			if len(args) == dash {
				return errors.New("a command to run is required after the -- separator")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ID = args[0]
			opts.Command = args[1:]
			return dispatch(c, cmd, c.handlers.ClaudeExec, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.ClaudeConfigDir, "claude-config-dir", "", "Use this directory as the session's Claude Code config directory instead of a generated one. Must be an absolute path.")
	f.BoolVar(&opts.FreshContext, "fresh-context", false, "Do not symlink the directories that hold resumable work.")
	f.BoolVar(&opts.NoMCP, "no-mcp", false, "Omit the MCP symlink and the --mcp-config flag together.")
	return cmd
}

func (c *CLI) newClaudeEnvCmd() *cobra.Command {
	opts := ClaudeEnvOptions{Shell: ShellZsh}
	cmd := &cobra.Command{
		Use:   "env <id>",
		Short: "Print shell commands that put an account's credentials in your environment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ID = args[0]
			return dispatch(c, cmd, c.handlers.ClaudeEnv, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.ClaudeConfigDir, "claude-config-dir", "", "Use this directory as the session's Claude Code config directory instead of a generated one. Must be an absolute path.")
	f.BoolVar(&opts.FreshContext, "fresh-context", false, "Do not symlink the directories that hold resumable work.")
	f.BoolVar(&opts.NoMCP, "no-mcp", false, "Omit the MCP symlink, the --mcp-config flag and the shell alias together.")
	f.Var(newEnumValue(&opts.Shell, string(ShellZsh), string(ShellBash), string(ShellFish)), "shell", "Which shell's syntax to print.")
	return cmd
}
