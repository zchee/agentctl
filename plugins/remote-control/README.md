# agentctl-remote-control

A Claude Code mod that restarts a session's Remote Control bridge after
`agentctl claude use --live <id> --restart-remote-control` (or `--undo
--restart-remote-control`) has changed the account under it. Claude Code drops
the bridge by itself when the credential changes; this mod starts the new one
inside the same process, with the same session id and transcript, without
typing into the terminal. It also works on its own through `/agentctl-rc`.

## Install

Typed at the prompt of a terminal Claude Code session:

```
/plugin install agentctl-remote-control --marketplace zchee/agentctl
```

Answer `y` to add the `zchee/agentctl` marketplace, then pick a scope (the
user scope makes it load in every session started afterwards). Each session
that should be reconnected by agentctl needs the mod loaded.

## Requirements and limits

- Claude Code 2.1.287 or newer. On an older release the mod stays inactive:
  it registers only `/agentctl-rc`, which says why.
- macOS only, like agentctl's credential swap. At session start the mod checks
  its own registry record with `/usr/bin/stat -f '%u %Lp %HT'`; any other
  answer (a Linux `stat` among them) keeps it inactive.
- First-party sessions only. A session behind a gateway or a third-party
  provider, or one whose credential comes from `CLAUDE_CODE_OAUTH_TOKEN`,
  `ANTHROPIC_API_KEY` or `ANTHROPIC_BASE_URL`, answers `unavailable` and runs
  nothing.

## Commands

- `/agentctl-rc` prints whether this session's registry record shows a
  Remote Control bridge, how many distinct bridges the mod has seen it hold
  (its generation), the surfaces the session draws on, the Claude Code
  release, and whether each environment variable the session's keychain
  item name derives from is set. It never prints a bridge id, a URL or a
  directory spelling.
- `/agentctl-rc reconnect` asks for the same guarded reconnect agentctl asks
  for and answers `requested`. The result appears as a toast when it is
  final: reconnected, already connected, unavailable, not confirmed, or
  expired.

## How agentctl talks to the mod

agentctl writes `<id>.request.json` (mode 0600, written to a temporary name
and renamed) into `<configHome>/agentctl/remote-control/<pid>/`, a 0700
directory it creates under the session's own configuration home, where
`<pid>` is the session's process id. Every 500 ms the mod lists that
directory, checks each request (name, real path, owner uid and mode `600`
through `/usr/bin/stat`, contract version, action, expiry, id, subject),
writes `<id>.ack.json` once, and writes `<id>.response.json` once the answer
is final. A `status` request is answered at once with bridge presence and
generation, surfaces, the release, and the provenance above. A `reconnect`
request runs `/remote-control` only when the session is idle and its registry
record, read on the same tick, shows no bridge; it then watches the record
for up to 30 seconds for a new bridge. Requests live in the session's
`$.state`, so a hot reload of the mod neither answers one twice nor runs
`/remote-control` twice. A refused request is remembered in `$.state` too, in
a list of at most 256 entries, so a file answered `busy` while the mod's table
was full is not admitted later, after a reload included. The bound has one
residual: when 256 unexpired refusals are held, the newest is still answered
and recorded by forgetting the oldest, and a file of that forgotten id still
in the directory is examined again; it can be answered a second time and, if
it was refused only as `busy` and has not expired, admitted. The mod creates
no directory, deletes nothing, and never reads the `*.key` files beside the
registry records. The full contract is in `docs/research/remote-control-mod.md`
at the repository root.

## The remaining race

The mod issues `/remote-control` only from a poll tick on which no turn is
running and the registry record shows no bridge, so the command runs within
milliseconds of that observation. A person who types `/remote-control` in
that same instant can still start a bridge first; the mod's command then
finds a connected bridge and Claude Code opens its Remote Control dialog
(Disconnect, Show QR code, Continue), which Escape dismisses without
disconnecting. The mod cannot close this window: Claude Code offers no way to
issue the command conditionally.

## Development

```
claude plugin validate --strict plugins/remote-control
claude plugin test plugins/remote-control
```

`tsconfig.json` extends `.claude-plugin/types/tsconfig.json`, which Claude
Code writes when it loads the mod from a folder it hot-reloads (for example
`claude --plugin-dir plugins/remote-control`); that directory is not
committed.
