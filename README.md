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
| `use --live <id> [--restart-remote-control] [--desktop] [--yes] [--json]` | Swap the live credential after consent. |
| `use --undo [--restart-remote-control] [--desktop] [--yes] [--json]` | Reverse the most recent live swap. |
| `use --forget <id> [--yes]` | Delete the generated session, not the account's credentials. |
| `exec <id> -- <command>...` | Run a command directly in the isolated environment, without a shell. |
| `env <id> [--shell zsh\|bash\|fish]` | Print shell setup for the isolated environment; default shell is zsh. |
| `desktop status [--json]` | Report desktop profiles' last-observed accounts, running processes, and interrupted switches. |
| `desktop use <id> [--yes] [--json]` | Quit the desktop app, exchange its profile for the chosen account's, and relaunch. |
| `desktop forget <name> [--yes]` | Delete one stored, inactive desktop profile, including its local Code sessions and tasks. |
| `desktop repair [--complete \| --revert] [--yes]` | Finish or reverse an interrupted desktop profile exchange. |

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
The item's other members, such as MCP logins, stay in place; see [MCP credentials](#mcp-credentials).
A refresh on either side can invalidate the other's refresh token. Avoid competing account switchers
and do not register the same refresh chain in independent stores. Owned credential files are private
(0600), not encrypted at rest; processes running as your user can read them.

#### MCP credentials

Claude Code keeps more than the account's token pair in its keychain item: OAuth tokens for remote
MCP servers (`mcpOAuth`), the client secrets of servers that registered dynamically
(`mcpOAuthClientConfig`), and plugin secrets (`pluginSecrets`). They belong to the machine, not to the
account. Every keychain write agentctl makes (a live swap, an undo, a namespace swap, and a refresh that
`status` saves into a namespace's item) writes the account's tokens together with every other
top-level member of the destination item, as read again while agentctl holds the item's locks. An
owned credential file never contributes such members, and a namespace whose item is absent receives
none. A swap does not touch the `headers` and `env` of `mcpServers` entries in `~/.claude.json`, nor
`mcp-needs-auth-cache.json` and `mcp-discovery-cache/`. An isolated session (`use <id>`) keeps its own
MCP logins in its namespace's item; agentctl does not copy the live item's into it. Claude desktop app
connectors use the app's own web session and are out of scope.

The write passes the whole document to `security add-generic-password -U` as hex in its argument
list, for every write, large or small; Claude Code does the same for a write over 4032 bytes. While
that `security` call runs, the document is visible to any process of your user that can list process
arguments, such as `ps -ww`. The write adds no application to the item's access list. If an
application other than `security` last wrote the item, macOS can ask for keychain access during the
write; the write then ends at its timeout and the swap reports `unknown` (exit 18).

The document may be up to 256 KiB. A larger one is refused before anything is written: a swap exits
31 with reason `credential_too_large`, and a `status` refresh names `credential_too_large` in the
row's state. A document that grows past the limit while agentctl holds the item, or one whose
argument list the host refuses because it offers less argument space than the limit needs, is a
discard (exit 17) whose note begins with `credential_too_large`; `security` never ran and the item is
unchanged.

agentctl re-reads the item under its locks and writes it within the same hold, but Claude Code does
not take those locks: an MCP token Claude Code writes while a swap holds the item can be lost; the
server then asks you to authorize again. The window is the hold, reported as `lock.hold_ms`, and no
check can detect the loss afterwards.

With `--json`, a `use --live` or `use --undo` report whose swap reached the write adds
`carried_members` (each carried member's name and, for an object, its entry count),
`siblings_readback`, and `transport` (`argv`). `siblings_readback` is `equal` when every carried member
read back as written, `differs` when another process rewrote one after agentctl's write (the note
names them), and `unverified` when the readback failed; it does not detect the loss described above.
Each write entry in `keychain-writes.jsonl` records the carried member names, the readback verdict,
and the transport. `status` and `doctor` print `mcp_oauth_entries=N`, the number of MCP servers whose
OAuth tokens a row's keychain document holds, for the live row and every namespace row whose
credential was read; `status --json` carries the same count as the row member `mcp_oauth_entries`,
null on other rows. A namespace row read from its credential file reports 0. No report, log, or audit
entry carries a member's value.

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

#### Desktop app

The desktop commands are macOS (darwin) only and use `/Applications/Claude.app`. A desktop switch is
separate from a Claude Code credential swap: the app uses its own web session, so a CLI swap does not
switch the desktop account unless `--desktop` asks for it.

`desktop use <id>` resolves a registered account, asks the app to quit, waits for its main and counted
family processes to stop, exchanges the whole directory at `~/Library/Application Support/Claude/`
with that account's stored profile, then relaunches the app. Finish or save work in the app first; the
confirmation names what will close and move. agentctl never kills the app. If the account has no stored
profile, the app creates a fresh one at launch. You sign in by hand once per profile; later selections
reuse it. Each profile also holds its own Code tab binary download. If the data directory already names
the requested account, `already_selected` neither quits nor moves anything.

Stored profiles live at `~/Library/Application Support/agentctl/claude-desktop/profiles/<name>/`.
The active profile lives at the app's data path, not in `profiles/`. The append-only `journal.jsonl`
and persistent `lock` sit beside `profiles/` in the desktop store, independent of `--config-dir` and
`AGENTCTL_CONFIG_DIR`; those select only the account registry used to resolve ids. Exchanges use
same-filesystem directory renames, never a copy fallback. `use` and `repair` do not edit files inside
profiles. `forget` is the exception: it deletes a stored, inactive profile and **all of that profile's
local Code sessions and tasks**, after confirmation. Use a name listed by `desktop status`; deletion
cannot be undone and is refused while the app runs or a switch is unresolved.

agentctl does not extract, decrypt, change or copy cookies or token caches, and never reads or changes
the safeStorage keychain password (`Claude Safe Storage`). It reads a bounded `config.json` only for
the plaintext `lastKnownAccountUuid`. That is the last account the profile observed, not proof of a
current sign-in. `desktop status` reports this identity, stored profiles, process counts and interrupted
switches. A failed observation exits 30, retains the rows it could read, and reports unknown counts as
`null` in JSON rather than zero. A stored profile with an unregistered identity is `foreign`; one with
the active profile's identity is marked `duplicate`.

A journal intent without a result blocks another switch or deletion. `desktop repair` inspects the
recorded directory identities and the actual layout. If no move began, it closes the intent as
`aborted`; if the target is already installed, it launches the app if needed and records the result.
Otherwise it reports the layout and requires `--complete` to install the target or `--revert` to restore
the original. An inconsistent layout is left untouched. A launch failure can leave the previous
profile restored, or an open intent needing repair; see the [desktop outcome words](#desktop-outcome-words).
A reported launch means a main process was observed, never that authentication or the Code tab works.

`use --live <id> --desktop` follows an applied swap with the same switch to that account, after the
Remote Control follow-up; the swap's question names what will close and move, and `--yes` answers both.
When the question was asked, the switch checks under the desktop lock that it would still do what the
question described (the same profile leaving the data path, the same stored profile arriving, an app
still on the account when nothing was to move, a refusal when a refusal was promised); anything else is
`desktop_consent_stale` with nothing quit or moved. The number of running app processes is not part of
that check. The switch never changes the swap's outcome or exit code: after an applied swap, a switch
that does not finish, or does not run, is a warning; a swap that did not apply leaves the desktop app
alone without a warning. The JSON outcome gains a `desktop` object (`outcome`, `before`, `after`,
`identity`; `outcome` is `null` when nothing ran, and its `detail` says why). `use --undo --desktop`
switches back only when the journal holds an `applied` switch recorded for the swap being undone, the
data path still holds the directory that switch installed, and that profile still reads as the switch
observed it, as the account it switched to (its first sign-in finished since), or as signed in to
nobody; anything else is `desktop_profile_mismatch` with nothing moved, and `before` names what the
data path held. When there is no swap to undo at all, `use --undo --desktop` prints its usual line and
a desktop warning on stderr; there is no swap report, so there is no `desktop` object. The undo selects the account's own stored profile; a
profile the forward switch stored as `fresh-…` or `unknown-…` stays stored, and the warning names it
and the `desktop forget` that removes it. A swap whose audit entry could not be written records a
switch no undo can match.

agentctl automates only the local lifecycle of your own app: quit, exchange its data directory,
relaunch. It never automates access to the service and never copies or injects a session. You decide
whether to use it under the applicable [Consumer Terms](https://www.anthropic.com/legal/consumer-terms).

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
| 12 | No longer produced: keychain writes pass the document to `security` in argv. It meant an encoded keychain line over 4032 bytes. |
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
| 30 | Remote Control pre-write refusal, desktop refusal, or incomplete desktop result (see below). |
| 31 | Credential document over the 256 KiB keychain update limit; reason `credential_too_large`, nothing written. |
| 129, 130, 143 | HUP, INT, or TERM after child teardown and cleanup. |

Codes 25, 26, and 28 are retired. Isolated `use` and `exec` forward the child's exit code, or
`128 + signal` for signal death (fallback 1). An applied swap still exits 0 if its subsequent Claude
configuration rewrite was skipped; inspect its warnings and JSON `config` result.

### Desktop outcome words

Desktop operations report these words; `desktop use --json` puts the word in `outcome`. Exit 30 does
not always mean nothing changed: inspect the word and the reported layout before retrying. Unknown
account ids or profile names, conflicting repair flags, and declined or unavailable consent exit 2;
fatal configuration or I/O failures exit 1. Such errors use `error` in a JSON operation report.

| Word | Code | Meaning |
|---|---|---|
| `selected` | 0 | The target profile is in place and a new main process was observed within the launch wait; identity is `matches`, `fresh`, or `unknown`. |
| `already_selected` | 0 | The data directory already names the requested account; nothing was quit or moved. |
| `launched_unverified` | 0 | A new main appeared after the launch wait while rollback was being decided; the target stays in place. |
| `repaired` | 0 | Repair recorded the completed or reverted exchange and observed a running main. |
| `nothing_to_repair` | 0 | No journal intent is unresolved. |
| `aborted` | 0 | Repair found that no exchange began, closed the intent, and asks you to run `desktop use` again. |
| `forgotten` | 0 | The named stored profile and its contents were deleted. |
| `desktop_unsupported_platform` | 30 | Desktop commands require macOS. |
| `desktop_app_not_found` | 30 | The supported app executable is missing from `/Applications/Claude.app`. |
| `desktop_bundle_mismatch` | 30 | The installed bundle does not have Claude's expected bundle identifier. |
| `desktop_lock_busy` | 30 | Another operation holds the per-user desktop lock. |
| `desktop_other_copy_running` | 30 | Another installed copy of the app runs; agentctl does not quit it. |
| `desktop_intent_open` | 30 | A switch has an intent without a result; run `desktop repair` first. |
| `desktop_cross_device` | 30 | The data path and profile store are on different filesystems; nothing is copied. |
| `desktop_profile_unreadable` | 30 | The default or target profile's identity could not be read safely. |
| `desktop_profile_mismatch` | 30 | The stored target last observed a different account. |
| `desktop_journal_failed` | 30 | The journal could not be read or the operation could not be recorded before an exchange. |
| `desktop_quit_refused` | 30 | The quit request failed; no profile was moved. |
| `desktop_quit_timeout` | 30 | Counted app processes remain after quitting; no profile was moved. |
| `desktop_repair_inconsistent` | 30 | The layout contradicts the intent, changed during recovery, or a repair move failed; the intent stays open. |
| `desktop_observe_failed` | 30 | Process observation failed, or status could not read part of its report; absence is not assumed. |
| `desktop_app_running` | 30 | Deletion was refused because an app process still runs. |
| `desktop_profile_in_place` | 30 | Deletion was refused for the profile recorded as in place by the last applied switch. |
| `desktop_repair_needs_choice` | 30 | The interrupted layout needs an explicit `--complete` or `--revert`. |
| `desktop_destination_exists` | 30 | A rename destination already exists; agentctl does not overwrite it. |
| `desktop_profile_symlink` | 30 | Deletion encountered a symlink and stopped; the message says whether anything was deleted and what remains. |
| `desktop_consent_stale` | 30 | A switch that follows a swap would no longer do what the swap's question described; nothing was quit or moved. |
| `reverted_relaunched` | 30 | The target did not start; the original profile was restored and its recovery launch was observed. |
| `relaunch_failed` | 30 | The original profile was restored, but its recovery launch was not confirmed. |
| `exchange_not_restored` | 30 | The exchange could not be restored safely; the intent stays open for repair. |
| `journal_not_recorded` | 30 | Moves or a launch may have completed, but the result was not recorded; run `desktop repair`. |
| `repaired_not_running` | 30 | Repair placed the profiles and recorded its direction, but the app's start was not confirmed. |
| `partial` | 30 | Profile deletion stopped partway; the message reports what remains. |

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
