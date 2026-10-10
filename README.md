# agentctl

`agentctl` manages Claude Code and Codex subscription accounts: usage reports, owned credentials,
isolated Claude sessions, and opt-in live Claude account swaps. Its design originates in the Rust
[`agctl`](https://github.com/zchee/agctl) tool; this implementation is Go. The supported credential and
process backend is macOS. Codex accounts can be tracked and refreshed, but not switched.

## Install

Use Go 1.27.2. Install the current module revision:

```sh
GOTOOLCHAIN=go1.27.2 go install github.com/zchee/agentctl@latest
```

The executable goes into `GOBIN`, or `$(go env GOPATH)/bin` when `GOBIN` is unset. Put that directory on
`PATH`. From a source checkout, build directly into your local executable directory:

```sh
mkdir -p "$HOME/.local/bin"
GOTOOLCHAIN=go1.27.2 go build -o "$HOME/.local/bin/agentctl" .
```

The Claude Code mod that `--restart-remote-control` relies on is installed separately, from the prompt
of a terminal Claude Code session; see [Remote Control](#remote-control).

Never install a binary built with the `agentctl_testing` tag. That tag enables test transports and fault
injection; it is only for the test suite. The release gate checks that those seams are absent.

## Commands

```sh
agentctl --help
agentctl claude --help
agentctl codex --help
agentctl claude use --help
agentctl --version
```

`--config-dir DIR` selects agentctl's store, not a vendor configuration directory. It works before or
after a subcommand and takes precedence over `AGENTCTL_CONFIG_DIR`. Otherwise the store is
`$XDG_CONFIG_HOME/agctl` when the XDG path is absolute, or `$HOME/.config/agctl`.
Use a command's `--help` for its full flag contract. The tables below show the command tree; angle
brackets denote values you supply, and square brackets denote optional arguments.

### Claude

All entries below follow `agentctl claude`.

| Command | Purpose |
|---|---|
| `status [--json] [--raw] [--all] [--by-identity]` | Report usage; identity folding affects tables only. |
| `watch [--interval DUR]` | Keep usage visible in a terminal UI. |
| `login [--label LABEL] [--manual] [--no-duplicate]` | Authorize an account and save an owned credential. |
| `accounts list [--all]` | List known accounts, including hidden rows when requested. |
| `accounts show <id>` | Inspect one account. |
| `accounts remove <id> [--delete-secret] [--yes]` | Drop the registry entry; optionally delete owned files. |
| `accounts relocate <id> [--yes]` | Move an unknown-organization namespace to its resolved organization. |
| `accounts forget <service>` | Hide a discovered keychain service without deleting its item. |
| `accounts unforget <service>` | Make that service visible again. |
| `import --from keychain [--dry-run]` | Record other Claude Code stores as read-only accounts. |
| `doctor [--remove-stale PATH] [--yes]` | Inspect stores and locks; optionally remove one proven-stale lock. |
| `use <id> [--new-only] [--json]` | Start Claude Code in an isolated session; `--new-only` is a synonym. |
| `use --live <id> [--restart-remote-control] [--yes] [--json]` | Swap the live credential after consent. |
| `use --undo [--restart-remote-control] [--yes] [--json]` | Reverse the most recent live swap. |
| `use --forget <id> [--yes]` | Delete the generated session, not the account's credentials. |
| `exec <id> -- <command>...` | Run a command directly in the isolated environment, without a shell. |
| `env <id> [--shell zsh\|bash\|fish]` | Print shell setup for the isolated environment; default shell is zsh. |

`status` also accepts repeatable `--account ID`, `--refresh`, `--no-cache`, and `--timeout DUR`
(default `10s`). `import` accepts repeatable `--claude-config-dir DIR` to select vendor stores.
Isolated `use`, `exec`, and `env` accept `--claude-config-dir DIR`, `--fresh-context`, and `--no-mcp`.
The directory override must be absolute and cannot be the live Claude configuration directory.
`claude doctor` has no `--json` flag. Stale-lock removal requires `--yes`, an eligible directory,
and an unchanged modification time across two samples 12 seconds apart; it does not break live locks.

`--restart-remote-control` asks running Claude Code sessions to start Remote Control again after a
live swap or undo; see [Remote Control](#remote-control) below.
`--json` does not supply consent: use `--yes` only when you intend to authorize the writes.

A live swap copies the incoming token pair into the live keychain item while retaining the owned copy.
A refresh on either side can invalidate the other's refresh token. Avoid competing account switchers
and do not register the same refresh chain in independent stores. Owned credential files are private
(0600), not encrypted at rest; processes running as your user can read them.

#### Remote Control

A live swap or undo stops Remote Control in every running Claude Code session that reads the swapped
store: Claude Code drops the bridge itself, now or on its next account check. The local conversation,
process and session id stay as they were. Without `--restart-remote-control`, agentctl names the
sessions its registry scan found with Remote Control on, and `/remote-control` typed in each one starts
it again.

With `--restart-remote-control` (on `use --live <id>` or `use --undo`), agentctl asks each running
session that has the `agentctl-remote-control` mod loaded and reads this store to start Remote Control
again after the swap. agentctl never types into a terminal. Before any keychain read it asks every
session with Remote Control on and Claude Code 2.1.287 or newer which credential it reads; after the
swap it waits up to 45 seconds for each eligible session's old bridge to disappear, then sends one
request. The mod runs `/remote-control` only when its session is idle and shows no bridge, so a
session in the middle of a turn starts Remote Control after that turn, provided the turn ends within
the request's 75-second lifetime. A bridge
still present after the 45 seconds gets no request, and a warning names `/remote-control` for that
session. `--yes` is allowed with the flag; the swap's own consent question still applies.

The flag refuses the swap with exit 30 and writes nothing when a session with Remote Control on does
not answer within 5 seconds (the mod is not loaded there), when Claude Code's session registry cannot
be read, or on a platform other than macOS. The JSON reasons are `remote_control_unreachable` and
`remote_control_unsupported_platform`. Run the swap again without the flag to proceed and restart
Remote Control by hand. A session that runs a Claude Code release older than 2.1.287, that rejects the
request file's owner or mode, that reads another credential, or that cannot start Remote Control (for
example behind a gateway) does not refuse the swap: it is counted, and a warning says what to do there.
A namespace-target undo, an already-active result, and an undo with nothing to undo send no request;
the last prints its one-line message and no JSON document.

The mod needs Claude Code 2.1.287 or newer and is installed from the prompt of a terminal Claude Code
session; each session that should be reconnected needs it loaded. See
[plugins/remote-control/README.md](plugins/remote-control/README.md) for its requirements, its
`/agentctl-rc` status command, and the request files it reads.

```text
/plugin install agentctl-remote-control --marketplace zchee/agentctl
```

With `--json`, the outcome carries a `remote_control` object only when the flag was given; it is then
present even when every count is zero. Its twelve integer counts are numbers of sessions, never names,
ids or URLs:

| Count | Sessions that |
|---|---|
| `eligible` | had Remote Control on, answered, and read the swapped credential. |
| `provenance_skipped` | read another credential, used a credential override, or ran behind a gateway. |
| `unreachable` | did not answer through the mod, or refused agentctl's request for a reason other than the request file's owner, mode or the release (for example `busy`). |
| `unavailable` | answered that Remote Control cannot start there. |
| `version_rejected` | run a Claude Code release older than 2.1.287, or recorded none. |
| `metadata_rejected` | refused the request file because of its owner or mode. |
| `dropped` | lost their old bridge after the swap. |
| `not_dropped` | still had their old bridge when the 45-second wait ended. |
| `reconnected` | reported a new bridge. |
| `not_confirmed` | gave no final answer, or one that is not success. |
| `already_connected` | had a bridge again before any request ran. |
| `restored` | still had a bridge after a pass that changed no credential. |

A reconnect result never changes the swap's exit code. Failures appear as warnings derived from the
counts, in `warnings` and once on stderr; an interrupt during the follow-up prints a counts-only note.
`reconnected` means the session's registry record shows a new bridge. A connected bridge proves neither
the intended account nor retained history: whether the conversation from before the swap appears on
claude.ai under the new account has not been verified.

### Codex

All entries below follow `agentctl codex`.

| Command | Purpose |
|---|---|
| `status [--json] [--raw] [--all]` | Report usage and refresh due owned accounts whose policy is `auto`. |
| `watch [--interval DUR]` | Watch usage without sending refresh tokens. |
| `login [--label LABEL] [--no-refresh]` | Run `codex login` in an owned scratch home, then verify and save it. |
| `accounts list [--all]` | List known accounts. |
| `accounts show <id>` | Inspect one account. |
| `accounts remove <id> [--delete-secret] [--yes]` | Drop the entry; optionally delete owned credentials. |
| `accounts forget <id>` | Hide a discovered account from reports. |
| `accounts unforget <id>` | Make it visible again. |
| `accounts set <id> --refresh auto\|never` | Set the owned account's automatic refresh policy. |
| `accounts refresh <id> --resend [--yes]` | Deliberately retry an unknown send after its one-hour floor. |
| `accounts refresh <id> --reset-floor [--yes]` | Clear a terminal refresh floor. |
| `import --from codex-home [--codex-home DIR] [--dry-run]` | Register another home without writing to it. |
| `doctor [--json]` | Diagnose home, credential, lock, audit, and refresh state without changing it. |

`status` also accepts repeatable `--account ID`, `--refresh`, `--no-cache`, and `--timeout DUR`
(default `10s`). The timeout applies to each request phase, not the whole pass: one usage request can
consume `4 × min(timeout, 5s) + 2 × timeout`; a pass with an owned auto-refresh account can consume
`1s + 19s + 1s + 2 ×` that budget. Unknown refresh outcomes are not automatically resent.
`accounts refresh` requires a terminal even with `--yes`; resend is allowed only once per unknown marker.

For both providers, watch defaults to `300s` and refuses intervals below `60s`. Durations accept whole
seconds, optionally suffixed with `s`, `m`, or `h`: `300`, `10s`, `5m`, and `2h` are valid; fractions,
compound durations, negative values, and milliseconds are not. Watch keys are `q`, Esc, Ctrl-C, or Ctrl-D
to quit; `r` to fetch without cache; arrows or `j`/`k` to select a row.

## Shell completions

Completion scripts come from the same command tree as argument parsing. Supported shells are bash,
zsh, fish, and PowerShell. Run the setup after installing `agentctl` on `PATH`.

### Bash

Load for the current shell, or add this to `~/.bashrc` (requires bash-completion):

```bash
eval "$(agentctl completions bash)"
```

File-based alternative; create the directory if needed, then source this file from `~/.bashrc`:

```bash
mkdir -p "$HOME/.local/share/bash-completion/completions"
agentctl completions bash >| "$HOME/.local/share/bash-completion/completions/agentctl"
source "$HOME/.local/share/bash-completion/completions/agentctl"
```

### Zsh

Load after initializing the completion system in `~/.zshrc`:

```zsh
autoload -Uz compinit && compinit
eval "$(agentctl completions zsh)"
```

File-based alternative; add the `fpath` line before `compinit` in `~/.zshrc`:

```zsh
mkdir -p "$HOME/.zfunc"
agentctl completions zsh >| "$HOME/.zfunc/_agentctl"
fpath=("$HOME/.zfunc" $fpath)
autoload -Uz compinit && compinit
```

### Fish

Load for this session:

```fish
agentctl completions fish | source
```

File-based alternative; fish loads this file on demand:

```fish
mkdir -p "$HOME/.config/fish/completions"
agentctl completions fish > "$HOME/.config/fish/completions/agentctl.fish"
```

### PowerShell

Load for this session, or put the line in `$PROFILE`:

```powershell
agentctl completions powershell | Out-String | Invoke-Expression
```

File-based alternative; source the generated file from `$PROFILE`:

```powershell
agentctl completions powershell | Out-File -Encoding utf8 "$HOME/.agentctl-completions.ps1"
. "$HOME/.agentctl-completions.ps1"
```

Elvish is unsupported. Running `agentctl completions elvish` exits 2 with this exact stderr line:

```text
agentctl: invalid value "elvish" for the shell argument: supported shells are bash, zsh, fish, and powershell
```

## Environment variables

These are production variables only. `AGENTCTL_LOG` is a level selector, not a `RUST_LOG` expression.

| Variable | Meaning |
|---|---|
| `AGENTCTL_CONFIG_DIR` | Override agentctl's store root; `--config-dir` wins. |
| `AGENTCTL_LOG` | `debug`, `info`, `warn`, or `error`; unset or invalid means `warn`; logs go to stderr. |
| `AGENTCTL_CLAUDE_USER_AGENT` | Override the Claude HTTP user agent. |
| `AGENTCTL_CODEX_USER_AGENT` | Override the Codex HTTP user agent. |
| `AGENTCTL_CLAUDE_OAUTH_SCOPES` | Override the space-separated scopes requested by Claude login. |
| `CLAUDE_CONFIG_DIR` | Locate the live Claude Code configuration and isolated-session seed source. |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR` | Select Claude's credential namespace; affects namespace-target swaps. |
| `CLAUDE_CODE_OAUTH_TOKEN` | Vendor token override; prevents credential switching when non-empty. |
| `CODEX_HOME` | Select the live Codex home read by discovery and import. |
| `HOME` | Locate the default store and vendor homes. |
| `USER`, `LOGNAME` | Identify the keychain account attribute. |
| `PATH` | Locate the `claude` and `codex` child executables. |
| `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN` | Presence-only Codex doctor diagnostics; values are not displayed. |
| `CODEX_REFRESH_TOKEN_URL_OVERRIDE` | Presence-only Codex doctor diagnostic, not an agentctl endpoint override. |
| `CODEX_APP_SERVER_LOGIN_CLIENT_ID` | Presence-only Codex doctor diagnostic. |

Isolated Claude child environments remove `CLAUDE_CODE_OAUTH_TOKEN`. Codex login forwards a limited
set of home, path, locale, terminal, proxy, and certificate variables with an agentctl-owned `CODEX_HOME`.

## Store compatibility

The Go tool reads and writes the same store layout as Rust `agctl`: registry, credential namespaces,
locks, audit logs, and refresh markers. The binary name and application environment prefix changed;
on-disk names did not. The default directory remains `~/.config/agctl`, not `~/.config/agentctl`.

| Relative path under the store | Contents |
|---|---|
| `config.json`, `.config.lock` | Account registry and its persistent lock. |
| `claude/<account>/<organization>/.credentials.json` | Owned Claude credential. |
| `claude/<account>/<organization>/.credentials.adopted.json` | Credential parked by a swap for undo. |
| `claude/.locks/<account>.<organization>.lock` | Persistent Claude namespace lock. |
| `claude/held-locks/`, `claude/keychain-writes.jsonl` | Peer-lock records and credential/config audit. |
| `claude-sessions/<account>/<organization>/` | Isolated Claude session. |
| `codex/<user>/<account>/auth.json` | Owned Codex credential. |
| `codex/.locks/<user>+<account>.lock`, `codex/.locks/scratch.lock` | Namespace and login-scratch locks. |
| `codex/.state/<user>+<account>.refresh` | Durable refresh outcome marker. |
| `codex/.scratch/`, `codex/writes.jsonl` | Login scratch homes and write-receipt audit. |
| `cache/claude/`, `cache/codex/` | Per-provider usage caches. |

Compatibility also retains the `agctl_pid` lock-record field, the `agctl` lock-record tree value, and the
`agctl-codex-login-` scratch-directory prefix. Store directories are created at 0700 and private files
at 0600. Persistent agentctl lock files are never unlinked. Both tools must use the same store locks;
separate store roots do not coordinate one refresh chain.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success, including applied/already-active swaps and non-fatal warnings. |
| 1 | Fatal configuration or I/O error; unsupported/deferred command. |
| 2 | Usage error or partial/refused result; status counts only failed rows actually shown. |
| 10 | Compromised held lock or non-unreachable lock acquisition failure. |
| 11 | Non-empty `CLAUDE_CODE_OAUTH_TOKEN` refuses switching. |
| 12 | Encoded keychain line exceeds 4032 bytes including its newline. |
| 13 | Live undo requested from a shell selecting a namespace. |
| 14 | Outgoing credential cannot be safely adopted. |
| 15 | Selected namespace is not owned by this registry. |
| 16 | Peer locks are busy. |
| 17 | Item changed or became unreadable, or insufficient hold budget remains. |
| 18 | Write timed out and verification could not determine its outcome. |
| 19 | Definite write failure. |
| 20 | Confirmation declined or unavailable; JSON is not consent. |
| 21 | Incoming migrated credential needs refresh. |
| 22 | Durable audit precondition failed. |
| 23 | Live store missing or dangling. |
| 24 | Live keychain item absent. |
| 27 | Undo found a different account in the live item. |
| 29 | Identity/profile unavailable or expired live token without audit attribution. |
| 30 | `--restart-remote-control` refused the swap before any write (see Remote Control). |
| 129, 130, 143 | HUP, INT, or TERM after child teardown and cleanup. |

Codes 25, 26, and 28 are retired. Isolated `use` and `exec` forward the child's exit code, or
`128 + signal` for signal death (fallback 1). An applied swap still exits 0 if its subsequent Claude
configuration rewrite was skipped; inspect its warnings and JSON `config` result.

## Known limitations

- `--restart-remote-control` reaches only sessions that have the `agentctl-remote-control` mod loaded;
  sessions without it refuse the swap when the flag is given. Typing `/remote-control` into a terminal
  for such sessions is not implemented.
- Linux is not supported beyond build/vet checks; credential operations require macOS.
- Claude doctor's `--remove-stale` is macOS-only.

## Testing

Run from the module root on macOS. Both build variants are required; only the tagged variant exposes
subprocess test seams. Race suites need the explicit 30-minute deadline because they test real lock
contention and sampling intervals.

```sh
GOTOOLCHAIN=go1.27.2 go build ./...
GOTOOLCHAIN=go1.27.2 go vet ./...
GOTOOLCHAIN=go1.27.2 go build -tags agentctl_testing ./...
GOTOOLCHAIN=go1.27.2 go vet -tags agentctl_testing ./...
GOTOOLCHAIN=go1.27.2 go test -race -count=1 -timeout=30m ./...
GOTOOLCHAIN=go1.27.2 go test -tags agentctl_testing -race -count=1 -timeout=30m ./...
GOTOOLCHAIN=go1.27.2 golangci-lint run --issues-exit-code 1 ./...
GOTOOLCHAIN=go1.27.2 golangci-lint run --build-tags agentctl_testing --issues-exit-code 1 ./...
GOTOOLCHAIN=go1.27.2 bash scripts/release-gate.sh
```

The release gate builds an untagged optimized binary, scans for test-seam absence with positive and
negative controls, checks selected source files, and parses the generated shell completions.

## License

Apache-2.0. See [LICENSE](LICENSE).
