# Remote Control restore through a Claude Code mod

agentctl's live account swap (`agentctl claude use --live <id>`, and `--undo`) changes the Claude Code
credential under running sessions, and Claude Code then drops each session's Remote Control bridge by itself.
With `--restart-remote-control`, agentctl asks the `agentctl-remote-control` mod (in `plugins/remote-control/`
of this repository) running inside each bridged session to start a new bridge in the same process, session id
and transcript, without typing into any terminal. The two halves talk through per-session request,
acknowledgement and response files under `<configHome>/agentctl/remote-control/<pid>/`, where `configHome` is
the directory that holds the session's registry record directory `sessions/` and `<pid>` is the session's
process id. On the mod side the only host commands are `/usr/bin/id -u` and
`/usr/bin/stat -f '%u %Lp %HT' <path>`, used to check that a request file is a regular file owned by the
session's uid with mode `600`. This file records the frozen contract between the two halves, the rule that
decides which sessions are eligible, and, in later sections, the verification record.

## Transport contract

- Directory: `<configHome>/agentctl/remote-control/<pid>/`, where `<pid>` is the session's process id and
  `configHome` is the session's configuration home: a non-empty `CLAUDE_CONFIG_DIR` as spelled, else
  `$HOME/.claude`, NFC-normalized. On the agentctl side it is the parent of the session registry directory
  `<configHome>/sessions`; on the mod side it is derived the same way from the session's own environment
  and must hold the mod's own registry record `<configHome>/sessions/<pid>.json`, with the pid taken from
  `CLAUDE_CODE_MESSAGING_SOCKET` (`/<pid>.sock`) and cross-checked against the record's `pid` and
  `messagingSocketPath`. agentctl creates `agentctl/` and `remote-control/` with mode 0700 and `<pid>/`
  with 0700, refusing a path whose `Lstat` is a symlink or not owned by its own uid. The mod never creates it;
  a missing directory means no request.
- Request file `<id>.request.json`, `id` = 32 lowercase hex characters from `crypto/rand`; written by agentctl
  to `<id>.request.json.tmp` with mode 0600 and renamed into place, so the mod never reads a partial file.
  Body (`v` is the contract version):
  `{"v":1,"id":"<id>","action":"status"|"reconnect","issuedAt":<ms epoch>,"expiresAt":<ms epoch>,
  "subject":{"service":"<keychain service name>"}}`. Lifetimes: `status` 3 000 ms, `reconnect` 75 000 ms.
  The mod refuses, with reason `expired`, a request whose `expiresAt` lies more than 600 000 ms after the
  moment it reads the file.
- Acknowledgement `<id>.ack.json`, written by the mod at most once per admission, immediately after validation:
  `{"v":1,"id","action","state":"accepted"|"rejected","reason"?,"answeredAt"}` with reasons `name`, `expired`,
  `duplicate`, `metadata`, `action`, `subject`, `version`, `busy`. A rejected ack may carry `action: ""` when
  the body could not be read (reasons `name`, `metadata`, `version`, `busy`); the mod writes `""` for
  `metadata`, `version` and `busy`, and the request's own action for every reason it learns after reading
  the body (`expired`, `duplicate`, `action`, `subject`, `name`). An accepted ack always carries the
  request's action.
- Response `<id>.response.json`, written by the mod exactly once per accepted request, when final:
  `{"v":1,"id","action","result","answeredAt","bridge":{"present":bool,"generation":n},"surfaces":[...],
  "version":"<base>","remoteControlListed":bool,"provenance":{...},"reason"?}` (`remoteControlListed` is whether `remote-control` is in `$.command.list()` at answer time; a `status` answer with it false classes the session `unavailable` at preflight). Results: `status` → `ok`; `reconnect` → `reconnected`,
  `already_connected`, `unavailable`, `not_confirmed`, `expired`, `cancelled`.
- Provenance in the response: `{"home","configDir":{"set":bool,"value"?},"secureStorageDir":{"set":bool,
  "value"?},"oauthTokenSet","apiKeySet","baseUrlSet","authorized"}`. In `configDir` and `secureStorageDir`,
  `value` is required only when `set` is true; for an unset variable it may be absent, and agentctl decides
  unset from `set`, never from the value. A response with `{"set":false}` and no `value` is complete. The mod
  writes an empty string for an unset variable, which agentctl ignores.
- Mod state per request, kept in `$.state` under the mod's name (`requests: {id: {phase, action,
  expiresAt, runStartedAt?, observeUntil?, terminal?: {response, published: bool}}}`). The table holds
  admitted requests only and is bounded: when 64 entries are pending (not `published`) or the table holds
  1 024 entries, a new request file whose name, path and metadata checks passed gets a `rejected` ack with
  reason `busy`, without its body being read, and `/agentctl-rc reconnect` answers that too many requests
  are pending. An entry leaves the table only once it is `published` and its `expiresAt` has passed; nothing
  is evicted before its `expiresAt`, published or not, so a request file still lying in the directory
  cannot be admitted, or run, a second time. Once its entry has left the table, such a file can only be
  refused (as `expired`, which answers it a second time).
- Refused files, kept in `$.state` under the mod's name as a list, oldest first (`rejected: [{id, reason,
  expiresAt, ack?}]`, `ack` holding the acknowledgement text until it is written). Every rejection the mod
  answers, `busy` included, is recorded there before its ack is written, and a file whose id is listed is
  never admitted or validated again; after a reload the list is read from `$.state` before the first
  admission, so a refused file is not admitted later even once capacity frees. A refusal whose body was not
  read or not trusted (`metadata`, `version`, `busy`) and a `name` or `expired` refusal are kept until 600 000
  ms after the refusal, the longest lifetime a request may ask for, so a file still lying there afterwards can
  only be refused as `expired`; an `action`, `duplicate` or `subject` refusal is kept until the request's own
  `expiresAt` when that is valid, else for 600 000 ms. The list holds
  at most 256 entries, and an entry is evicted only to make room: the oldest one whose `expiresAt` has passed
  first. When all 256 are unexpired, the newest rejection is still answered and recorded by evicting the
  oldest entry; a file of the evicted id still in the directory is examined again on a later tick and can be
  answered a second time, and, if it was refused only as `busy` and has not expired, admitted. An unwritten
  ack in the list is retried with the identical bytes on every tick, across reloads.
- Admission and recovery are distinct: the unseen-id check applies only to a file whose id is in neither the
  table nor the refused list; an id in the table is recovered at its recorded phase, never re-validated. Phases: `accepted` → (`waiting_idle` | `running` | `final`) → `published`. Every transition
  is written to `$.state` before the side effect it enables (the run, the file write). Rules, evaluated in
  this order on every poll tick (500 ms) for each non-published `reconnect` entry, and once immediately at admission (a `status` request is answered at admission and has no rule list). An entry in `running` is subject to rule 6 only, never to rules 1 to 5, so a run is issued at most once per id:
  1. `expiresAt` passed and no run issued → `final(expired)`.
  2. `remote-control` absent from `$.command.list()`, or `authorized` false → `final(unavailable)`, no run.
  3. The record read on this tick shows a non-empty `bridgeSessionId` and no run was issued →
     `final(already_connected)`, no run (the mod never waits for a connected bridge to drop; agentctl waits
     for the drop before sending).
  4. A turn is running (tracked by `turn.start`/`turn.complete`, cross-checked with the record's `status`)
     → `waiting_idle`.
  5. Otherwise (idle, no bridge id, not expired, command listed) → write `running` with `runStartedAt =
     now`, `observeUntil = now + 30 000`, then call `$.command.run`; a rejection → `final(unavailable)`.
  6. In `running`: a non-empty `bridgeSessionId` → `final(reconnected)`; `observeUntil` passed →
     `final(not_confirmed)`; the registry record missing or its `pid`/`sessionId` changed →
     `final(not_confirmed)`.
  7. `final(x)`: the terminal response payload is stored in `$.state` (`terminal.response`), then written to
     `<id>.response.json`, then `published: true` is stored. The response content is immutable once stored;
     a rewrite after a reload writes the identical bytes (idempotent republication). Publication, of the
     ack and of the response alike, advances only after the file write succeeded: while the transport
     directory is missing the entry stays pending and the identical stored bytes are tried again on a later
     tick.
  - Reload: `session.start` reloads the table and resumes each entry at its phase: `running` entries are never
    run again and are observed against their stored `observeUntil`; `final` entries with `published: false`
    are republished; `waiting_idle` and `accepted` entries re-enter the rule list. The ack is written at
    admission only; a reload between admission and the ack write republishes the ack the same way (an
    `ack.published` flag).
- agentctl side: an ack or response file is read only when `Lstat` shows a regular file (never a symlink)
  owned by agentctl's own uid, through the bounded reader already used for registry records; its mode is
  not checked because the response carries no secret. An accepted ack and every response are used only if
  their `v`, `id` and `action` equal the request's; a rejected ack is used when its `v` and `id` match and
  its `action` is the request's or empty. An ack with
  `state: rejected` ends the wait and maps its reason (`metadata` → `metadata_rejected`, `version` →
  `version_rejected`, `busy` and every other reason → `unreachable`); a response whose `result` is not one of the
  allowed arms for the action, or lacks a required field, is treated as "not yet" and retried until the
  deadline, as is any unparsable or truncated ack or response; a response that arrives before its ack is
  accepted. agentctl waits for the ack up to the status lifetime and for the response up to the request's
  lifetime, polling every 250 ms; a missing final answer at the deadline is `not_confirmed` (never success).
  Result to count: `reconnected` → `reconnected`; `already_connected` → `already_connected`; `unavailable` →
  `unavailable`; `not_confirmed`, `expired`, `cancelled` and no answer → `not_confirmed`. Warnings are
  computed from the counts plus the multiset of result words (no session identity), so the `not_confirmed`
  warning can say how many were `expired`. agentctl deletes its own request, ack and response files when
  done, and on start sweeps files in its own `<pid>/` directories older than 24 h; the mod deletes nothing
  (no remove API) and answers a late request only through the table above.
- Validation on the mod side, in order: file name matches `^[0-9a-f]{32}\.request\.json$`; `$.fs.stat(path,
  {resolve:true}).realPath` starts with the resolved transport directory; the mod's only two host commands,
  both by absolute path and never through a shell (`/usr/bin/id -u`, run once at session start, and
  `/usr/bin/stat -f '%u %Lp %HT' <path>`, whose answer must be `<uid> <octal mode> Regular File`; any
  failure or other shape fails closed), answer own uid, mode `600`, regular file; JSON parses with `v: 1`, known `action`, `expiresAt`
  in the future and at most 600 000 ms ahead, `id` unseen; `subject.service` present. Any failure writes a
  `rejected` ack (when the name and path checks passed) or ignores the file (when they did not). A full
  table answers `busy` only after the name, path and metadata checks passed, and before the body is read;
  a file that fails the metadata check gets its `metadata` rejection, never `busy`.
- Cancellation: SIGINT to agentctl during the post-swap phase cancels the context; sessions still pending are
  counted `not_confirmed`, a counts-only note is written to stderr, and the signal exit path is unchanged
  (the reference rule). The mod finishes or expires its request on its own.

## Provenance truth table

The mod answers `status` with `provenance`: `{home, configDir: {set, value}, secureStorageDir: {set, value},
oauthTokenSet, apiKeySet, baseUrlSet, authorized}` where `authorized` is `$.session.authorize() !== null`.
Values are directory spellings, never secrets. agentctl builds `claude.EnvView{Home, ConfigDir, SecureStorageDir,
OAuthTokenSet}` from it and derives `ServiceName`.

| Session environment | Derived service | Eligible for a swap whose subject service is `S` |
|---|---|---|
| `CLAUDE_SECURESTORAGE_CONFIG_DIR` unset, `CLAUDE_CONFIG_DIR` unset | `Claude Code-credentials` | iff `S` is the unsuffixed live service |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR` unset, `CLAUDE_CONFIG_DIR=<Home>/.claude` explicitly (absolute; `~` is not expanded by `configHome`, so a literal `~/.claude` is a different directory and is tested separately) | `Claude Code-credentials-<sha8 of the NFC spelling>` | iff `S` equals that suffixed name (this row and the first share the registry under `<Home>/.claude/sessions` and differ in service; the test asserts both) |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR` unset, `CLAUDE_CONFIG_DIR=""` (set, empty) | unsuffixed (`configDirOrDefault` keeps the empty string, `configHome` falls back to `<Home>/.claude`) | iff `S` is unsuffixed |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR=""` (set, empty) | unsuffixed | iff `S` is unsuffixed |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR=<dir>` | suffixed by `<dir>` | iff `S` equals it |
| `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY` or `ANTHROPIC_BASE_URL` set, or `authorized` false | n/a | never (`provenance_skipped`): the session does not read the keychain item, or runs behind a gateway or a third-party provider |
| response missing or unparsable | n/a | `unreachable`: with `--restart-remote-control` the swap is refused before any keychain read with reason `remote_control_unreachable`, exit 30, nothing written |

## Divergences from the reference

The Rust `agctl` restarted Remote Control by typing into tmux panes after a per-pane human attestation, and
disconnected each bridge before the swap. This port asks a mod inside each session instead, so every part of
the reference that existed to make keystrokes safe is gone, and its JSON and refusal contract differ.

- **Count set.** The reference's `remote_control` object had eleven counts: `eligible`, `disconnected`,
  `reconnected`, `restored`, `not_disconnected`, `not_confirmed`, `skipped`, `gone`, `already_connected`,
  `not_attested` and `attestation_declined`. This port has twelve: `eligible`, `provenance_skipped`,
  `unreachable`, `unavailable`, `version_rejected`, `metadata_rejected`, `dropped`, `not_dropped`,
  `reconnected`, `not_confirmed`, `already_connected` and `restored`. The six reference names that are gone
  (`disconnected`, `not_disconnected`, `skipped`, `gone`, `not_attested`, `attestation_declined`) describe a
  pre-swap disconnect, a pane check or a human answer; none of those happens without keystrokes, so none is
  observable, and permanent zeros would read as measurements. The seven new names report what the mod path
  can observe: which sessions answered and read the swapped item, why a session was not asked, and whether
  the old bridge disappeared before a request was sent. The object is still present only when the flag is
  given. Two shared names changed meaning: `reconnected` is a new bridge in the session's registry record
  within the mod's 30-second observation window (the reference required 25 seconds of observed connection
  after typing), and `restored` counts sessions that still show a bridge after a pass that changed no
  credential, without any input.
- **No terminal requirement, no `--yes` conflict.** The reference refused with `remote_control_needs_tty`
  unless stdin and stderr were terminals, and rejected `--yes` with the flag, because each input group needed
  a fresh `y` typed by the person for that pane. With no per-pane attestation, both rules are lifted: the
  flag works with `--json` on a pipe and with `--yes`, and the swap's own consent still gates `--yes`.
- **Refusal reasons.** Exit 30 keeps the reference's meaning that no swap was made and nothing was written,
  under the exit-table name `remote_control_refused`. Two reasons exist now:
  `remote_control_unsupported_platform` (a platform other than macOS, decided before anything is read) and
  `remote_control_unreachable` (a session with Remote Control on did not answer the status request within
  the 5-second preflight or refused it for a reason other than the request file's owner, mode or the
  release, such as `busy`; or the session registry could not be read, so no session's eligibility can be
  established). The reference's `remote_control_not_disconnected` (a pane failed to disconnect before the
  swap) has no counterpart because nothing is disconnected.
- **No disconnect step.** The reference typed `/remote-control` and then `Up Up Enter` in each pane before
  the swap, on the theory that a bridge disconnected first keeps its conversation on claude.ai. Claude Code
  offers no programmatic disconnect (only the interactive dialog), and the isolated experiment on 2.1.292
  measured the bridge disappearing about 2 seconds after the credential changed, with Claude Code's own
  message naming the account change. agentctl therefore waits up to 45 seconds after the swap for the old
  bridge to disappear and sends a request only then; a bridge still present gets no request
  (`not_dropped`). Whether the conversation from before the swap appears on claude.ai under the new account
  is not verified by either design.
- **Timing.** The reference ran one stage of at most 30 seconds that included the per-pane questions, then
  waited an empirical 25 seconds before typing again. This port has a preflight bounded at 5 seconds inside
  the swap deadline and before the keychain read (each status request lives 3 seconds), and a follow-up that
  runs after every lock is released, outside the swap deadline, under its own 90-second cap: the drop wait
  of 45 seconds, then a reconnect request that lives 75 seconds so a session can finish a running turn.
- **Version floor.** The reference had a provisional floor and last-verified value of 2.1.281 and warned and
  proceeded on newer releases. This port has a single floor, 2.1.287, the oldest release whose mod API the
  mod is built against; agentctl checks the registry record's `version` before sending anything and the mod
  checks its own release, and there is no ceiling warning. The operator run below is repeated on each
  Claude Code release that agentctl claims to support.
- **Scope.** The reference acted only on live-store sessions whose registry records named a tmux pane. This
  port acts on any session with the mod loaded, in any terminal, and decides whether a session reads the
  swapped item from the environment the mod reports (the provenance truth table above) instead of a human
  attestation.

Two behaviours have no reference counterpart and were settled during implementation:

- **`/clear` keeps the poll running.** Claude Code fires `session.end` with reason `clear` when `/clear` ends
  a conversation, but the process, its registry record and its transport directory stay, and no
  `session.start` follows. The mod therefore keeps its 500 ms poll armed on that reason, so the session stays
  reachable for its next conversation. Every other end reason cancels the poll and finishes each pending
  request as `cancelled`, which agentctl counts as `not_confirmed`.
- **Test timing seam.** A build with the `agentctl_testing` tag reads `AGENTCTL_REMOTE_CONTROL_TIME_SCALE`,
  the number of milliseconds that stand for one second of the drop wait, the reconnect lifetime and the
  follow-up cap; values outside 1 to 999 are ignored. The preflight and status waits stay real, because the
  refusal they decide is what the tests assert. A release build has no such variable and runs at one second
  per second; the release gate lists the variable and its source file among the seams that must be absent.

## Operator run

This procedure checks the production mod against a real Claude Code release, with the model gateway off, in
a tmux session that uses an isolated configuration directory and therefore an isolated keychain item. It
never touches the unsuffixed `Claude Code-credentials` item, the real agentctl store or a running session.
It needs two owned accounts, A and B, already logged in to the real store. Run every command from the
repository root.

1. Set the paths and identifiers (`A`/`AO` and `B`/`BO` are the account and organization ids of the two
   accounts in the real store):

   ```sh
   BASE=/Users/zchee/go/src/github.com/zchee/agentctl/.omc/artifacts/rc-run
   RUN="$BASE/run"
   BIN="$BASE/agentctl"
   STORE="$BASE/store"
   MOD=/Users/zchee/go/src/github.com/zchee/agentctl/plugins/remote-control
   CLAUDE_BIN=/Users/zchee/.local/share/claude/versions/2.1.296
   CFG="$STORE/claude/$A/$AO"
   mkdir -p "$RUN" "$BASE/work" "$BASE/cfg"
   ```

2. Build an untagged binary and check the mod:

   ```sh
   GOENV=off GOTOOLCHAIN=go1.27.2 go build -o "$BIN" .
   claude plugin validate --strict "$MOD"
   claude plugin test "$MOD"
   ```

3. Build the experimental store: a registry holding only the owned entries of A and B, each pointed at its
   copied directory (the paths are ASCII, so their NFC spelling is their bytes), then opaque copies of the
   two owned directories (no credential bytes are printed):

   ```sh
   DA="$STORE/claude/$A/$AO"; DB="$STORE/claude/$B/$BO"
   mkdir -p -m 700 "$STORE" "$STORE/claude" "$DA" "$DB"
   jq --arg A "$A" --arg B "$B" --arg DA "$DA" --arg DB "$DB" \
     --arg SA "$(printf %s "$DA" | shasum -a 256 | cut -c1-8)" \
     --arg SB "$(printf %s "$DB" | shasum -a 256 | cut -c1-8)" '
     {version: 1, accounts: [.accounts[]
       | select(.kind.kind == "owned" and (.account_uuid == $A or .account_uuid == $B))
       | if .account_uuid == $A then .kind.export_spelling = $DA | .kind.export_sha8 = $SA
         else .kind.export_spelling = $DB | .kind.export_sha8 = $SB end],
      forgotten_services: []}' "$HOME/.config/agctl/config.json" >| "$STORE/config.json"
   chmod 600 "$STORE/config.json"
   cp -Rp "$HOME/.config/agctl/claude/$A/$AO/." "$STORE/claude/$A/$AO/" && \
   cp -Rp "$HOME/.config/agctl/claude/$B/$BO/." "$STORE/claude/$B/$BO/" && \
   cp -Rp "$CFG/.credentials.json" "$BASE/cfg/.credentials.json"
   "$BIN" --config-dir "$STORE" claude accounts list
   ```

   The listing must show only A and B, with owned locations under `$STORE`.

4. Seed A into the isolated keychain item, whose service name is `Claude Code-credentials-` followed by the
   first eight hex digits of the SHA-256 of the NFC spelling of `$CFG`:

   ```sh
   env -u CLAUDE_CODE_OAUTH_TOKEN \
     CLAUDE_SECURESTORAGE_CONFIG_DIR="$CFG" CLAUDE_CONFIG_DIR="$BASE/cfg" \
     "$BIN" --config-dir "$STORE" claude use --live "$B" --yes
   cp -Rp "$HOME/.config/agctl/claude/$A/$AO/.credentials.json" "$CFG/.credentials.json"
   env -u CLAUDE_CODE_OAUTH_TOKEN \
     CLAUDE_SECURESTORAGE_CONFIG_DIR="$CFG" CLAUDE_CONFIG_DIR="$BASE/cfg" \
     "$BIN" --config-dir "$STORE" claude use --live "$A" --yes
   ```

5. Start the probe session with the production mod loaded from the repository. `env -i` keeps the model
   gateway's `ANTHROPIC_BASE_URL` out of it, so it talks to Anthropic directly:

   ```sh
   tmux new-session -d -s rcmod -x 160 -y 48 -c "$BASE/work" \
     "env -i HOME=$HOME USER=$USER LOGNAME=$LOGNAME \
      PATH=$HOME/.local/bin:/opt/homebrew/bin:/usr/bin:/bin TERM=xterm-256color \
      CLAUDE_CONFIG_DIR=$CFG DISABLE_OMC=1 CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 \
      $CLAUDE_BIN --plugin-dir $MOD --model sonnet --effort low --strict-mcp-config \
      --setting-sources user --tools \"\" --no-chrome --name rcmod-run"
   ```

6. In the session, send `Reply with the single word pong.`, then run `/remote-control` once and accept
   Claude Code's first-use prompt. `/agentctl-rc` must show a bridge present, generation 1, release
   2.1.296, and `authorized` true with no token, key or base URL override. Record the process, the session
   and the transcript inode:

   ```sh
   REC=$(grep -l '"rcmod-run"' "$CFG"/sessions/*.json)
   jq -c '{pid, sessionId}' "$REC" >| "$RUN/identity-before.json"
   SID=$(jq -r .sessionId "$REC")
   stat -f %i "$CFG"/projects/*/"$SID".jsonl >| "$RUN/inode-before.txt"
   ```

7. Start a bridge sampler that records presence only, never the bridge id, once a second with `date`:

   ```sh
   while :; do
     printf '%s %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')" \
       "$(jq -r '(.bridgeSessionId // "") != ""' "$REC")"
     sleep 1
   done >> "$RUN/bridge.log" &
   SAMPLER=$!
   ```

8. Forward swap to B with the flag, timed by `date`:

   ```sh
   date '+%Y-%m-%d %H:%M:%S %Z' >> "$RUN/forward.times"
   env -i HOME=$HOME USER=$USER LOGNAME=$LOGNAME PATH=/opt/homebrew/bin:/usr/bin:/bin \
     CLAUDE_CONFIG_DIR="$CFG" \
     "$BIN" --config-dir "$STORE" claude use --live "$B" --restart-remote-control --yes --json \
     >| "$RUN/forward.json" 2>| "$RUN/forward.stderr"; echo "exit $?" >> "$RUN/forward.times"
   date '+%Y-%m-%d %H:%M:%S %Z' >> "$RUN/forward.times"
   jq .remote_control "$RUN/forward.json"
   ```

   Then send `Reply with the single word pong2.` in the session, run `/agentctl-rc`, and run
   `"$BIN" --config-dir "$STORE" claude status --account "$B" --no-cache --timeout 10s` with the same
   environment as the swap.

9. Undo with the flag, recorded the same way:

   ```sh
   date '+%Y-%m-%d %H:%M:%S %Z' >> "$RUN/undo.times"
   env -i HOME=$HOME USER=$USER LOGNAME=$LOGNAME PATH=/opt/homebrew/bin:/usr/bin:/bin \
     CLAUDE_CONFIG_DIR="$CFG" \
     "$BIN" --config-dir "$STORE" claude use --undo --restart-remote-control --yes --json \
     >| "$RUN/undo.json" 2>| "$RUN/undo.stderr"; echo "exit $?" >> "$RUN/undo.times"
   date '+%Y-%m-%d %H:%M:%S %Z' >> "$RUN/undo.times"
   jq .remote_control "$RUN/undo.json"
   ```

   Then send `Reply with the single word pong3.`, run `/agentctl-rc`, and repeat the identity and inode
   commands of step 6 into `identity-after.json` and `inode-after.txt`, then stop the sampler with
   `kill "$SAMPLER"`.

10. Check that no artifact holds a URL or the current bridge id. Run this while the bridge is present: the
    id is piped into `grep` and never printed, and both commands must print nothing:

    ```sh
    grep -rlE 'https?://' "$RUN"
    jq -r .bridgeSessionId "$REC" | grep -rlF -f - "$RUN"
    ```

11. Exit the probe session (`/exit`), then check the gateway case: with the model gateway running, start a
    second session the same way as step 5 with `ANTHROPIC_BASE_URL=http://127.0.0.1:18764` added to its
    `env -i` list and `--name rcmod-gateway`, run `/agentctl-rc` and `/agentctl-rc reconnect` in it, and
    exit it.

12. Clean up. The undo in step 9 put A back. Remove the experimental roots; the isolated keychain item stays,
    because agentctl has no documented operation that deletes a keychain item:

    ```sh
    tmux kill-session -t rcmod
    /bin/rm -rf "$STORE" "$BASE/cfg"
    ```

Assertions to record, each with the `date` output it was observed at:

- The forward swap exits 0 and its `remote_control` object shows `eligible: 1`, `dropped: 1`,
  `reconnected: 1`, and zero for every other count; its stderr has no Remote Control warning.
- The undo exits 0 with the same counts.
- `identity-before.json` equals `identity-after.json` and `inode-before.txt` equals `inode-after.txt`: the
  same process, session id and transcript across both swaps, and the transcript holds `pong`, `pong2` and
  `pong3`.
- `/agentctl-rc` reports generation 1 before the forward swap, 2 after it and 3 after the undo, and
  `bridge.log` shows present, absent, present, absent, present in that order.
- The drop latency (the swap's start time in `*.times` to the first absent sample) and the reconnect latency
  (the first absent sample to the next present one), for both directions, computed from the logged times.
- `claude status` names B as live after the forward swap.
- Both commands of step 10 print nothing.
- In the gateway session, `/agentctl-rc` shows the base URL override and `authorized` false, and
  `/agentctl-rc reconnect` ends in the toast `unavailable`, with no bridge started.
- Whether the conversation from before the swap appears on claude.ai under B is checked by hand and recorded
  as seen, not seen, or not checked.

The record of the run is appended below this section, with its `date` timestamps, once the run has happened.
