# Claude desktop app: switching the signed-in account by exchanging its data directory

Written 2026-10-11 19:00:49 JST (from `date` in the command that wrote this file). Every other instant below is
copied from the named source file, where it came from that file's own `date` output. Structure only: this record
names directories, files, keys, process paths and keychain item names, never a cookie, token, password or
user name.

## Purpose and scope

`agentctl claude use --live` and `--undo` exchange the Claude Code CLI credential. The desktop app
(`/Applications/Claude.app`, bundle `com.anthropic.claudefordesktop`, version 2.31226.1, Electron) does not
read that credential: it signs in with a claude.ai web session held in its own data directory, so a CLI swap
leaves the desktop on the old account. This record collects what was established before any code was
written: where the app keeps its account, what Anthropic documents about sign-in, which processes belong
to a running instance, and the two spike runs that proved the exchange on this Mac.

What agentctl will automate, as approved:

| Step | Design |
|---|---|
| Store | one per-user directory `~/Library/Application Support/agentctl/claude-desktop/` (mode 0700), independent of `--config-dir`, holding `profiles/<name>/`, an append-only fsynced `journal.jsonl` and one `lock`; the agentctl store is consulted only to resolve account ids |
| Data path | `~/Library/Application Support/Claude` (called `D` below), resolved from `HOME`, no symlink component allowed in its parent chain; agentctl exchanges whole directories at `D` and never writes inside whichever one is in place |
| Quit | refuse when another installed copy's main executable runs (the AppleScript quit addresses the bundle id and could reach it); skip the quit when nothing of the app runs; otherwise `osascript -e 'tell application id "com.anthropic.claudefordesktop" to quit'`, then poll until zero main processes and zero counted family processes remain, 20 s budget, nothing renamed or relaunched on a timeout or refusal |
| Exchange | same filesystem checked by device id before any quit; renames are exclusive (`RENAME_EXCL`), verified afterwards by (device, inode), no copy fallback |
| Launch | `open -n /Applications/Claude.app` (fixed path, never bundle id); readiness is a new main process of this user within 15 s; a file is never readiness; "launched" never means signed in |
| Identity | `config.json` is read through the 64 KiB bounded reader for the plaintext key `lastKnownAccountUuid`; reader states `fresh`, `unreadable`, `unknown`, `uuid`; the value is last-observed identity, never "signed in" |
| Journal | an intent row before the first rename, a result row after the launch step, each with (device, inode) pairs; undo acts only on an `applied` result whose swap audit id matches and whose observed identity still holds; `repair` resolves an open intent from the recorded pairs |
| Commands | `agentctl claude desktop status`, `desktop use <id>`, `desktop forget <name>`, `desktop repair`; later `--desktop` on `use --live` and `use --undo` |
| Platform | darwin only; elsewhere refused before any read |

What agentctl never touches: the `Cookies` database, the two token caches in `config.json`, the keychain item
`Claude Safe Storage` (the safeStorage password), the service (no request is made to claude.ai, no session is
copied or injected), the gated multi-account mode, and a second concurrent instance. agentctl automates the
local lifecycle of the person's own app: quit, exchange the data directory, relaunch. The person signs in once
per profile.

## What the app stores and where

Measured 2026-10-11 01:07-01:10 JST (`.omc/research/claude-desktop-account-storage.md`, its own header).

| Item | Location | What it is | Evidence |
|---|---|---|---|
| Data directory `D` | `~/Library/Application Support/Claude/` | a standard Electron profile (`Cookies`, `Local Storage/`, `Session Storage/`, `Partitions/`, `WebStorage/`, `Preferences`, `Local State`) plus the app's own JSON files and two Claude Code trees | `ls -la` of `D` and one level down |
| Web session | `D/Cookies` | Chromium cookie database; on `.claude.ai` the sign-in cookie `sessionKey` (secure, httponly, encrypted) with `lastActiveOrg` beside it; every `encrypted_value` starts with the `v10` tag | `sqlite3 .schema` and a column-length query on a scratch copy |
| Encryption key | keychain item service `Claude Safe Storage` | Electron safeStorage password; Chromium derives the AES-128-CBC key with PBKDF2-HMAC-SHA1, 1003 iterations, salt `saltysalt`; a value encrypted under another key does not decrypt; a missing item is regenerated at random on launch | `security find-generic-password -s` (attributes only); Chromium `keychain_key_provider.mm`, `keychain_password_mac.mm` |
| Token caches | `D/config.json` keys `oauth:tokenCache`, `oauth:tokenCacheV2` | base64 of `v10` ciphertext under the same key | base64 decode of the prefix only |
| The one plaintext identity key | `D/config.json` key `lastKnownAccountUuid` | the last account the app observed; an agentctl account UUID; nothing shows it is cleared on sign-out | `jq` on the file; `strings` on the extracted bundle |
| Known accounts | `D/ant-device-registry.json` | one key per account UUID, opaque values; lists every known account, not the active one | key listing only |
| Hosted Claude Code | `D/claude-code/<version>/<hash>/claude.app/Contents/MacOS/claude` (Mach-O, bundle `com.anthropic.claude-code`), `D/claude-code-vm/<version>/claude` (Linux ELF) | the Code tab's own binary, downloaded and updated by the app; it receives a token the app mints from the web session as `CLAUDE_CODE_OAUTH_TOKEN`; it never reads the CLI's keychain item | `file`, `PlistBuddy`; bundle strings `performOauthFlow`, `/oauth/authorize`, `/v1/oauth/token` |
| Hosted sessions | `D/claude-code-sessions/<account uuid>/<session uuid>/scheduled-tasks.json` | per-account scheduled tasks only; the seven directory names match `~/.config/agctl/claude/` exactly | `ls`; `fd -t l` found no symlink into `~/.claude` |
| CLI keychain items | services `Claude Code-credentials` and `Claude Code-86c75be7` | the CLI's; the desktop bundle contains neither name | `security dump-keychain` filtered on `svce`; `strings` on the bundle |
| Chrome native-messaging host | `/Applications/Claude.app/Contents/Helpers/chrome-native-host`, manifest `~/Library/Application Support/Google/Chrome/NativeMessagingHosts/com.anthropic.claude_browser_extension.json` (`type: stdio`) | the host of the Claude browser extension; Chrome starts it from the app bundle | `jq` on the manifest; `ps` parent chain (run 1 below) |
| Profile selection | none | a signed build sets `userData` itself and deletes `CLAUDE_USER_DATA_DIR` from the environment it forwards; its own log line says signed builds ignore the variable; `--user-data-dir` cannot select a second profile | `index.pre.js`, `index.chunk-BXpqGRfm.js` in the extracted asar |
| Multi-account | config key `authentication.disableMultiAccount` (scope `1p`, public-undocumented) and remote feature gate `96101707` | with the gate off the code keeps one account; no account-switcher symbol exists | bundle strings |

The identity key held a different value at the storage inspection (prefix `a31d5c6d`, 01:07 JST, the storage
note) than at both spike runs (prefix `552d914f`, `run2.log` and the run 1 record); only the fact that it
changed is recorded here. It is consistent with the key being last-observed identity rather than proof of a
live session.

Consequence: the only lever that touches no credential is which directory sits at `D` when the app starts.

## Sign-in facts from the documentation

Written 2026-10-11 01:10:52 JST (`.omc/research/claude-desktop-signin-docs.md`, its own header).

| Fact | Source |
|---|---|
| Sign-in methods are the claude.ai ones, shared by web, desktop and mobile: Google, or an emailed link or code; no dedicated password exists | https://support.claude.com/en/articles/13189465-log-in-to-your-claude-account |
| The desktop quickstart says to sign in with the Anthropic account and, when prompted to sign in online, to complete it and restart the app | https://code.claude.com/docs/en/desktop-quickstart |
| Sign-out is from the app menu; for Code tab authentication errors the documented fix is sign out and back in, or quit the app completely (not just the window) and sign in again | https://code.claude.com/docs/en/desktop (troubleshooting) |
| claude.ai Settings > Account > Log Out signs out every device, desktop applications included; Claude Code OAuth tokens are a separate list under Settings > Claude Code | https://support.claude.com/en/articles/10310342-how-do-i-log-out-of-all-active-sessions |
| The session credential is the `sessionKey` cookie (authentication, `.claude.ai`, one month), with `sessionKeyV2` and `activitySessionId`; `lastActiveOrg` is a preferences cookie (one year) | https://privacy.claude.com/en/articles/10023541-what-cookies-does-anthropic-use |
| Switching between organizations of one account is through the initials menu; one account at a time | https://support.claude.com/en/articles/13189465-log-in-to-your-claude-account (FAQ) |
| Enterprise pinning: the app reads the preferences domain `com.anthropic.claudefordesktop`, key `forceLoginOrgUUID` | https://support.claude.com/en/articles/12622667-enterprise-configuration |
| The Code tab runs its own copy of Claude Code with its own version; desktop and CLI share `~/.claude.json`, `~/.claude/settings.json`, CLAUDE.md, hooks and skills; the desktop does not call `apiKeyHelper` or read `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN`; it uses OAuth | https://code.claude.com/docs/en/desktop, https://code.claude.com/docs/en/authentication |
| The CLI stores its credential in the keychain, keyed per `CLAUDE_CONFIG_DIR`; multiple CLI accounts use one configuration directory each | https://code.claude.com/docs/en/authentication |
| Deep links (`claude://claude.ai/new`, `claude://code/new`, ...) are navigation only; no documented way injects a session | https://support.claude.com/en/articles/14729294-open-claude-desktop-with-a-link, https://code.claude.com/docs/en/deep-links |
| Electron safeStorage stores its key in the keychain as `<app> Safe Storage`; the app should be code signed for consistent behaviour | https://www.electronjs.org/docs/latest/api/safe-storage; Electron `shell/browser/electron_browser_main_parts.cc` |
| Multi-account on the desktop is an open request without an Anthropic reply | https://github.com/anthropics/claude-code/issues/18435 |
| Consumer Terms bar access through automated or non-human means outside an API key and bar sharing login information | https://www.anthropic.com/legal/consumer-terms (sections 2 and 3) |

Third-party only (not relied on): a second instance with `--user-data-dir` (reported unreliable because the
`claude://` callback can land in the wrong instance), and cookie-swapping browser extensions. Unknown from the
documentation: whether the app re-reads the cookie database while running, which fixed set of variables the
app imports from the shell profile, and whether a locally written cookie would count as prohibited automation.
agentctl's stance: it automates quit, exchange and relaunch of the person's own app and never automates access
to the service; the person decides.

## Process classification

The exchange is safe only while no process of the app's instance can write into `D`. The rule the spike
settled, and the production snapshot will implement over `kern.proc.all` plus `kern.procargs2`:

| Class | Rule | Waited for |
|---|---|---|
| Main | this user's processes whose path is exactly `/Applications/Claude.app/Contents/MacOS/Claude` | yes |
| Family, reason `data` | every process whose path begins with `D/` (the Code tab's hosted binary under `claude-code/`, anything else run from the data directory) | yes |
| Family, reason `orphan` | a process under `/Applications/Claude.app/` whose parent is pid 1 (a leftover of a main that exited, such as `chrome_crashpad_handler`) | yes |
| Family, reason `parent-missing` | a bundle process whose parent pid is not in the snapshot; unknown ancestry is never evidence of absence | yes |
| Family, reason `descendant` | a bundle process any of whose ancestors, followed through the complete parent table, is a main or a counted process; a helper reached through an executable outside both prefixes is still the app's | yes |
| Family, reason `unknown` | every other bundle process: ancestry ends somewhere else and no exception applies; fail closed, printed | yes |
| Foreign (the single verified exception) | the exact path `/Applications/Claude.app/Contents/Helpers/chrome-native-host` whose direct parent is a live main executable of Google Chrome, Chrome Beta, Canary, Dev or Chromium; recorded with pid, parent pid, path and parent path; reported, not waited for; the same path under any other parent is `unknown` | no |
| Other copies | this user's processes whose path ends `/Contents/MacOS/Claude` and lies under neither prefix: another installed copy of the app; a refusal before any quit | n/a |

Enumeration errors fail closed: a `ps` (or sysctl) failure, an empty answer, or a row that does not begin
`<pid> <ppid> <uid>` stops the operation; it never counts as zero. The user's uid is resolved once by a checked
`id -u`, since a wrong uid would count the app's processes as another user's, that is as zero.

Why this shape. Run 1 (below) stopped on one lingering bundle process that was Chrome's native-messaging
host, started by Google Chrome two days earlier and holding nothing under `D`. The first amendment followed
the parent chain and ignored every bundle process whose parent was outside the app. Review found that rule
fail-open: an absent parent, a relay grandchild, or an orphaned relay would all have been treated as foreign.
The rule was inverted into the fail-closed form above: every bundle process counts unless it is this one
verified exception, and the exception needs a live Chrome-family main as its direct parent. Whether the
exchange can affect the host is not assumed from run 1; the pre-check before run 2 proved, for the host pid
alive at that instant, that it held nothing under `D`.

## Spike run 1 (2026-10-11 07:36:03 JST): no-go, explained

Instants from the script's own `date`, recorded in `.omc/plans/claude-desktop-profiles.md`. The person ran
earlier bytes of the script (sha256 prefix `9168fcb4`) whose family rule was "every process under the
bundle".

| Fact | Evidence |
|---|---|
| Baseline identity `552d914f`; main process 13069; 21 other processes before | the script's printout |
| `quit requested at 07:36:04`; after 20 s: main processes 0, other processes under the bundle or the data directory 1; the script stopped before any move, as designed | the script's printout |
| Nothing moved: `D` kept inode 122371396; the scratch directory `agentctl-spike.Fxwyj9` stayed empty | inode check after the run |
| The lingering process is `/Applications/Claude.app/Contents/Helpers/chrome-native-host`, pid 37162, parent 34854 `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`, both started 2026-10-09 23:29 | `ps -axo pid=,ppid=,uid=,lstart=,comm=` at 07:37 JST |
| Chrome starts it: the native-messaging manifest names that path as the extension host | `jq` on the manifest |
| It holds no file under `D`; it holds its log `~/Library/Logs/Claude/chrome-native-host.log`, a socket under `/tmp/claude-mcp-browser-bridge-<user>/`, pipes to Chrome; its executable image is a since-deleted updater staging copy, so it predates the current bundle | `lsof -p 37162` (paths only) |
| Its log (1862 lines) never mentions `Application Support/Claude/` | `grep -c` |
| Read-only classification at 07:40:46 JST with the app relaunched: 1340 rows, mains 1, family 17, foreign 1 (the host under Chrome), other copies none | the snapshot functions sourced from the amended script |
| Read-only check of the fail-closed rule at 08:02:41 JST: 1294 rows (bare names, a negative uid accepted), mains 1, counted 17 (16 `descendant`, 1 `orphan`: `chrome_crashpad_handler`), excepted exactly the host under Google Chrome, no other copy, no row under `D` (the Code tab was not open) | the snapshot functions sourced from the final script |

## Spike run 2 (2026-10-11 18:41:41 JST): go

The final script bytes (sha256 `a27aa15f...`, checked in the same command) were run on this Mac by the
planning session rather than by the person, at the user's choice. Deviations from the user-run design: the
prompt was answered through a FIFO after a screenshot was read; the two window observations came from
`screencapture -l <window id>` images kept in the session scratchpad, not in the repository, because the
restored image shows other sessions' content. The hosted Code child appeared only after a session was started
in the Code tab, not when the tab was merely opened (two checks at 18:31:48 and 18:37:23 JST found none; the
plan records them). Instants below are from
`run2.log` and `run2-observations.txt` (retained evidence, below), both written by `date`.

Pre-checks, same command as the start (18:41:40 JST, `run2.log`):

| Check | Result |
|---|---|
| Hosted child row | pid 36589, parent 36585 (`Helpers/disclaimer`, a child of the main), path `D/claude-code/2.1.295/.../claude.app/Contents/MacOS/claude` |
| Host row | `37162 34854 /Applications/Claude.app/Contents/Helpers/chrome-native-host` |
| Parent row | `34854 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome` (a `ps -p` of the parent pid, not the number alone) |
| `lsof -p 37162` | exit 0, 15 lines, 0 lines matching `Library/Application Support/Claude(/|$)` (the pattern matches `D` itself and every path below it) |
| Script | sha256 `a27aa15febb827e6971d020f012eeaeef37bd817c2bd259f227934cc5a56fa60` |

The run:

| Step | Instant (JST) | Observation |
|---|---|---|
| Start | 18:41:41 | baseline identity `552d914f`; main 63246; 20 counted (18 `descendant`: helpers, `Helpers/disclaimer`, plugin and renderer helpers; 1 `data`: the hosted child 36589; 1 `orphan`: `chrome_crashpad_handler` 64616); excepted 1 (the host under Chrome) |
| First quit | requested 18:41:42, no process left 18:41:45 | 3 s (difference of the two log instants); the hosted child was included in the wait |
| Inventory | — | 116572 entries (relative path, size, inode, sub-second mtime; no content); original inode 122371396 |
| Launch on the empty path | requested 18:43:12, new main 43157 at 18:43:13 | 1 s; `PASS`: fresh tree created at `D` (23 top-level entries); `PASS`: fresh `config.json` has no account identity |
| Window 1 | prompt 18:43:18, observed 18:44:20 (`run2-observations.txt`) | window 6727 (600x600) shows the "Claude for Mac / Get started" onboarding screen, no account, not signed in; nobody signed in |
| Second quit | requested 18:44:21, no process left 18:44:21 | under 1 s |
| Restore | — | `PASS`: the original profile unchanged while aside (116572 entries, empty `diff`) |
| Relaunch | requested 18:45:43, new main 44896 at 18:45:44 | 1 s; `PASS`: restored identity equals the baseline (`552d914f`); after: 17 counted, excepted 1 |
| End | 18:45:49 | `PASS 4, FAIL 0`, exit 0 |
| Window 2 | observed 18:46:48 (`run2-observations.txt`) | window 6751 (5120x2880) shows the original account's Code tab with its three hosted sessions and their histories; the Code tab works |

After the run the plan records a re-check of `D` (inode 122371396, identity `552d914f`) and the deletion of
the scratch directories (`agentctl-spike.fsyey8` with the fresh tree, identity absent, and run 1's empty
`agentctl-spike.Fxwyj9`) at 18:46:57 JST.

Verdict: go. Every pass condition held, and the open question of whether the hosted Code child stops with the
quit is answered for this app version: it was gone within 3 s of the first quit request.

## Fixture battery

The final script was run against fakes, never against the real app: a fake `HOME` holding a profile whose
`lastKnownAccountUuid` is a placeholder, and fake `id`, `ps`, `osascript` and `open` first on `PATH`. The
fakes keep a process table of `pid ppid uid path` rows in a state file; `open` registers a main under launchd,
two helpers under it and a fresh tree at `D`; `osascript` clears the table, or keeps helpers as orphans, or
keeps a hosted child under `D/claude-code/`, or refuses, or does nothing, by knob, and never removes the
Chrome-started host; `ps` can fail or print a malformed line on the n-th call and always prints bare command
names, an empty command, a negative uid, a sibling `Claude Mail.app` and a fake Chrome; `id` can fail, print
nothing or print a non-number. The harness refuses a process table with a duplicate pid.

| Item | Value |
|---|---|
| Script | `spike.sh` (retained evidence, below), sha256 `a27aa15febb827e6971d020f012eeaeef37bd817c2bd259f227934cc5a56fa60`, the bytes run 2 executed |
| Results | `results8.txt` (retained evidence, below), sha256 `18c9dbb8e09073d714ce8eaafbc2d0fb93c48de0a8f335465a739225aa6b640d` |
| Battery instants | start 2026-10-11 18:10:21 JST, end 2026-10-11 18:17:17 JST (the file's first and last lines) |
| Cases | 31 case headers (counted in this file); the plan records 31 exit statuses, each equal to its expected value |

Case families and what each proved:

| Family | Cases | Outcome |
|---|---|---|
| Happy path | main plus one helper | two quits, two launches, `PASS` x4, original inode back |
| Enumeration failures | `ps` fails before anything, in the first quit poll, in the launch poll, inside the exit handler; `ps` prints a malformed row in the quit poll or in the handler; `id -u` fails, prints nothing, prints a non-number | fail closed: nothing quit, launched or moved before the first rename; after it, the handler prints the layout and the manual restore command and moves nothing |
| Quit failures | first or second quit times out, is refused, leaves helpers, leaves a hosted child under `D` | nothing moved before the exchange; after it, not restored, manual command printed |
| Launch failures | `open` fails after the exchange (restored after a verified zero-process check); `open` fails at the restore launch (original already back) | the handler restores only after zero processes are verified |
| Interrupts | stdin closed at the prompt; SIGTERM at the prompt (exit 130) | handler as above with the app running |
| Tamper | a file written into the aside profile | `FAIL: profile-a changed while aside` with the diff line; restore proceeds; identity still passes |
| Classification | the native host under Chrome (excepted, run passes, "excepted: 1" at the end); the same host orphaned (`orphan`, timeout); under `/usr/bin/other` (`unknown`, timeout); a helper whose parent is absent (`parent-missing`, timeout); a relay between the main and a helper (`descendant` while the main lives, `unknown` once it is gone, timeout); an orphaned relay (`unknown`, timeout); only a hosted child running (counted, quit not skipped, passes) | the fail-closed rule counts every unproved bundle process and never ignores a process under `D` |
| Other copy | a second copy's main under `~/Applications` | refused before any quit, nothing moved |
| Rename primitive | the destination directory appears between the check and the `mv` (unit test with a wrapper) | "rename did not land", the real location named; no silent nesting |
| Live read-only | the snapshot functions against this Mac's real process list | the 08:02:41 JST row in the run 1 table |

Invariants over the battery (the plan's statement, checked against the results file): no case moved anything
after an enumeration failure, a malformed answer or a uid failure; no case quit or moved anything while another
copy's main ran; no case restored while any process of the app (bundle helper or hosted child) remained.

## Retained evidence

| File | sha256 (`shasum -a 256`, this session) |
|---|---|
| `.omc/artifacts/w14/run2/run2.log` | `500fab2f210932fecea470591c878e1e7fa1becd0348ae100eea5a785b630e02` |
| `.omc/artifacts/w14/spike/spike.sh` | `a27aa15febb827e6971d020f012eeaeef37bd817c2bd259f227934cc5a56fa60` |
| `.omc/artifacts/w14/spike/results8.txt` | `18c9dbb8e09073d714ce8eaafbc2d0fb93c48de0a8f335465a739225aa6b640d` |

`run2-observations.txt` sits beside `run2.log`; the two screenshots stay in the session scratchpad. The
earlier result files (`results4.txt` to `results7.txt`) and the earlier script bytes (`spike.sh.rev6` to
`spike.sh.rev9`) are kept for diffing and are not evidence for the final script.

## Open questions carried

| Question | State |
|---|---|
| A stopped second copy of the app and the bundle-id quit | the AppleScript quit addresses the bundle id; a running other copy is a refusal; a stopped one has not been exercised |
| An app-owned executable outside both prefixes holding `D` after the quit | none seen: `lsof +D D` before run 2 listed the main, one helper, the hosted child and Spotlight's `mdworker`; the inventory diff is the evidence per run |
| The native host's later access to `D` | the pre-check proves the instant before the run only; a host that opens `D` later would be found by the inventory diff, not prevented |
| Power-loss durability | the journal is fsynced and `repair` recovers from the recorded (device, inode) pairs; not exercised |
| Fresh versus external `D` | run 2 exercised an app-created fresh tree; a profile created elsewhere and renamed in has not been exercised |

## Next

The gate before the `--desktop` flag on `use --live` and `use --undo` is an operator verification on this
Mac, after the `desktop` command family exists: `desktop use` with the second account shows the sign-in
screen; the person signs in as the second account; `desktop status` shows `D` with that account's identity;
`desktop use` with the original account brings back the original account with its chats. Each quit and launch
latency is recorded with `date`, and with the Code tab open in each profile: whether it works, whether its
hosted process (path under `D/claude-code/`) appears in the status's family count, and whether it is gone
after the quit (helpers under the bundle are not a proxy for it). The result is appended to this record.
