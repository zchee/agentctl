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
- Acknowledgement `<id>.ack.json`, written by the mod at most once, immediately after validation:
  `{"v":1,"id","action","state":"accepted"|"rejected","reason"?,"answeredAt"}` with reasons `name`, `expired`,
  `duplicate`, `metadata`, `action`, `subject`, `version`.
- Response `<id>.response.json`, written by the mod exactly once per accepted request, when final:
  `{"v":1,"id","action","result","answeredAt","bridge":{"present":bool,"generation":n},"surfaces":[...],
  "version":"<base>","remoteControlListed":bool,"provenance":{...},"reason"?}` (`remoteControlListed` is whether `remote-control` is in `$.command.list()` at answer time; a `status` answer with it false classes the session `unavailable` at preflight). Results: `status` → `ok`; `reconnect` → `reconnected`,
  `already_connected`, `unavailable`, `not_confirmed`, `expired`, `cancelled`.
- Mod state per request, kept in `$.state` under the mod's name (`requests: {id: {phase, action,
  expiresAt, runStartedAt?, observeUntil?, terminal?: {response, published: bool}}}`, bounded at 64 entries,
  oldest `published` entry evicted first). Admission and recovery are distinct: the unseen-id check applies
  only to a file whose id is not in the table; an id in the table is recovered at its recorded phase, never
  re-validated. Phases: `accepted` → (`waiting_idle` | `running` | `final`) → `published`. Every transition
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
     a rewrite after a reload writes the identical bytes (idempotent republication).
  - Reload: `session.start` reloads the table and resumes each entry at its phase: `running` entries are never
    run again and are observed against their stored `observeUntil`; `final` entries with `published: false`
    are republished; `waiting_idle` and `accepted` entries re-enter the rule list. The ack is written at
    admission only; a reload between admission and the ack write republishes the ack the same way (an
    `ack.published` flag).
- agentctl side: an ack or response file is read only when `Lstat` shows a regular file (never a symlink)
  owned by agentctl's own uid, through the bounded reader already used for registry records; its mode is
  not checked because the response carries no secret. A file is used only if its `v`, `id` and `action` equal the request's; an ack with
  `state: rejected` ends the wait and maps its reason (`metadata` → `metadata_rejected`, `version` →
  `version_rejected`, every other reason → `unreachable`); a response whose `result` is not one of the
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
  in the future, `id` unseen; `subject.service` present. Any failure writes a `rejected` ack (when the name
  and path checks passed) or ignores the file (when they did not).
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
