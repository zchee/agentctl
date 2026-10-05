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

// CodexImportSource names where `codex import` reads accounts from.
type CodexImportSource string

// CodexImportSourceCodexHome reads a Codex home directory holding a
// credential file.
const CodexImportSourceCodexHome CodexImportSource = "codex-home"

// RefreshMode says whether agentctl refreshes an owned Codex account on
// its own. It is the command line's spelling of the registry's refresh
// policy, kept separate so the stored vocabulary and the flag's accepted
// values can change independently.
type RefreshMode string

const (
	// RefreshModeAuto refreshes when the access token is expired or
	// rejected.
	RefreshModeAuto RefreshMode = "auto"

	// RefreshModeNever never sends a refresh token.
	RefreshModeNever RefreshMode = "never"
)

// CodexStatusOptions carries the parsed flags of `codex status`.
type CodexStatusOptions struct {
	JSON     bool
	Raw      bool
	Refresh  bool
	NoCache  bool
	All      bool
	Accounts []string
	Timeout  time.Duration
}

// CodexWatchOptions carries the parsed flags of `codex watch`.
type CodexWatchOptions struct {
	Interval time.Duration
}

// CodexLoginOptions carries the parsed flags of `codex login`.
type CodexLoginOptions struct {
	Label     string
	NoRefresh bool
}

// CodexAccountsListOptions carries the parsed flags of `codex accounts list`.
type CodexAccountsListOptions struct {
	All bool
}

// CodexAccountsShowOptions carries the parsed arguments of `codex accounts show`.
type CodexAccountsShowOptions struct {
	ID string
}

// CodexAccountsRemoveOptions carries the parsed arguments of `codex accounts remove`.
type CodexAccountsRemoveOptions struct {
	ID           string
	DeleteSecret bool
	Yes          bool
}

// CodexAccountsForgetOptions carries the parsed arguments of `codex accounts forget`.
type CodexAccountsForgetOptions struct {
	ID string
}

// CodexAccountsUnforgetOptions carries the parsed arguments of `codex accounts unforget`.
type CodexAccountsUnforgetOptions struct {
	ID string
}

// CodexAccountsSetOptions carries the parsed arguments of `codex accounts set`.
type CodexAccountsSetOptions struct {
	ID      string
	Refresh RefreshMode
}

// CodexAccountsRefreshOptions carries the parsed arguments of `codex accounts refresh`.
type CodexAccountsRefreshOptions struct {
	ID         string
	Resend     bool
	ResetFloor bool
	Yes        bool
}

// CodexImportOptions carries the parsed flags of `codex import`.
type CodexImportOptions struct {
	From      CodexImportSource
	CodexHome string
	DryRun    bool
}

// CodexDoctorOptions carries the parsed flags of `codex doctor`.
type CodexDoctorOptions struct {
	JSON bool
}

// newCodexCmd builds the `codex` command group.
func (c *CLI) newCodexCmd() *cobra.Command {
	cmd := groupCommand("codex", "Work with Codex (ChatGPT) subscription accounts.")
	cmd.AddCommand(
		c.newCodexStatusCmd(),
		c.newCodexWatchCmd(),
		c.newCodexLoginCmd(),
		c.newCodexAccountsCmd(),
		c.newCodexImportCmd(),
		c.newCodexDoctorCmd(),
	)
	return cmd
}

func (c *CLI) newCodexStatusCmd() *cobra.Command {
	opts := CodexStatusOptions{Timeout: HTTPTimeoutDefault}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show subscription usage for every known Codex account.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.CodexStatus, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.JSON, "json", false, "Emit the report as JSON instead of a table.")
	f.BoolVar(&opts.Raw, "raw", false, "Include the untouched upstream response body in the output.")
	f.BoolVar(&opts.Refresh, "refresh", false, "Refresh expired credentials even when a cached value would do.")
	f.BoolVar(&opts.NoCache, "no-cache", false, "Ignore the on-disk usage cache for this run.")
	f.BoolVar(&opts.All, "all", false, "Show rows that are hidden by default, such as stale siblings.")
	f.StringArrayVar(&opts.Accounts, "account", nil, "Limit the report to this account; repeat to name several.")
	f.Var(newDurationValue(&opts.Timeout, ParseDuration), "timeout", "Per-request HTTP timeout, such as 10s or 5m.\n\nThe budget for each phase of one request, not for the pass. One request can take 4 × min(--timeout, 5s) + 2 × --timeout (40s by default), and when an owned account may be refreshed the pass may take up to 1s + 19s + 1s + 2 × that (101s by default), because a refresh POST has its own budget and is never cut short.")
	return cmd
}

func (c *CLI) newCodexWatchCmd() *cobra.Command {
	opts := CodexWatchOptions{Interval: WatchDefault}
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Watch Codex subscription usage in a terminal UI.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.CodexWatch, opts)
		},
	}
	cmd.Flags().Var(newDurationValue(&opts.Interval, ParseWatchInterval), "interval", "How often to refetch usage; must be at least 60s.")
	return cmd
}

func (c *CLI) newCodexLoginCmd() *cobra.Command {
	var opts CodexLoginOptions
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to a ChatGPT account and store its credentials.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.CodexLogin, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.Label, "label", "", "Give the resulting account a human-readable label.")
	f.BoolVar(&opts.NoRefresh, "no-refresh", false, "Record the account with refreshing switched off, so agentctl never sends its refresh token.")
	return cmd
}

func (c *CLI) newCodexAccountsCmd() *cobra.Command {
	cmd := groupCommand("accounts", "Inspect and manage the Codex accounts agentctl knows about.")

	var listOpts CodexAccountsListOptions
	list := &cobra.Command{
		Use:   "list",
		Short: "List known Codex accounts.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.CodexAccountsList, listOpts)
		},
	}
	list.Flags().BoolVar(&listOpts.All, "all", false, "Include rows that are hidden by default.")

	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one Codex account in full.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(c, cmd, c.handlers.CodexAccountsShow, CodexAccountsShowOptions{ID: args[0]})
		},
	}

	var removeOpts CodexAccountsRemoveOptions
	remove := &cobra.Command{
		Use:   "remove <id>",
		Short: "Forget a Codex account, optionally deleting its stored credentials.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			removeOpts.ID = args[0]
			return dispatch(c, cmd, c.handlers.CodexAccountsRemove, removeOpts)
		},
	}
	remove.Flags().BoolVar(&removeOpts.DeleteSecret, "delete-secret", false, "Also delete the credential file agentctl wrote for this account.")
	remove.Flags().BoolVar(&removeOpts.Yes, "yes", false, "Do not prompt for confirmation.")

	forget := &cobra.Command{
		Use:   "forget <id>",
		Short: "Hide a Codex account from reports.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(c, cmd, c.handlers.CodexAccountsForget, CodexAccountsForgetOptions{ID: args[0]})
		},
	}

	unforget := &cobra.Command{
		Use:   "unforget <id>",
		Short: "Stop hiding a previously forgotten Codex account.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(c, cmd, c.handlers.CodexAccountsUnforget, CodexAccountsUnforgetOptions{ID: args[0]})
		},
	}

	var setOpts CodexAccountsSetOptions
	set := &cobra.Command{
		Use:   "set <id>",
		Short: "Change whether agentctl may refresh an owned account's grant.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if setOpts.Refresh == "" {
				return errors.New("the required flag --refresh was not provided")
			}
			setOpts.ID = args[0]
			return dispatch(c, cmd, c.handlers.CodexAccountsSet, setOpts)
		},
	}
	set.Flags().Var(newEnumValue(&setOpts.Refresh, string(RefreshModeAuto), string(RefreshModeNever)), "refresh", "Whether agentctl refreshes this account on its own.")

	var refreshOpts CodexAccountsRefreshOptions
	refresh := &cobra.Command{
		Use:   "refresh <id>",
		Short: "Act on an account whose refresh outcome is unknown, or whose 401 floor has become terminal.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			refreshOpts.ID = args[0]
			return dispatch(c, cmd, c.handlers.CodexAccountsRefresh, refreshOpts)
		},
	}
	refresh.Flags().BoolVar(&refreshOpts.Resend, "resend", false, "Send the stored refresh token once more. Only for a row whose last refresh outcome is unknown, only an hour after that send, and only once per marker.")
	refresh.Flags().BoolVar(&refreshOpts.ResetFloor, "reset-floor", false, "Lift the terminal state a repeatedly unhelpful refresh left behind.")
	refresh.Flags().BoolVar(&refreshOpts.Yes, "yes", false, "Do not prompt for confirmation. Refused without a terminal.")
	refresh.MarkFlagsMutuallyExclusive("resend", "reset-floor")

	cmd.AddCommand(list, show, remove, forget, unforget, set, refresh)
	return cmd
}

func (c *CLI) newCodexImportCmd() *cobra.Command {
	var opts CodexImportOptions
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Record the accounts another Codex home holds.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.From == "" {
				return errors.New("the required flag --from was not provided")
			}
			return dispatch(c, cmd, c.handlers.CodexImport, opts)
		},
	}
	f := cmd.Flags()
	f.Var(newEnumValue(&opts.From, string(CodexImportSourceCodexHome)), "from", "Which source to import from.")
	f.StringVar(&opts.CodexHome, "codex-home", "", "The Codex home directory to read, instead of the one this environment names.")
	f.BoolVar(&opts.DryRun, "dry-run", false, "Report what would be imported without changing anything.")
	return cmd
}

func (c *CLI) newCodexDoctorCmd() *cobra.Command {
	var opts CodexDoctorOptions
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report on Codex store health, locks, refresh state and stray files.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dispatch(c, cmd, c.handlers.CodexDoctor, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Emit the report as JSON instead of a table.")
	return cmd
}
