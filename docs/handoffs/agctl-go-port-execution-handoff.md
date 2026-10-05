# Handoff: agctl → Go port (plan complete, pending approval; next step is execution)

- Written: 2026-10-05 16:30:54 JST (from `date` in the command that listed `.omc/`)
- Session: https://claude.ai/code/session_019Gqsu3YqQoibgVecjoSu1o (lead on Claude Fable 5.1)
- Supersedes: `.omc/handoffs/agctl-go-port-plan-handoff.md` (planning handoff; every item in
  its "Next action at resume" was completed in this session).

## State of the repository

- Branch `main`, in sync with `origin/main` at `60b5e9a`. Working tree: only the pre-existing
  untracked `README.md` (one line) and `main.go` (licence header + `package main`). Neither is
  the lead's work; leave them untracked until the foundation lane owns `main.go`.
- Commits landed this session (all gpg-signed, subjects ≤ 72 columns, pushed):
  - `34585b7` docs/research: capture the read-only inventory of the Rust agctl source
  - `0957188` docs/research: record verified Go module versions and APIs for the port
  - `3a35bd7` docs/plans: record the Go port plan for agctl, pending approval
  - `60b5e9a` docs/plans: keep fixture scripts as the e2e fakes, pin the sysctl map
- Working copies (gitignored): `.omc/plans/agctl-go-port.md` == `docs/plans/agctl-go-port.md`
  (738 lines, byte-identical at this handoff); `.omc/research/*.md` == `docs/research/*.md`.
- No Go source written. No `go.sum`, no `internal/`, no CI workflow yet.
- All four P0 worker lanes (`inventory-modules`, `inventory-invariants`, `inventory-oracles`,
  `research-modules`) reported, were accepted, and acknowledged `shutdown_request`. No
  teammate is alive. No stray `.omc` directories (only the state root).
- `.claude/settings.local.json` holds only an empty `env` object.

## The plan (read it first)

`docs/plans/agctl-go-port.md`, status **pending approval**. Sections that drive execution:

| Section | What it fixes |
|---|---|
| 2 | every closed decision (modules, keychain, memguard, log grammar, testing endpoints fail closed, Rust implementation normative over comments) |
| 3, 3.1 | command surface, duration grammar, exit codes 0/1/2 and 10–30 with conditions |
| 4, 4.1 | Go package layout mirroring the Rust tree; pinned module versions |
| 6 | env vars (`AGCTL_*` → `AGENTCTL_*`), child-env allowlists, testing-only names |
| 7.1–7.5 | invariants, timing budgets, endpoints, rendering rules, oracle rules (Insta trim caveat) |
| 8 | phase table P0–P9 and wave table W1–W9 with lanes and exit gates |
| 9 | implementation steps with Rust `file:line` references |
| 10–12 | testing seams (`agentctl_testing` tag), expanded test plan, Go gate list |
| 13–16 | pre-mortem, risks, acceptance criteria, verifier steps |
| 17 | worker rules (astra codex agents, `team-lead` address, `/commit` + push per task) |
| 19 | the nine judgement calls and their answers |

## Decisions taken this session (already in the plan; do not re-ask)

Table rendering lipgloss/v2/table · explore/writer stay on luna-fast · security review on
astra `codex-security-reviewer` · planning artefacts mirrored to tracked `docs/` ·
Darwin proc via `unix.SysctlKinfoProcSlice` · secrets in memguard `Enclave` with momentary
`LockedBuffer` · `AGENTCTL_LOG` = slog levels only · tests: teatest/v2 (pseudo-version) +
testscript · over-long commit subjects amended and force-pushed with lease.

## Rules the next lead must keep

- Workers: Agent tool, distinct `name`, no `isolation`, no `model`; types from
  `~/.claude/agents/codex-*` (`executor`, `test-engineer`, `code-reviewer`, `verifier`,
  `security-reviewer`, `critic`, `architect`, `analyst`, `document-specialist`). Every lane
  self-reports its model on line 1; all four P0 lanes reported `claude-gpt-6-astra-ultrafast[1m]`.
- Workers deliver reports with SendMessage to **`team-lead`** (sending to `main` is rejected).
  Long reports are also recoverable from the lane's transcript JSONL (`agentName` field,
  SendMessage `tool_use.input.message`); a `uv` script that does this lives at
  `/private/tmp/claude-501/-Users-zchee-go-src-github-com-zchee-agentctl/2e641301-6689-45d9-ae55-5c3df977766c/scratchpad/extract.py`
  (scratchpad; may be gone after restart, trivial to rewrite).
- Shut each lane down in the turn its report is accepted.
- `/commit` + `git push origin main` after each small task; gpg-signed; message via `-F`
  file; **gate the 72-column subject check with `exit 1` before `git commit`** (two subjects
  slipped through this session and had to be amended + force-pushed).
- No plan markers (W1, P2, lane names) in code, comments or commit messages.
- Replies to the user in Japanese; worker briefs in English; every timestamp from `date`.
- Run from the repo root with absolute paths; never `cd`; check `fd -H -I -t d '^\.omc$'`
  before every commit.

## Next action at resume

1. Ask the user for explicit approval of `docs/plans/agctl-go-port.md` via AskUserQuestion
   (approve via team / approve via ralph / request changes / critic review first).
2. On approval, start P1 (wave W1: `foundation-cli`, `foundation-errs-config`,
   `foundation-testutil`, `foundation-ci` in parallel) through `/oh-my-claudecode:team` with
   the plan path as context. Each lane's brief must include: the Rust reference path and
   commit (`/Users/zchee/rust/src/github.com/zchee/agctl` @ `dbf6aeab84dfbe2465de496316d7bf6780940a60`),
   the plan sections it implements, the gate list (plan section 12), the `/commit` + push
   rule, and the `team-lead` delivery address.
3. P2 spikes (httptrace classification, `~/.claude.json` span splice, `sysctl KERN_PROC`
   field map, memguard lifecycle) gate P3+; a no-go goes back to the user.
4. Update the phase/wave tables in the plan (and the `docs/plans/` copy) at every wave
   boundary, commit, push.
