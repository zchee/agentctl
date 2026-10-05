# Handoff: agctl → Go port, execution session 2 (restart mid-wave)

- Written: 2026-10-05 20:59:52 JST (from `date` in the command that listed the tree)
- Session: https://claude.ai/code/session_01D9CqpUnLvb5A1R5ThNPgzK (lead on Claude Fable 5.1)
- Supersedes: `docs/handoffs/agctl-go-port-execution-handoff.md` (its "Next action at
  resume" items 1–3 are done; item 4 is the standing rule below).
- Reason for the restart: the user asked for it after `claude-status` reported an API
  rate-limit failure (`idleReason: failed`); five lanes were still alive at that moment.

## State of the repository

- Branch `main` in sync with `origin/main` at `c18eca1`. 98 commits since the approval
  commit `add5eff`; every one gpg-signed, subject ≤ 72 columns, Codex + Fable + session
  trailers on lane commits (lead's docs commits carry Fable + session only).
- **The working tree holds uncommitted work of lanes that were alive at the restart.** Do
  not discard it (`git checkout`/`reset`/`clean` are forbidden until each owner has
  judged it). Owners and files:
  - `runtime-core`: `internal/runtime/cleanup/{cleanup,cleanup_test}.go` (modified: FIFO
    order per the reference), `internal/runtime/signals/signals.go` + `main.go`
    (modified) and new `internal/runtime/signals/{constants_test,deferral,
    deferral_process_test,deferral_release,deferral_release_test,deferral_test,
    deferral_testing,deferral_testing_test}.go` (the forced-exit path: after the 10 s
    deferral exit 128+n WITHOUT `Purge()`, warning logged; tagged shortened-deferral
    test). The lane had said it was applying exactly these two decisions.
  - `claude-file-store`: `internal/secret/audit.go` (modified) + new
    `internal/secret/audit_required_test.go` (required-member refusal for the
    lock-break audit record, divergence 5 of its report). Shutdown was requested and
    then held back for this one commit.
  - `claude-peer-locks`: new `internal/secret/claude_lock_acquire.go`,
    `internal/secret/claude_lock_anchor.go` (acquisition protocol, task 3 of 5). The
    file declared `Acquire`, colliding with `namespace_lock.go`; the lane was told to
    rename it (e.g. `AcquirePeerLocks`) and had not confirmed.
  - `README.md` (one line, pre-existing, untracked on purpose).
- Plan: `docs/plans/agctl-go-port.md` (mirrored to `.omc/plans/`), status "approved;
  P1 in progress" header is stale — section 8 phase/wave tables are the truth (P0–P2 ✅,
  P3 W3 ✅ lanes + verified/reviewed, W4 running, W5/W6 partly landed early).
- Team state: `.omc/state/sessions/271dafaa-2e88-4c61-8f7a-9ecb771acef0/team-state.json`
  (`workers` / `workers_done` lists). New session: write fresh state under its own id.

## Where every wave stands (details with hashes in plan section 8)

| Wave | Status | Open items |
|---|---|---|
| W1 foundation | ✅ CLOSED (`verify-w1-close` at `5aa6701`) | — |
| W2 spikes | ✅ 4 GO (memguard conditional: `secret.EnsureLockedMemoryBudget()`) | — |
| W3 read path | ✅ lanes done; verified (1 blocker) + reviewed (APPROVE, 2 moves done in `fix-w3`) | the exact-byte LF printer in `internal/render` → assigned to `claude-status` |
| W4 status | 🔶 | `claude-token-refresh` ✅ (`6a12fba` `f04ec63` `3ba4f21`); `claude-accounts-read` landed `ad27ad8` `c18eca1`, txtar `testdata/script/e2e_accounts.txtar` NOT yet landed; `claude-status` landed `6684d14` `2d3fc1e` `a663fae`, still owed: `ClaudeStatus` field in `internal/app/handlers.go` + one-line `main.go` wiring to `app.Handlers(...)`, the render printer + rewiring of `internal/render/golden_test.go:167-222`, `testdata/script/e2e_status.txtar`, swap of its stdlib pass runner for `internal/runtime/coordinator` |
| W5 file store (early) | 🔶 | `claude-file-store` 5 commits landed; one commit pending in the tree (above) |
| W6 peer locks (early) | 🔶 | `claude-peer-locks` landed `a646c57` `9aeb8f0` `32dfbef`; tasks left: acquisition protocol (in tree, needs the rename), `config_lock` |
| runtime (early) | 🔶 | `runtime-core` 6 commits landed; FIFO + forced-exit commit pending in the tree |
| W5 rest, W7, W8, W9 | 🔜 | not started |

## Decisions taken this session (all in plan sections 2 / 17 / 19; do not re-ask)

Plan approved via team · worker model = agent frontmatter (`claude-gpt-6-astra-fast[1m]`;
most lanes self-identify as Fable 5, user said continue) · Codex trailer on lane commits ·
W2 parallel with W1 · W3 before W1 fully closed · early start of file-store / peer-locks /
runtime-core · golden names strip only the `agctl__` prefix · memguard conditional GO with
a fatal startup budget check (64 pages) · forced signal exit after the 10 s deferral skips
`Purge()`; cleanup callbacks FIFO as the reference · `Tree` type owned by peer-locks with
file-store's spelling (`TreeOwn`="agctl", `TreeLive`="live") · `internal/lockfile` leaf
package (config→secret would cycle) · `KeychainLineLimit` lives in `internal/secret` ·
`internal/app/handlers.go` created by the first lane that needs it, others add fields.

## Rules the next lead must keep (additions to the previous handoff's list)

- Lanes share ONE index: commit with `git commit --gpg-sign -F "$MSG" --only -- <paths>`,
  format with path-scoped `gofumpt`/`goimports-rereviser`, never `.`. A lane that leaves
  a non-compiling file on disk breaks every other lane's gate; peer-locks did it three
  times — say it in every brief.
- Lanes that refuse to commit on `main` ("branch first" harness rule): tell them the user
  authorised direct commits; if they still refuse (spike-httptrace did), have them send
  the file list + commit texts, re-run their package gates yourself, commit with
  `--only`.
- Verify/review lanes run on a `git archive HEAD` snapshot in their scratch dir because
  the live tree always has in-flight files.
- Everything else from `docs/handoffs/agctl-go-port-execution-handoff.md` ("Rules the
  next lead must keep") still applies: `team-lead` address, shutdown in the acceptance
  turn, `/commit` + push per task with the `exit 1` subject gate, no plan markers in
  code or commit messages, timestamps from `date`, absolute paths, stray `.omc` check.

## Next action at resume

1. Read this file, plan section 8, and `git status --short`. Confirm no lane is alive
   (`ListAgents`); the five above are dead with the old session.
2. Spawn three **resume** lanes (`executor`, names `runtime-core-2`,
   `claude-file-store-2`, `claude-peer-locks-2`) whose first instruction is: inspect the
   in-flight files listed above, finish exactly the pending commit(s) described, run the
   package gates, commit with `--only`, push. Peer-locks-2 then does `config_lock`
   (task 5) and its final report.
3. Spawn `claude-status-2` (resume: handlers field, `main.go` wiring, render printer +
   golden test rewiring, `e2e_status.txtar`, coordinator swap) and
   `claude-accounts-read-2` (resume: `e2e_accounts.txtar`, final report). Give both the
   same ownership map as before (plan section 8 W4 row + the lane briefs in the
   `/private/tmp/claude-501/.../271dafaa-.../scratchpad/w1-briefs.md` style; the briefs
   themselves live only in the old session's transcript).
4. When W4 lands: `verify-w4` + `review-w4` on a snapshot, fix lane, then W5
   (`claude-watch`, `claude-login` with PKCE/loopback/exchange, `claude-import-accounts-write`)
   per plan section 8; `claude-file-store` is already done, so W5 has three lanes.
5. Update plan section 8 and the changelog at every boundary; mirror to `.omc/plans/`;
   commit; push.

## Addendum (2026-10-05 21:02:05 JST): what the dying lanes reported after the restart decision

- **Root cause of the lane failures: the account's session limit** ("You've hit your
  session limit · resets 9:20pm (Asia/Tokyo)"), not a per-request rate limit. Start the
  new session after 21:20 JST, and keep the lane count lower (about five) at first.
- **Ready for a lead commit (gated by their owners, uncommitted because both lanes
  refuse to commit on `main`):**
  - runtime-core: FIFO cleanup (`internal/runtime/cleanup/*`) and the forced signal
    exit (`internal/runtime/signals/signals.go`, new `deferral*.go`,
    `constants_test.go`, `main.go` lines for `controller.Execute`). Scoped race
    tests pass on both tags (count=3 tagged), lint 0, vet clean, binary scan proves the
    deferral env literal is absent from the release build. Commit subjects to write
    by the lead; keep `main.go`'s `app.Handlers` wiring (claude-status's,
    also uncommitted) in the same tree state.
  - claude-file-store: `internal/secret/audit.go` (32 lines, all 10 required
    lock-break members + 3 per sample refused when missing/null) and
    `internal/secret/audit_required_test.go` (40 cases). Both tags pass for those
    tests; zero diagnostics.
- **Two defects found, unfixed:**
  1. `testdata/script/cli_smoke.txtar:63` still expects `claude status` to exit 1
     (unwired handler); with `app.Handlers` wired it exits 2 (unreadable accounts).
     The status lane must update that expectation when it lands the wiring.
  2. `internal/secret/audit.go`: concurrent first creation of the audit log returns
     ENOENT from `OpenAuditLogAt` (reproduced 50× with `-race` on the unchanged
     file; test `TestAuditConcurrentAppendersNeverInterleaveHalfLines` loses one of 16
     writers). Pre-existing in `0e2c63c`/`db49114`; fix in the file-store resume lane.
- claude-peer-locks: `Acquire` was renamed `AcquirePeerLocks` in the tree (uncommitted);
  its in-flight files still fail vet (`LockSubject` undefined, duplicate
  `selfStartIdentity`) at the moment of the restart. Also seven pre-existing lint
  findings in the secret package (ptr inline calls, unused `reasonPtr`, De Morgan)
  outside any lane's diff; give them to the peer-locks resume lane.
- Resume order correction: land runtime-core's and file-store's ready commits FIRST
  (one small lead or executor lane running their gates), then status (handlers field,
  `main.go`, printer, smoke fix, txtar), then accounts txtar, then peer-locks.
