# Plan: port agctl (Rust) to agentctl (Go)

- Status: **approved 2026-10-05 (user, "approve via team"); P0–P2 done, P3 in progress (W4), W5/W6 partly landed early; section 8 is the truth**
- Written: 2026-10-05; last revised 2026-10-05 15:52:37 JST (`date`; earlier revisions in the
  changelog, section 21)
- Session: https://claude.ai/code/session_019Gqsu3YqQoibgVecjoSu1o
- Mode: `/oh-my-claudecode:plan --direct --deliberate`
- Rust reference (frozen for the port): /Users/zchee/rust/src/github.com/zchee/agctl,
  branch `rc-restart`, commit `dbf6aeab84dfbe2465de496316d7bf6780940a60`
  (98 non-test files, 51,922 physical lines; `docs/research/agctl-inventory-modules.md`)
- Target: /Users/zchee/go/src/github.com/zchee/agentctl (`module github.com/zchee/agentctl`,
  `go 1.27`, toolchain go1.27.1 darwin/arm64; `main.go` holds only the licence header and
  `package main`)
- Inputs: `.omc/handoffs/agctl-go-port-plan-handoff.md`; user answers of 2026-10-05;
  three inventory reports in `docs/research/agctl-inventory-{modules,invariants,oracles}.md`;
  module research in `docs/research/agctl-go-modules.md`.
- Rust paths below (`src/...`, `tests/...`, `fixtures/...`) are relative to the Rust
  reference; Go paths are relative to this module.

---

## 1. Requirements summary

Port the `agctl` CLI (account and credential manager for Claude Code and Codex) to Go
with **macOS full parity** for every `claude` and `codex` subcommand. The on-disk store
stays byte-compatible with the Rust binary (`~/.config/agctl`, same registry format,
same schemas), so the Rust fixtures, schemas and snapshot bodies remain the test oracles
and both binaries can be pointed at the same store during parity sign-off.

Deferred to later phases (section 8): Linux (Rust landed only the Phase 1 process
backend; Phase 2 credential support is recorded NO-GO in
`docs/plans/agctl-linux-support.md:24-62` of the Rust repo) and
`--restart-remote-control` (tmux-driven, `RC_LAST_VERIFIED_VERSION = (2,1,281)` is
marked provisional at `src/provider/claude/remote_control.rs:62-65`).

## 2. Decisions already taken (closed)

| Topic | Decision | Source |
|---|---|---|
| Binary / env prefix | `agentctl`; env vars `AGENTCTL_*` (`AGENTCTL_CONFIG_DIR` replaces `AGCTL_CONFIG_DIR`; same for every production and testing name in section 6) | handoff |
| Store compatibility | `~/.config/agctl` (XDG resolution on macOS too, `src/config/paths.rs:98-116`), same registry format, same `schemas/*.json` | handoff |
| Scope | macOS full parity now; Linux and `--restart-remote-control` later | handoff |
| CLI framework | `github.com/spf13/cobra` only; `completions elvish` dropped and documented; bash/zsh/fish/powershell from the same `cobra.Command` | handoff |
| TUI (`watch`) | `charm.land/bubbletea/v2` + `charm.land/lipgloss/v2` | handoff |
| Table rendering | `charm.land/lipgloss/v2/table`; alignment rules (section 7.4) are custom code on top | user, 2026-10-05 |
| Keychain | `security(1)` child process; reads `show-keychain-info`, `find-generic-password`, `dump-keychain`; writes only `add-generic-password -U` with the line on stdin (`-i`), 4032-byte line bound; **no delete code path**; no cgo; `fixtures/fake-security.sh` reused | handoff |
| TOML (Codex `config.toml`, read-only) | `github.com/zchee/go-toml` (verified to exist, Apache-2.0, by the research lane) | handoff |
| Snapshots | `testdata/*.golden` + `github.com/google/go-cmp` with `-update`; Rust `.snap` bodies reused (with the normalisation rule in section 7.5) | handoff |
| JSON Schema in tests | `github.com/santhosh-tekuri/jsonschema/v6` | handoff |
| JSON | `encoding/json/v2` + `encoding/json/jsontext`; `omitzero`, never `omitempty` | handoff + Go.md |
| HTTP | `net/http` + `net/http/httptrace`; no wrapper library; refresh-POST outcome classification re-derived and table-tested before use (section 9, W2) | handoff |
| Worker model | every worker via `~/.claude/agents/codex-*`; the frontmatter model is authoritative (the Agent tool cannot pass a gateway model id). P0 lanes ran on `claude-gpt-6-astra-ultrafast[1m]`; the frontmatter was changed to `claude-gpt-6-astra-fast[1m]` at 15:30 JST and the user kept that for P1 onwards; `codex-explore` / `codex-writer` stay on `claude-gpt-5.6-luna-fast[1m]` (accepted) | user, 2026-10-05 (twice) |
| Commit trailers for lane commits | `Co-Authored-By: Codex <noreply@openai.com>` + `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` + `Claude-Session`; the astra lanes count as a cross-vendor codex lane | user, 2026-10-05 |
| Security review routing | astra `codex-security-reviewer`, overriding the global opus rule for this project; the lead never runs the review itself | user, 2026-10-05 |
| Commit cadence | `/commit` + `git push` after each small task; planning artefacts mirrored from `.omc/plans/`, `.omc/research/` to tracked `docs/plans/`, `docs/research/` (`.omc/` is globally gitignored) | user, 2026-10-05 |
| Comments | no "ported from agctl" or source-reference comments in Go code; comments state the reason in plain words | user |
| Comments vs implementation in Rust | where a Rust comment and its implementation disagree, the **implementation** is normative for parity (refusal code 10 also covers non-Unreachable acquisition errors, `src/commands/use.rs:2404`; a profile GET happens in swap phase A, `src/provider/claude/swap.rs:302`; automatic stale-lock breaking exists at `src/secret/claude_lock.rs:1779` despite the doctor comment). Intentional corrections are a separate, later change | lead, per analyst recommendation |
| Testing endpoint defaults | the Go testing build fails closed to `http://127.0.0.1:9` for **both** providers when an override is unset (Rust Claude seams fall back to the vendor, `src/provider/claude/oauth.rs:147`; Codex already fails closed, `src/provider/codex/oauth.rs:268`). Test-only behaviour, documented divergence | lead |
| Darwin process observation | `golang.org/x/sys/unix` `SysctlKinfoProcSlice(KERN_PROC)` (no cgo, no extra dependency); identity fields re-derived from `kinfo_proc` and validated in the W2 spike against the Rust fields (`src/runtime/proc/macos.rs:350-463`); `ps` is not acceptable (`:10-16`) | user, 2026-10-05 |
| Secret memory | `github.com/awnumar/memguard` v0.23.0: tokens live in an `Enclave` (encrypted on the heap, not mlocked); a `LockedBuffer` is opened only around each I/O boundary and destroyed immediately; `memguard.Purge()` runs on every exit path including the signal path; mlock limits and the 16 KiB Darwin page size are documented constraints | user, 2026-10-05 |
| Log filter | `AGENTCTL_LOG` accepts `debug\|info\|warn\|error` only; unset or invalid falls back to `warn`; documented as a subset of `RUST_LOG`; the Rust e2e `agctl=trace` becomes `debug` | user, 2026-10-05 |
| Test tooling | TUI: `github.com/charmbracelet/x/exp/teatest/v2` pinned at pseudo-version `v2.0.0-20261004011457-ad85c59fdf4e` (its go.mod names bubbletea v2.0.0-rc.1; compatibility with v2.0.10 verified in W1). E2E: `github.com/rogpeppe/go-internal/testscript` v1.16.0 with txtar scripts; `Setup` points `AGENTCTL_SECURITY_BIN`/`AGENTCTL_CODEX_BIN` at an owned `bin/` dir; flock-holder, SIGTERM and pipe-drain helpers are registered as testscript commands | user, 2026-10-05 |
| Timestamps / zones | `time` with the system zone database (macOS always ships one); `time/tzdata` embedding deferred to P9 if a Linux image lacks zoneinfo | lead |

All judgement calls raised during planning are now closed; section 19 records them.

## 3. Command surface to reproduce

Dispatch map from `src/main.rs:38-63,102-158` and `src/commands/codex/mod.rs:55-62`;
argument definitions in `src/cli.rs` (`claude` 380-669, `codex` 676-842, `completions`
370-375).

```
agentctl [--config-dir DIR] claude status [--json] [--all] [--by-identity] [--timeout S] [--no-cache] ...
agentctl claude watch [--interval S] [--timeout S]
agentctl claude login
agentctl claude accounts {list [--all],show,remove,relocate,forget,unforget} ...
agentctl claude import --from keychain [--dry-run]
agentctl claude doctor [--remove-stale PATH --yes]          # no --json on Claude doctor
agentctl claude use [--live|--undo|--forget] [--yes] [--json] <account>
agentctl claude exec [opts] -- <cmd...>
agentctl claude env [--shell zsh|bash|fish]
agentctl codex status [--json] [--raw] ...
agentctl codex watch [--interval S]
agentctl codex login
agentctl codex accounts {list,show,remove,forget,unforget,set <id> --refresh auto|never,refresh <id> [--resend|--reset-floor] [--yes]}
agentctl codex import --from codex-home
agentctl codex doctor [--json]
agentctl completions {bash,zsh,fish,powershell}
```

Exact flag spellings, arities, defaults and help strings are copied from `src/cli.rs` by
the W1 lane (help output is a golden, modulo binary name). `--config-dir` is accepted
before or after the subcommand (`src/cli.rs:6-11`). Help/version derive from the root
command (`src/cli.rs:340`). The interval/timeout grammar is the Rust one, not
`time.ParseDuration`: bare seconds `300` and `2h` accepted, `10ms`, `2h30m`, fractions
and negatives rejected, outer whitespace trimmed, u64-seconds overflow checked
(`src/cli.rs:286-318`).

### 3.1 Exit codes (`src/error.rs`, `src/cli.rs:69-234`, `src/provider/claude/swap.rs:265-278,447-456`)

| Code | Name | Condition |
|---:|---|---|
| 0 | ok | success; swap `applied` / `already_active`; backend refusal B is a warning only (`src/cli.rs:52,58`) |
| 1 | fatal | `AppError::Config`, `AppError::Io` (`src/error.rs:152-154`); signal-handler install failure (`src/main.rs:47`) |
| 2 | partial | `Keychain`, `Http`, `Auth`, `Refused`, `Partial` (`src/error.rs:155-159`); `status` counts only *shown* failed rows (`src/commands/status.rs:185-219`, `src/commands/codex/status.rs:155-194`); cobra usage errors also exit 2 (separate contract, `src/cli.rs:42-48`) |
| child | `use`, `exec` | forward child exit; signal death maps to `128+signal`; fallback 1 (`src/commands/export.rs:317-322`) |
| 143/129/130 | TERM/HUP/INT | after child teardown and emergency cleanup (`src/runtime/signals.rs:103-133`); main defers to the signal exit (`src/main.rs:50-56`) |
| 10 | REFUSED_A | compromised hold (mtime drift / unreadable held mtime) **and** non-Unreachable acquisition errors (`src/commands/use.rs:2404,2422,2474`) |
| 11 | REFUSED_C | `CLAUDE_CODE_OAUTH_TOKEN` is non-empty (`src/commands/use.rs:795`) |
| 12 | REFUSED_D | encoded keychain line > 4032 bytes incl. newline (`src/secret/keychain_write.rs:74`) |
| 13 | REFUSED_E | live-target undo while `CLAUDE_SECURESTORAGE_CONFIG_DIR` selects the namespace (`src/provider/claude/swap.rs:111`) |
| 14 | REFUSED_F | outgoing credential cannot be adopted safely (`src/provider/claude/adopt.rs:88-174`) |
| 15 | PRECONDITION | namespace not owned by this registry |
| 16 | BUSY | peer locks held, not broken (`src/commands/use.rs:2415`) |
| 17 | DISCARDED | item changed/unreadable under hold, or remaining hold budget < 1.2 s (`src/commands/use.rs:2435-2481`) |
| 18 | UNKNOWN | write timed out and post-release verification is inconclusive (`src/provider/claude/swap.rs:376`) |
| 19 | WRITE_FAILED | definite non-timeout write error (`src/commands/use.rs:2520`) |
| 20 | CANCELLED | confirmation declined or unavailable; `--json` does not imply `--yes` (`src/provider/claude/swap.rs:389`) |
| 21 | NEEDS_REFRESH | expired incoming migrated credential (`src/provider/claude/swap.rs:400`) |
| 22 | AUDIT_REFUSED | durable audit precondition failed (`src/provider/claude/swap.rs:315`) |
| 23 | LIVE_UNREACHABLE | live store absent/dangling (`src/provider/claude/swap.rs:136`) |
| 24 | LIVE_ITEM_ABSENT | live keychain item absent (`src/provider/claude/swap.rs:145`) |
| 27 | LIVE_UNDO_ITEM_CHANGED | undo sees a third account (`src/provider/claude/swap.rs:180,255`) |
| 29 | IDENTITY_UNAVAILABLE | profile unavailable / live token expired without own audit attribution (`src/provider/claude/swap.rs:191,251`) |
| 30 | RC_NOT_DISCONNECTED | remote-control preflight (platform, TTY, disconnect) failed (`src/commands/use.rs:194-213`) |

25, 26, 28 are retired and never reused (`src/cli.rs:55,203-205`). Each refusal carries a
letter *or* a reason, never both (`src/provider/claude/swap.rs:216,244`).

## 4. Architecture of the Go module

Binary at the module root (`main.go`), everything else under `internal/`, mirroring the
Rust tree so parity review is a structural diff. Rust LOC from
`docs/research/agctl-inventory-modules.md` part (a) size the lanes.

| Go package | Rust origin (LOC) | Responsibility |
|---|---|---|
| `main.go` | `src/main.rs` (160) | cobra root, logging init, signal context, deferred signal exit, exit-code translation |
| `internal/cli` | `src/cli.rs` (878), `src/commands/completions.rs` (57) | command tree, flags, duration grammar, swap exit constants, completions (silent `EPIPE`, `src/commands/completions.rs:45-47`) |
| `internal/errs` | `src/error.rs` (175) | `AppError` kinds (Config, Io, Keychain{class}, Http{status,retryAfter}, Auth{invalidGrant}, Refused, Partial{failed}), exit mapping, `agentctl: {err}` on stderr |
| `internal/config` | `src/config/{mod,paths,codex,import}.rs` (1,581) | config dir resolution, versioned registry, modes 0700/0600, `.config.lock`, Claude/Codex/cache/session trees, identifier confinement |
| `internal/secret` | `src/secret/**` (8,770) | `Secret` type over a memguard `Enclave`, reader interface, `security(1)` read transport, stdin-only write transport, file store, secret_file (temp+fsync+rename), pending replay, namespace lock, Claude peer locks (`claude_lock.rs`, 2,046), config lock, foreign activity, held-lock records, append-only audit |
| `internal/provider` | `src/provider/mod.rs` (259) | provider identity, usage/auth interfaces, fetch error classes, user agent |
| `internal/provider/claude` | `src/provider/claude/**` (7,500) | account states, credentials (lossless merge, digests, keychain line), discovery, namespace (NFC + SHA-256), OAuth (PKCE, loopback callback), usage HTTP, swap vocabulary, adopt decisions, `~/.claude.json` rewrite, live sessions (RC, later) |
| `internal/provider/codex` | `src/provider/codex/**` (8,647) | home/config.toml, auth_store (sole `auth.json` I/O), credentials (ordered document), claims, discovery, lock/proof/permit typestate, refresh state machine with durable markers, OAuth refresh POST + outcome classification, usage HTTP, audit |
| `internal/usage` | `src/usage/**` (564) | normalized usage model (floored percentages, missing is not zero), per-account raw cache (300 s TTL, stale fallback) |
| `internal/render` | `src/render/**` (2,554) | status tables (lipgloss/table, psql style), reset countdown/absolute formatting, JSON v1 (Claude status, isolation report) and v2 (Codex status), codex doctor table + JSON, TUI row traits |
| `internal/tui` | `src/tui/**` (711) | Bubble Tea v2 watch model: reducer (`app.rs`), frame (`ui.rs`), terminal entry/restore |
| `internal/commands` | `src/commands/*.rs` (12,415) | status, watch loop, login, import, doctor, accounts, use (4,587), isolate, export (exec/env), prompting/TTY attestation |
| `internal/commands/codex` | `src/commands/codex/**` (4,402) | codex status/watch/login/import/doctor/accounts/accounts_refresh/pass |
| `internal/runtime` | `src/runtime/**` (3,306) | coordinator (bounded workers, cancellation, child ownership), signals, cleanup registry, log buffer (256 KiB), tty readiness, fault seam (tagged), lock-order witness (tagged), proc (macOS backend now; Linux later), tmux (later) |
| `internal/testutil` | `tests/common/**` | temp config/home/bin tree, fake keychain install, env scrubbing, golden/schema helpers, flock holders, bounded polling, pipe draining |
| `testdata/` | `src/render/snapshots`, `src/tui/snapshots`, `src/provider/codex/snapshots`, `schemas/` | 14 snapshot bodies as goldens (section 7.5), 4 schemas verbatim |
| `fixtures/` | `fixtures/**` | 41 files copied byte-for-byte: 3 scripts, 14 Claude, 14 Codex, 10 RC screens |

Cross-cutting Go rules: `context.Context` first parameter on every I/O function;
`encoding/json/v2` everywhere; `log/slog` text handler on stderr with a `ReplaceAttr`
redaction hook and a `Secret` type implementing `slog.LogValuer`; generics only where the
Rust code is already duplicated across providers (registry CRUD, forget/unforget, watch
loop over `TuiRow`); function signatures on one line; `any`, `min`/`max`, `for range n`,
`strings.Cut*`, `slices.Clone`, `b.Loop()`.

### 4.1 Module set (versions verified 2026-10-05, `docs/research/agctl-go-modules.md`)

| Module | Version | Use |
|---|---|---|
| `github.com/spf13/cobra` | v1.10.2 | CLI, completions (`GenBashCompletionV2`, `GenZshCompletion`, `GenFishCompletion`, `GenPowerShellCompletionWithDesc`) |
| `charm.land/lipgloss/v2`, `.../table` | v2.0.6 | tables (`table.New().StyleFunc().Headers().Rows()`), `lipgloss.Width` for cell width |
| `github.com/charmbracelet/colorprofile` | v0.4.3 | explicit `colorprofile.NewWriter(w, environ)` with `Profile = colorprofile.NoTTY` for the plain-string tables (Lip Gloss v2 has no `NewRenderer`) |
| `charm.land/bubbletea/v2` | v2.0.10 | `watch`; `View() tea.View` with `View.AltScreen = true`; `tea.Tick` re-armed on every tick |
| `github.com/charmbracelet/x/exp/teatest/v2` | v2.0.0-20261004011457-ad85c59fdf4e | TUI tests (`NewTestModel`, `Send`, `Type`, `FinalOutput`) |
| `github.com/zchee/go-toml` | v0.0.0-20260722214334-70b8c27cb946 | read-only `Unmarshal` of Codex `config.toml` (`toml:"name,omitzero"` tags); `WithCopiedStrings` to avoid retaining the arena |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | tests: `UnmarshalJSON` → `AddResource` → `Compile` → `Validate`; `DefaultDraft(Draft2020)` |
| `encoding/json/v2`, `encoding/json/jsontext` | Go 1.27 stdlib | all JSON; `jsontext.Decoder` for token offsets in the `~/.claude.json` splice |
| `golang.org/x/sys/unix` | v0.48.0 | `Flock`, `Fstat`, `Openat`/`Unlinkat`, `SysctlKinfoProcSlice` |
| `golang.org/x/term` | v0.46.0 | `IsTerminal`, `GetSize`, `MakeRaw`/`Restore` |
| `golang.org/x/text/unicode/norm` | v0.42.0 | NFC before namespace hashing |
| `github.com/awnumar/memguard` | v0.23.0 | `Enclave`/`LockedBuffer` behind the `Secret` type |
| `github.com/google/go-cmp` | v0.7.0 | test assertions and golden diffs |
| `github.com/rogpeppe/go-internal/testscript` | v1.16.0 | e2e txtar scripts |
| `log/slog`, `net/http`, `net/http/httptrace`, `crypto/*`, `os/exec`, `os/signal` | stdlib | logging, HTTP, PKCE/SHA-256/random, children, signals |

Not used: `gofrs/flock` (x/sys suffices), `go-runewidth`/`x/text/width` (lipgloss width
first; revisit only if the width corpus in W3 fails), `x/exp/slog`, `x/net/http2` (no typed
HTTP/2 classification; unknown stream errors stay "outcome unknown").

Remaining crate mappings with a stdlib answer (`docs/research/agctl-inventory-modules.md`
part (b)): `open` → `exec.CommandContext(ctx, "open", url)` detached on macOS
(`src/commands/login.rs:170`); `base64`/`hex`/`sha2`/`rand`/`url` → `encoding/base64`
(raw URL), `encoding/hex`, `crypto/sha256`, `crypto/rand` (PKCE, temp names) and
`math/rand/v2` (jitter only), `net/url`; `etcetera` → explicit XDG lookup in
`internal/config`; `jiff` → `time`; `signal-hook` → `os/signal.NotifyContext`;
`thiserror`/`anyhow` → typed errors + `errors.Is/As`; `tracing` → `log/slog`. Test-only:
`flate2` → `compress/gzip`; `rcgen` → `httptest.NewTLSServer`; `httpmock` →
`httptest.Server` on real sockets; `rustix pty` → `github.com/creack/pty` (version
pinned by the W1 lane from the module proxy) for the tty-readiness tests
(`src/runtime/tty_tests.rs:14-25`); `assert_cmd`/`predicates`/`tempfile` →
`testscript`, `cmp`, `t.TempDir()`.

## 5. RALPLAN-DR summary

### Principles

1. **Store compatibility is a contract**: every byte written under `~/.config/agctl`, to
   `~/.claude.json` or to a Codex `auth.json` must be indistinguishable from the Rust
   binary's output.
2. **Secrets never leave the process unredacted**: no token in logs, errors, panics,
   `--json` or goldens; the keychain client reads and upserts only; keychain writes carry
   the secret on stdin, never argv.
3. **Test seams cannot ship**: every test-only override lives behind the `agentctl_testing`
   build tag; a release gate proves absence by binary scan with positive controls.
4. **Oracles over opinions**: Rust fixtures, schemas and snapshot bodies settle parity
   disputes; the Rust implementation (not its comments) is normative.
5. **Risky mechanisms are spiked first**: refresh-POST outcome classification and the
   byte-exact `~/.claude.json` rewrite are proven with table tests before any command uses
   them.
6. **Deterministic lifecycle instead of destructors**: Rust `Drop`-based witnesses
   (unaudited-receipt panic, lock-order thread-local) become explicit ownership tokens
   checked at well-defined points, because Go has no deterministic destructor and
   goroutines migrate between threads.

### Decision drivers

1. Zero data-loss risk on a user's live credential store (`use --live`, keychain upsert,
   `~/.claude.json`, Codex `auth.json`).
2. Parity verifiable mechanically (goldens, schemas, exit codes, argv logs) so review is
   cheap and worker lanes can run in parallel.
3. Small, independently committable waves, each gated by tests, executed by astra lanes.

### Options

**A. Faithful module-by-module port (chosen).** Mirror the Rust tree and port file by
file, reusing oracles. Pros: parity review is structural; invariants map one-to-one; lanes
split by package. Cons: some Rust-shaped abstractions survive (proof/permit typestates
become interface + unexported constructors); larger initial LOC.

**B. Provider-generic redesign.** One generic registry/usage/doctor engine with thin
`claude`/`codex` adapters. Pros: less code long term. Cons: parity is harder to prove
(oracles map to behaviour, not structure); the providers diverge materially (keychain vs
`auth.json`, peer-lock swap vs durable-marker refresh); redesign risk lands on the
credential path. Rejected for now; revisit after P7 with goldens as a safety net.

**C. Go wrapper that shells out to the Rust binary.** Rejected: not a port; adds a Rust
build dependency; gains nothing for parity.

## 6. Environment variables

Production (`docs/research/agctl-inventory-modules.md` part (c)); every `AGCTL_` name is
renamed `AGENTCTL_` in the port:

| Variable | Meaning | Rust site |
|---|---|---|
| `AGENTCTL_CONFIG_DIR` | store root override (CLI `--config-dir` wins) | `src/cli.rs:343`, `src/config/paths.rs:80,108` |
| `AGENTCTL_LOG` (was `RUST_LOG`) | log filter; unset/invalid falls back to `warn`; stderr only | `src/main.rs:66-90`; levels only (section 19) |
| `AGENTCTL_CLAUDE_USER_AGENT`, `AGENTCTL_CODEX_USER_AGENT` | HTTP user agent | `src/provider/claude/mod.rs:40`, `src/provider/codex/mod.rs:63` |
| `AGENTCTL_CLAUDE_OAUTH_SCOPES` | replaces login scopes | `src/provider/claude/oauth.rs:135,387-394` |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR`, `CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_OAUTH_TOKEN`, `CODEX_HOME`, `HOME`, `USER`/`LOGNAME`, `PATH` | vendor variables read as in Rust | `src/provider/claude/namespace.rs:65-72,109-114`; `src/commands/codex/mod.rs:73-77`; `src/secret/mod.rs:383` |
| `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN`, `CODEX_REFRESH_TOKEN_URL_OVERRIDE`, `CODEX_APP_SERVER_LOGIN_CLIENT_ID` | presence-only doctor diagnostics | `src/commands/codex/doctor.rs:113-117,180` |

Child environment allowlists are compatibility surfaces: `codex login` passes exactly
`HOME, PATH, TMPDIR, LANG, TERM, HTTP_PROXY, HTTPS_PROXY, NO_PROXY, ALL_PROXY` + lowercase
forms, `SSL_CERT_FILE, SSL_CERT_DIR`, non-empty `LC_*`, and an owned `CODEX_HOME`
(`src/provider/codex/login_child.rs:120-139,209-236`). `claude exec`/`env` strip
`CLAUDE_CODE_OAUTH_TOKEN` (`src/commands/export.rs:44`).

Testing-only (compiled only under the `agentctl_testing` tag; full seam table in
`docs/research/agctl-inventory-invariants.md` part (d)): `AGENTCTL_CLAUDE_{TOKEN,AUTHORIZE,PROFILE,USAGE}_URL`,
`AGENTCTL_CODEX_{TOKEN,USAGE}_URL`, `AGENTCTL_KEYCHAIN_BACKEND`, `AGENTCTL_SECURITY_BIN`,
`AGENTCTL_CODEX_BIN`, `AGENTCTL_NO_BROWSER`, `AGENTCTL_FAULT`, `AGENTCTL_FAULT_RESUME`,
`AGENTCTL_SWAP_DEADLINE_MS`, `AGENTCTL_RC_BUDGET_MS`, `AGENTCTL_TMUX_BIN`,
`AGENTCTL_FAKE_CODEX_*`, `AGENTCTL_FAKE_TMUX_*`. Fixture-script knobs
(`AGCTL_FAKE_SECURITY_*`) keep their Rust names because the scripts are copied verbatim;
the Go harness sets them.

## 7. Invariants, budgets and rendering contracts

### 7.1 Safety invariants (full table with test anchors: `docs/research/agctl-inventory-invariants.md` (c))

Each becomes a named Go test; the lane that ports the package owns the test.

| Invariant | Rule | Enforced at |
|---|---|---|
| Rooted layout | `--config-dir` > `AGENTCTL_CONFIG_DIR` > XDG config + `agctl`; Claude-only operations never create `codex/` | `src/config/paths.rs:137,177,210,276,312,410` |
| Private modes | new dirs 0700, new credential/lock files 0600; existing modes not repaired; live config replacement preserves target mode | `src/config/paths.rs:74,77,427`; `src/provider/claude/claude_json.rs:1041` |
| Identifier confinement | invalid path segments rejected; Codex rejects leading dots; lock names `user+acct` | `src/config/paths.rs:347,477,495,522` |
| Descriptor-relative mutation | lexical containment **and** dirfd-relative, no-follow traversal (`openat`/`unlinkat`); no fresh path resolution for leaf ops | `src/secret/file_store.rs:513,886`; `src/secret/secret_file.rs:196,212` |
| Lock inode persistence | `.config.lock`, `.locks/<acct>.<org>.lock`, `.locks/<user>+<acct>.lock` are never unlinked | `src/secret/namespace_lock.rs:88`; `src/commands/doctor.rs:1013` |
| Flock fails closed | only `EWOULDBLOCK` retries; `ENOTSUP` never authorises a write | `src/secret/namespace_lock.rs:250,263` |
| Atomic credential write | exclusive 0600 temp, fsync, rename; never truncate; pending metadata before pending bytes | `src/secret/secret_file.rs:189,218,328`; `src/secret/pending.rs:15` |
| No plaintext fallback | keychain read errors never fall through to credential files | `src/secret/location.rs:12`; `src/secret/mod.rs:195` |
| Keychain transport | reads: preflight/dump/find only, `/usr/bin/security` in production; one write transport, argv `-i`, payload on stdin; no delete transport can be constructed | `src/secret/security_cli.rs:59-69`; `src/secret/keychain_write.rs:1,495` |
| 4032-byte line | line incl. newline ≤ 4032; no argv fallback | `src/secret/keychain_write.rs:74`; `src/provider/claude/credentials.rs:271` |
| Redaction | one private exposure helper per provider; PKCE verifier redacted; `sk-ant` runs replaced before 512-byte truncation; config errors never quote values | `src/provider/claude/credentials.rs:427,508,524`; `src/provider/claude/oauth.rs:129,1037`; `src/provider/claude/claude_json.rs:46,283` |
| Secret containment | Rust wipes PKCE and Codex refresh buffers; keychain stdout is a plain buffer. Go goes further: every token lives in a memguard `Enclave`, plaintext exists only inside a `LockedBuffer` opened around one I/O call and destroyed after it; `Purge()` on every exit path. Copies made by `net/http`, `json/v2` and the GC are outside the guarantee and documented as such | `src/provider/codex/oauth.rs:305,336`; `src/secret/security_cli.rs:170` |
| Peer-lock protocol | `.oauth_refresh.lock` → `<store>.lock` → `.storage-write.lock` mkdir order, reverse release, EEXIST restarts (max 3) | `src/secret/claude_lock.rs:1185,1284,1503,1547` |
| Stale peer-lock proof | age/profile eligibility, no live holder evidence, unchanged mtime across 12 s, wall/monotonic skew ≤ 1 s, third sample before rmdir, one break per acquisition (three samples; doctor uses two) | `src/secret/claude_lock.rs:1690-1805` |
| Hold budget | every held mtime unchanged; write starts only if elapsed + 1.2 s ≤ 3 s | `src/secret/claude_lock.rs:1088`; `src/commands/use.rs:2422-2481` |
| Swap phase separation | no HTTP (except phase-A profile GET), prompt or sampling while peer locks are held; refusal D before spawn | `src/provider/claude/swap.rs:6,287,302,352` |
| Live config byte preservation | rewrite only when re-serialising the unmodified document is byte-identical; replace `oauthAccount`, delete exactly five caches; backup first; recheck digest and lock before rename; separate `.claude.json.lock` never broken | `src/provider/claude/claude_json.rs:98,277,292,820,886-964`; `src/secret/config_lock.rs:1` |
| Append-only audit | 0600 regular no-follow file, append + fsync, 8-hex digest prefixes, never tokens; wrong modes refused | `src/secret/audit.rs:599,613,646,1037,1149` |
| Codex typestate | live homes read-only; only `auth_store` opens `auth.json`; verified login / locked refresh / pending replay are the only writers; receipts must reach audit | `src/provider/codex/auth_store.rs:157,249,613,736`; `src/provider/codex/proof.rs:90,106` |
| Codex durable send marker | inflight marker persisted before POST; unknown outcome blocks automatic resend; resend needs TTY + fresh consent, once per marker after floor | `src/provider/codex/refresh.rs:391,402,703,730,1044` |
| Lock ordering | never take `.config.lock` while owning a Codex namespace guard; witness via explicit per-operation ownership context (not thread-local) | `src/runtime/lock_order.rs:36-92`; `src/config/mod.rs:303` |
| Signals | TERM/HUP/INT cancel, tear down children, run emergency cleanup, exit 143/129/130; main never races the signal exit | `src/runtime/signals.rs:103-133`; `src/main.rs:50-56` |

### 7.2 Timing budgets (named constants, one test per row; full list in the invariants report)

| Constant | Value | Rust site |
|---|---|---|
| `WatchFloor` / `WatchDefault` | 60 s / 300 s | `src/cli.rs:33,446,751` |
| `HTTPTimeoutDefault` | 10 s | `src/cli.rs:438,743` |
| `NamespaceLockRetry` / `NamespaceLockWait` / create attempts | 250 ms / 5 s / 8 | `src/secret/namespace_lock.rs:59,63,294` |
| `RegistryLockWait` | 5 s | `src/config/mod.rs:83` |
| Peer lock: heartbeat / contention rounds / stale sample / skew / hold / config hold / restarts | 5 s / 5×(1 s+1 s jitter) / 12 s / 1 s / 3 s / 1.2 s / 3 | `src/secret/claude_lock.rs:134-195` |
| Peer stale profiles | refresh/legacy 60 s, storage-write 15 s, config 10 s | `src/secret/claude_lock.rs:217-238` |
| Config-lock ladder | 200/400/800 ms + jitter | `src/secret/config_lock.rs:72,426` |
| Config rewrite cumulative gates | 50/200/400/650/950/1100/1200 ms | `src/provider/claude/claude_json.rs:757-777` |
| Security CLI | read/preflight 2 s, dump 10 s, write 1.2 s, verify read 800 ms (distinct constants) | `src/secret/security_cli.rs:48,51`; `src/secret/keychain_write.rs:90,97` |
| Doctor | min age 60 s, sample 12 s | `src/commands/doctor.rs:108,115` |
| Import deadline / swap overall | 60 s / 120 s | `src/commands/import.rs:47`; `src/commands/use.rs:174` |
| Watch loop | request 10 s, margin 5 s, UI poll 250 ms, quit drain 250 ms | `src/commands/watch.rs:101-126` |
| Coordinator | workers 4, join 500 ms, child poll 10 ms, watchdog 25 ms | `src/runtime/coordinator.rs:68-79` |
| Signals | child poll 5 ms, kill settle 50 ms, exit deferral 10 s | `src/runtime/signals.rs:78-93` |
| Claude OAuth | token 30 s, profile 10 s, loopback 600 s, poll 250 ms, retry floor 5 s, callback read 10 s, refresh lead 300 s | `src/provider/claude/oauth.rs:108-126,1155`; `src/provider/claude/credentials.rs:69` |
| Usage cache TTL / connect | 300 s / 5 s | `src/usage/cache.rs:48`; `src/provider/claude/usage.rs:102` |
| Codex refresh POST phases | 2/3/2/2/8/2 s (sum 19 s), write 1 s, status lock 1 s | `src/provider/codex/oauth.rs:96-115`; `src/provider/codex/refresh.rs:98-104` |
| Codex refresh policy | resend wait 1 h, max Retry-After 24 h, 401 floor 60→120→240 min, terminal count 3, access lead 5 min, last-refresh 8 d | `src/provider/codex/refresh.rs:108-126`; `src/provider/codex/auth_store.rs:123,1623`; `src/provider/codex/credentials.rs:69,73` |
| Codex misc | login child 600 s, scratch locks 5 s, stale scratch 15 min, doctor listing 15 s, PID recycle 1 s | `src/provider/codex/login_child.rs:77`; `src/commands/codex/login.rs:70-81`; `src/commands/codex/doctor.rs:125`; `src/provider/codex/home.rs:83` |
| Log buffer while TUI owns terminal | 256 KiB, keep head, count dropped tail | `src/runtime/log_writer.rs:53` |

### 7.3 HTTP endpoints (`docs/research/agctl-inventory-invariants.md`)

Claude authorize `https://claude.com/cai/oauth/authorize` (consumer) /
`https://platform.claude.com/oauth/authorize` (console); token
`https://platform.claude.com/v1/oauth/token`; profile
`https://api.anthropic.com/api/oauth/profile`; usage `https://api.anthropic.com/api/oauth/usage`;
Codex refresh `https://auth.openai.com/oauth/token`; Codex usage
`https://chatgpt.com/backend-api/wham/usage`. Loopback callback binds 127.0.0.1.

### 7.4 Rendering contracts (`docs/research/agctl-inventory-oracles.md` (g))

- Claude status columns: Account, Org, Plan, 5h, Weekly, Fable (weekly), Credits, 5h reset,
  Weekly reset, State; `--by-identity` inserts Kind after Plan (`src/render/table.rs:74-148`).
  Codex status: Account, Plan, Kind, 5h, Weekly, Credits, 5h reset, Weekly reset, State
  (`:101-110`). Claude `accounts list`: Id, Account, Org, Kind, Source, State, Location
  (`src/commands/accounts.rs:75-79`). **Codex `accounts list` is not a table**: `id  kind  email`
  lines, optional `  (forgotten)`, `-` for missing email (`src/commands/codex/accounts.rs:163-185`).
- Style: psql (header rule + `|` separators, no outer box), left alignment, one-space cell
  padding, no trimming, right padding preserved, trailing LF (`src/render/table.rs:266-275,337-346`).
  No terminal-width reflow. Hidden rows → footer `N entry/entries hidden (--all)`.
- Reset columns: per column, countdown flush-left and `(absolute)` flush-right to the
  column's max natural width; em dash/blank never justified (`src/render/table.rs:287-330,446-509`).
  Absolute: same day `4:15 PM`; < 7 days `Sun 02:00 PM`; otherwise `Sep 16 02:00 PM`;
  past countdown prints `now` (`src/render/reset.rs:53-69,117-120,164-197`).
- Continuation rows: `  ↳ <window>` in the first cell, percentage in Weekly, reset in Weekly
  reset, 5h reset blank, identity cells blank (`src/render/table.rs:237-250,386-415`).
- Missing values are em dash; percentages floored; credits cell states
  (`src/render/table.rs:410-444`, `src/usage/model.rs:5-15`).
- **No `NO_COLOR` handling and no `--no-color` flag exist**; tables are plain strings. Do
  not add colour switches. Width is Unicode display width (papergrid/ratatui use
  `UnicodeWidthStr`), so Go must use a display-width function, never `len` or rune count.
- Watch: provider-neutral blocks (header: shown-row count, last fetch age, fetching/next
  countdown, stale marker; per-account gauges with 10-cell labels; footer
  `q quit · r refresh · ↑↓ select`); keys q/Esc/Ctrl-C/Ctrl-D, r, Up/k, Down/j, no wrap, no
  mouse; first pass immediate, one in-flight pass, `r` forces no-cache and coalesces; pass
  deadline interval−5 s; raw mode + alt screen restored on return/panic/signal
  (`src/commands/watch.rs:97-147,262-265,299-417,466-477`; `src/tui/ui.rs:47-178`).

### 7.5 Oracle rules (`docs/research/agctl-inventory-oracles.md` (e))

- Schemas (draft 2020-12, copied verbatim): `status.v1.json` ← `claude status --json`;
  `status.v2.json` ← `codex status --json`; `codex-doctor.v1.json` ← `codex doctor --json`;
  `doctor.v1.json` validates the **internal** Claude isolation report only (no
  `claude doctor --json` flag exists). Schema validation proves shape, not byte order.
- 14 `.snap` files. Strip the header: drop the first `---` line through the next `---` line
  (header length varies). **Insta trims trailing whitespace at end of file and normalises
  CRLF**, so seven table snapshots lack the final row's right padding that stdout emits.
  Rule: table goldens are compared after the same end-of-file trim (comparator helper),
  **and** one exact-byte stdout test per table asserts the untrimmed padding and trailing LF.
  Interior trailing padding stays. TUI snapshots (3 + 1 Codex) are quoted `TestBackend`
  frame dumps: compared through a frame-dump adapter on the Bubble Tea `View()` output,
  never against ANSI streams. `f78_capture_json_v2.snap` is a `{windows, credits}` fragment.
- Fixtures: all 41 reusable as data; the three scripts need POSIX shell + `xxd`/Perl
  (`fixtures/fake-security.sh:140-147`), Perl for the fake-codex held-lock branch, and
  Perl/JSON::PP + `shasum` for fake-tmux (later phase). Injection is by
  `AGENTCTL_SECURITY_BIN` / `AGENTCTL_CODEX_BIN` into an owned `bin/` directory, not PATH
  replacement (`tests/common/mod.rs:529-553`, `tests/common/codex.rs:83-95`).
  `fixtures/claude/security-find.txt` has no current consumer; keep it, do not cite it.
- Rust e2e suite: 24 files, ~409 `#[test]` declarations (e2e_swap alone 129); table in the
  oracles report. `tests/cli_smoke.rs:65-255` is the help/version/completions/broken-pipe
  contract.

## 8. Phases and waves

### Phase table

| Phase | Description | Status | Landed |
|---|---|---|---|
| P0 | Planning: this document; research mirrored to `docs/research/` | ✅ done | `34585b7`, `0957188`, `3a35bd7`, `60b5e9a`; handoff `b6df779` |
| P1 | Foundation: module skeleton, `internal/cli` tree + duration grammar + completions, `internal/errs`, `internal/config` (paths, registry, locks), `internal/testutil`, fixtures/schemas/goldens copied, CI | ✅ done 2026-10-05 (W1 closed by `verify-w1-close` at `5aa6701`: 923 / 926 tests green on both tags, all eleven review and verification findings fixed) | foundation `1992cca`…`a9aa959`, fixes `a1c875b`…`add32a7` (wave table) |
| P2 | Spikes with go/no-go gates: refresh-POST outcome classification on `net/http`+`httptrace`; byte-exact `~/.claude.json` edit by span splicing; Darwin process observation via `sysctl KERN_PROC`; memguard lifecycle under the signal handler and mlock limits | ✅ done 2026-10-05 (four GO verdicts; memguard conditional on the startup budget check) | darwin-proc `2645bec` `92fc1bb` `92c31af`; jsontext `c01e1f2` `0807f39` `caeefc9`; memguard `f8a9432`…`451898b`; httptrace `3569e0e` `d165427` `5aa6250`; verdicts in `docs/research/agctl-spike-*.md` |
| P3 | Claude read path: `security(1)` reader, credentials, namespace, discovery, usage + cache, render tables/reset/JSON v1, `status`, `accounts list/show` | ✅ done 2026-10-06 01:13 JST (W3 + W4, closed with the W4/W5 gate) | status `6684d14` `2d3fc1e`, token refresh `6a12fba` `f04ec63` `3ba4f21`, fix round `9108486` `9ea9c2d` |
| P4 | Claude `watch` (Bubble Tea v2), `login` (PKCE + loopback), `import --from keychain`, `accounts remove/relocate/forget/unforget`, file store + pending replay | ✅ done 2026-10-06 01:13 JST (W5) | watch `e8f3fb6`, login `3b9b525`, import/accounts write `7045063`, file store `eaf44b8`, fix round `7b8201b` `5d3cc81` `6e3fac3` `cfb218c` `8a4d99a` |
| P5 | Claude `use` (isolate), `exec`, `env`, `doctor` (incl. `--remove-stale`), then `use --live` / `--undo` / `--forget` (peer locks, swap, adopt, audit, `~/.claude.json`) | ✅ done 2026-10-06 08:02 JST (W6 closed 01:56 JST; W7 closed after the gate on `59da7da`/`52bcd50` and re-verification on `be43c6c`) | W6 `6952073` `ac7b86f`; W7 `8662f57` … `52bcd50`, fixes `9597086` `a5cd742` `be43c6c` |
| P6 | Codex: home/config.toml, auth_store, credentials, claims, `status/watch/accounts/import/doctor`, login child, refresh state machine + resend policy | ✅ done 2026-10-06 10:27 JST (W8: four lanes + one fix lane, 23 implementation commits, gate fixed in five commits, re-verified on `08dea5f`) | `fd9d4f3` `fd518ae` `61bdb7a` … `f784164`, fixes `fbf4a1e` … `08dea5f` |
| P7 | Release gates: seam-absence proof, completions for four shells, README, parity sign-off against the Rust binary on one store | ✅ done 2026-10-06 11:18 JST (W9; automated parity complete, the 22 keychain/endpoint steps await a manual run) | `64894e9` `dd9dc40` `b059a4d` `b371ce9` `b650179`, fixes `289df4e` `e71769f` `0f1dbb8` `934c6b4` `e43ea9c` `308371e` `c203eef` |
| P8 (later) | `--restart-remote-control`. **Design decided 2026-10-09 21:19:08 JST on the isolated spike of 2026-10-07 (report `.omc/handoffs/w12/rcspike.md`, artifacts `.omc/artifacts/rc-spike/`, both untracked):** the bridge is not disconnected before the swap; Claude Code (2.1.292 measured) drops it itself about 2 s after the credential change with "signed-in claude.ai account or organization changed on this machine" while model requests keep succeeding under the new account, so the reference's tmux Up/Up/Enter disconnect step is unnecessary; reconnection runs through a Claude Code mod, whose `$.command.run({command: 'remote-control'})` starts a new bridge generation in the same process, session id and transcript (inode unchanged); the agentctl-to-mod transport is file polling (request/response files under the session's `CLAUDE_CONFIG_DIR`); tmux `send-keys` stays only as the fallback for sessions that do not load the mod; version pin: mods need Claude Code 2.1.287 or newer (replaces `RC_LAST_VERIFIED_VERSION`). RC attestation and the screen fixtures follow the mod design. Unverified: whether claude.ai/code shows the pre-swap conversation under the new account (the user checks it). | 🔶 implemented 2026-10-11 (mod `plugins/remote-control/`, marketplace, `--restart-remote-control` over request files; nine commits `af1094a` `836ee7f` `deaedc6` `cfa2602` `67fa790` `c148690` `05198bd` `99c9c2b` `8de1d8c`; round-1 gates verify FAIL / review REQUEST CHANGES / security APPROVE WITH FINDINGS, every finding fixed in the last two commits; round-2 gates running; the operator run on 2.1.296 and the claude.ai history check stay open) | plan `.omc/plans/remote-control-mod.md`, contract `docs/research/remote-control-mod.md` |
| P9 (later) | Linux: process backend (procfs), platform refusals (keychain unsupported, `--remove-stale` exit 1), CI on `ubuntu-26.04`; Phase 2 credentials stay NO-GO until re-decided | 🔜 not scheduled | — |

Current point: **P7 closed 2026-10-06 11:18:40 JST on `82de94e` (old `308371e`; main was rewritten on 2026-10-06 ~13:35 JST to remove `.claude/omc.jsonc` from history and re-sign every commit, so every hash in this document written before that time is pre-rewrite and resolves by subject). W10 (parity VM, P7 follow-up) scripts stage closed 2026-10-06 15:42:51 JST on `c22dca6`: the user ruled that the release parity sign-off requires the manual keychain run of `docs/research/agctl-parity.md`, executed in a tart macOS VM (`macos-golden-gate-xcode`) with the guest VMs kept for other end-to-end tests, ONE disposable Claude account (forward/undo RECORDED as already-active) and refresh coverage deferred. `scripts/parity-vm.sh`, `scripts/parity-vm-guest.sh`, `scripts/lib/parity-common.sh` landed as `f46e531`; review findings (ownership marker before clone, scalar type change accepted as RECORDED, snapshot with an absent base, then work-directory-only deletion authority and stale-lock recovery) fixed in `ac27460` and `c22dca6`, each pin re-gated; dry-base measurements: tart tolerates the sidecar file, `tart clone` does not copy it, `tart delete` removes it. Next: the operator run (prepare -> GUI Claude Code login -> snapshot -> run reference / run go -> compare), then the "Manual run" section and Verdict of `docs/research/agctl-parity.md` (verify-w10e). P8 (remote control) and P9 (Linux; go-toml's amd64 SIMD names first) stay deferred; hygiene follow-ups ran as W11 on 2026-10-07: go-toml archsimd pin (`6ce537d`), release env isolation (`31c0732`), chflags (`a150440`), CI shellcheck + release gate (`5707842`, hosted macOS job green; the linux job's Test step has been failing since before W11 and is P9 input), fault-point names as tagged constants with a strict release gate (`7970620`, `0387843`, `0bc5885`); the fault source-contract test landed as `a3be959` with the release-gate comment `fc16949` on 2026-10-07 11:33:25 JST, closing W11. Open after W11: the W10 operator keychain parity run (new WORK dir), the CI linux job's Test step (red since before W11, P9 input), hosted macOS Test flakiness in `internal/provider/codex` (run 37521478936 on the plan-only `8613a14`), and the follow-up to move the fault source-contract resolver onto `go/types` so receiver shapes are resolved by construction. W12 closed 2026-10-09 21:18:34 JST on `280504e`: the fault source-contract resolver now resolves callers by Go type identity (`golang.org/x/tools/go/packages`, per-variant `types.Info`, physical positions, cwd-only fail-closed module root); four fresh verify/review rounds (three fix lanes), 199 table cases per mode; gates green on go1.27.1 (release-gate pin) and go1.27.2 (hosted pairing with golangci-lint v2.14.0). Open after W12: the remote-control design record (P8 row), the W10 operator keychain parity run, the CI linux job (P9 input), the hosted macOS codex test flakiness, and the by-name `GOTOOLCHAIN=go1.27.1` pins in `internal/commands/export_process_test.go` and `scripts/release-gate.sh` (a local checkout without that SDK on PATH fails the exec teardown test; hosted CI downloads it).**

### Wave table (P1–P7)

| Wave | Lanes (parallel) | Depends on | Exit gate |
|---|---|---|---|
| W1 | `foundation-cli` (cobra tree, flags, duration grammar, exit constants, completions), `foundation-errs-config` (errs, paths, registry, modes, `.config.lock`, namespace lock), `foundation-testutil` (fixtures/schemas/goldens copy + header-strip script, temp tree, fake-bin install, golden/schema helpers), `foundation-ci` (`.github/workflows/ci.yaml`) | — | section 12 gates green; `agentctl --help` and `completions` goldens match `tests/cli_smoke.rs` modulo binary name. **Lanes done 2026-10-05** (cli `1992cca` `c1e0c04` `bd8b9de` `cd81e64` `06fe00f` `e870694`; errs-config `670e226` `0e67fe1` `e75b7ff` `f88a166`; testutil `727ca91` `d2ba9ee` `19dcadc` `4b2393d` `53b62a5` `a9275e5`, helpers rode in `cd81e64`; ci `6aa7acb` `a9aa959`). Verification at `92c31af`: section 12 green on both tags (527 / 530 tests), oracles and `cli_smoke` surface pass, one gap (`RegistryLockWait` has no test). Review: REQUEST_CHANGES, one MAJOR (namespace lock writes no holder body), six MINOR, four NIT. `fix-w1` closed all of them in `a1c875b` `8b3556d` `6cb7bdf` `7c9b97d` `c664191` `26b14e3` `099708e` `5b6a747` `a97fc71` `add32a7` (flock core extracted to the new leaf package `internal/lockfile` because `secret` imports `config`; registry bytes proven against two documents written by the Rust binary; help text 91 of 101 blocks byte-identical, the rest have no cobra rendering slot; `expectexit` testscript helper; `main.go` runs `secret.EnsureLockedMemoryBudget()` first and `secret.Purge()` once on every exit path). **W1 CLOSED** by `verify-w1-close` at `5aa6701` (snapshot gates green both tags, 923 / 926 tests, every finding verified by test name or byte comparison) |
| W2 | `spike-httptrace`, `spike-jsontext`, `spike-darwin-proc`, `spike-memguard` | W1 for the gate; started in parallel with W1 on 2026-10-05 by the user's decision (eight lanes at once) | table tests green; one verdict file per spike, `docs/research/agctl-spike-{httptrace,jsontext,darwin-proc,memguard}.md`; no-go stops the dependent waves and goes back to the user. **darwin-proc: GO** (`2645bec` `92fc1bb` `92c31af`; every libproc field maps onto `unix.KinfoProc`; `P_comm` 16-byte limit documented; sysctl reads other users' records, so no per-process refused/signalable sweep). **jsontext: GO** (`c01e1f2` `0807f39` `caeefc9`; no stdlib formatter reproduces the reference pretty-printer, so a ~90-line canonical printer over `jsontext.Decoder` tokens does, ground-truthed against serde_json 1.0.151 `preserve_order`+`float_roundtrip`; API `Reproduce` / `Plan` / `Rewrite.Apply` with `ErrChanged` as the under-lock recheck; refusals are bare sentinels so no document bytes leak). **memguard: GO, conditional on the startup budget check** (`f8a9432` `233d002` `27d3563` `3c33dc9` `0076689` `6bd66a7` `17c0ebf` `451898b`; `secret.EnsureLockedMemoryBudget()` refuses a soft `RLIMIT_MEMLOCK` below 64 pages, 1 MiB on Darwin; memguard v0.23.0 deadlocks in purge after a failed allocation, measured at the fifth open under 256 KiB). **httptrace: GO** (`3569e0e` `d165427` `5aa6250`, committed by the lead because the lane would not commit on `main`; every reference outcome class reproduces on real sockets; TLS failures stay unknown as in the reference; HTTP/1.1 only; unknown never resends). **W2 complete, all four GO.** |
| W3 | `claude-secret-read` (security_cli, location, reader), `claude-credentials-namespace` (provider identity, credentials, namespace NFC+SHA-256, account states, claims), `claude-usage` (usage model + cache + Claude usage HTTP), `render-table` (table, reset, row) | W1; started 2026-10-05 while `fix-w1` was still closing the W1 review items (user's decision, seven lanes); the full W1 close bounds W4 | unit + golden tests; `fake-security.sh` argv log equals Rust e2e expectations. **Lanes done 2026-10-05**: secret-read `2bf45f3` `b80259f` `f5bda70` `c3f28f9`; credentials-namespace `507531c` `d7f8b7c` `a3979ea` `fff7b53` (no Claude-side `claims.rs` exists in the reference; claims are Codex-only, W8); usage `c048a01` `5aa6701` `89b8a9a`; render-table `27a861e` `826122d` `c0f7729` `aaafbfb` (the lane itself retracted two claims: its exact-byte tests append the LF inside the test, and the display-width corpus was not compared against the reference). Verification at `b49f3d0`: gates green on both tags (1277 / 1303 tests), argv parity proven, width parity MEASURED against unicode-width 0.2.2 (7 / 7 identical), one blocker: no printer emits the trailing LF, so the exact-byte claim is unproven → assigned to `claude-status`, which owns the stdout path. Review: APPROVE, two MINOR moves (`KeychainLineLimit` and the line assembly into `internal/secret` mirroring the reference layout; duplicated headline scope removed from `provider/claude`) → `fix-w3` lane; all thirteen declared divergences judged acceptable |
| W4 | `claude-status` (discovery, status pass, JSON v1, partial exit, `internal/app` handler composition), `claude-accounts-read`, `claude-token-refresh` (the refresh grant and profile GET split out of the W5 login lane because the status refresh barriers need them) | W3 lanes done; started 2026-10-05 while `verify-w3`/`review-w3` run | e2e goldens (normalised + exact-byte); `status.v1.json` validation. **Landed**: token-refresh `6a12fba` `f04ec63` `3ba4f21` (done); status `6684d14` `2d3fc1e` `a663fae`, handler composition `c18eca1`, `main.go` wiring with the smoke expectation `claude status` → exit 2 on an empty store `4efe497`, exact-byte printer `render.Print` closing the W3 blocker `25c36f5`; accounts-read `ad27ad8` `c18eca1`. Session 3 resume lanes done: `claude-status-2` coordinator runner `aa87d5e`, `e2e_status.txtar` + in-process `testutil.ScriptCmds` `ce31a3a` (13 of 22 reference status e2e tests reachable; the 9 others need the refresh write path, the profile GET persistence or doctor); `claude-accounts-read-2` printer route `a0c501f`, `e2e_accounts.txtar` `8370554` (the reference has no standalone list/show e2e, so six `accounts_tests.rs` contracts are promoted to subprocess cases with a stdout oracle captured from the reference binary and embedded in the txtar; the eleven write/doctor e2e tests are deferred to their waves). **All W4 lanes landed 2026-10-05 21:47 JST.** Verification + review of the `895ece3` snapshot: W4 FAIL / REQUEST_CHANGES (gates green on both tags, lint clean; blockers: `refresh.go` still a stub with a contract mismatch against the landed `RefreshAccess`, no migrated-item path, no pending resolution before the cache early return, unbounded `os.ReadFile` and a blocking open in discovery (FIFO hang reproduced), staged temps without emergency cleanup; 12/1/9 of the 22 reference status e2e tests covered). Fix round (session 3, from 22:05 JST): all findings closed by `fix-w4-status` (`c19e9e6` `d1bba2f` `880fb83` `e6e9854` `f46b155` `9108486` `9ea9c2d`) and `fix-w4-secret` (`319fd9e` `bd949d0` `b3aa3a4` `59d2b1f` `f873cc7`); re-verification runs with the W5 gate on `9ea9c2d`. `internal/app` composes handlers per family through a registry (`029371c` `245e3cb`) so parallel lanes never edit one literal |
| W5 | `claude-watch` (tui + watch loop + log buffer), `claude-login` (PKCE, loopback, browser opener), `claude-file-store` (secret_file, file_store, pending, audit), `claude-import-accounts-write` | W4 | e2e + frame-dump goldens; `tests/e2e_login.rs`, `e2e_import.rs`, `e2e_accounts.rs` parity. **Started 2026-10-05 22:05 JST in parallel with the W4 fix round** (user's decision to maximise parallelism): `claude-watch` DONE (TUI model `dd53caa`, terminal lifecycle with real PTYs `66e95b1`, watch loop `7805eaf`, `e2e_watch.txtar` `e8f3fb6`; the reference has no standalone watch e2e beyond the interval-floor smoke case); `claude-login` PKCE + loopback `a254a69`, exchange `3efc573`, login command + `ClearStaleFiles` `cd5b7a2` (all 12 reference login e2e tests reachable; `e2e_login.txtar` pending); `claude-import-accounts-write` DONE (import plan `208d7b6`, shared prompting `9f4b307`, `import --from keychain` `2d52828` registry-only as the reference, accounts remove/relocate/forget/unforget `030315e`, `e2e_import.txtar` + `e2e_accounts_write.txtar` `7045063`: 5 + 7 reference tests all reachable; relocate reads the raw profile document because the reference needs only `organization.uuid`). `fix-w4-secret` DONE (keychain writer `319fd9e`, staged-temp cleanup `bd949d0`, redaction + profile factories `b3aa3a4`, `CurrentAccount` + tagged write fault seams `59d2b1f`, unprivileged permission test + lint config `f873cc7`). `fix-w4-status` DONE (discovery safety `c19e9e6`, `ProfileDocument` `d1bba2f`, file-row refresh barrier `880fb83`, migrated-keychain refresh + pending before cache `e6e9854`, token-response containment + hidden-failed exit test `f46b155`, unavailable-flock testing seam `9108486`, the nine status e2e cases + `e2e_refresh.txtar` with all 28 reference refresh tests mapped and the pass-deadline-as-busy contention classification `9ea9c2d`; the held-lock reference cases use a blocked loopback token holder instead of `hold_lock`). `claude-login` DONE (`e2e_login.txtar` `3b9b525`, 12 reference tests + loopback success). Cross-lane APIs: `secret.KeychainWriter`, `commands.Prompter`, `commands.ClearStaleFiles` (login → relocate), `ProfileDocument` (status → relocate). **Early**: `claude-file-store` done (five commits in session 2 plus `eaf44b8`, the required-member refusal of the lock-break audit record); the concurrent first-creation ENOENT of the audit log (concurrent `O_CREAT` opens on APFS) was fixed by `claude-peer-locks-2` in `895ece3` (existing log opened without creation flags, exclusive creation on ENOENT, reopen on EEXIST). `runtime-core` done (six commits in session 2 plus FIFO cleanup `9e288a8` and the forced signal exit `4efe497`). **Gate of the W4 fix round + W5 (`verify-w5`/`review-w5`, snapshot `777108f`): all section 12 gates green on both tags (4468/4704 pass events, lint `0 issues.`, eight root scripts twice, TUI goldens byte-equal, 33 commits signed); REQUEST_CHANGES on four items, all fixed: per-attempt PKCE exchange body `7b8201b`, credential blob wiped right after its write in login/relocate `5d3cc81`, account headings local `6e3fac3`, login children bound to the test context `cfb218c`. Re-verification (`verify-w5b`, snapshot `cfb218c`): gates green on both tags, three fixes confirmed by mutation, one test-adequacy gap closed by a real child-cancellation test `8a4d99a`; the last schema identifier left by the comment cleanup removed in `06cf9a1`. **W4 and W5 closed.** Inherited and recorded, not changed: the migrated duplicate-service refusal is unreachable through discovery because both implementations compact the listing by service first (`discovery.rs:115-116`, `status.rs:1395-1398`). Deferred: the doctor assertion of the hidden-sibling status case (W6), the root-only branch of the pending permission test (privileged CI). Join semantics checked by the status lane: both `coordinator.RunPass` (`coordinator.go:411`) and the reference (`coordinator.rs:491-514,551-559`, `thread::scope`) join started workers unconditionally and the 500 ms constant bounds only the watchdog sweep, so there is no parity gap there |
| W6 | `claude-isolate-export` (use/exec/env, session seed), `claude-doctor` (incl. `--remove-stale`, held-lock records), `claude-peer-locks` (claude_lock, config_lock, foreign_activity) | W5, W2 | exit-code table (0/1/2 + 10–30 where reachable); `e2e_isolate.rs`, `e2e_doctor*.rs` parity. **Landed** (session 3, from 00:55 JST 2026-10-06): `claude-isolate-export`: session seed + exported seed policy lists + canonical config rendering `edd3e98`, env rendering per shell `a00da26`, child-exit transport (`Dependencies.Signals`, `errs.ChildExit`, main exits silently with the child's code) `045c1dc`, exec lifecycle with real-child signal tests `ca10dd7`, isolated `use` incl. `--json` `64873ef`, `e2e_isolate.txtar` `6952073` (21 reference tests: 17 mapped, the four `--forget` cases deferred to W7; `--new-only` is a no-op synonym in isolated mode and isolated `use` writes no credential, as `use.rs:191-251`; the env comment drops the reference's identifier suffix). `claude-doctor`: report + isolation model `f61facd`, real-process heartbeat sampling `2235fb9`, `--remove-stale` (macOS-only, `--yes` strictly required, no audit record, two samples over the doctor interval, as `doctor.rs`) `aa8f403`, `e2e_doctor.txtar` `c636314` (16 named cases incl. the four doctor cases of `e2e_accounts.rs` and the hidden-sibling assertion deferred from the status wave; vanished/unreadable heartbeat observations count as held, a conservative divergence). **Gate** (`verify-w6`/`review-w6`, snapshot `6952073`): every section 12 gate green on both tags (4710/4954 pass events, ten root scripts twice, env and doctor stdout byte-equal to the reference binary modulo the name); one MAJOR found and reproduced by both lanes: doctor's stale removal re-baselined its heartbeat sample after the warning output, so a lock touched during that output could be removed; fixed in `ac7b86f` (the age-qualified first observation is carried through the interval for removal and the namespace report; warning-boundary regressions proven by mutation) and re-verified PASS by `verify-w6b`. Follow-up [testutil]: the doctor script's identity assertion also matches a UUID in the temporary parent path. **W6 closed.** **Early**: `claude-peer-locks` landed constants/clock `a646c57`, held-lock records + foreign activity `9aeb8f0`, stale proof + automatic break `32dfbef`; `claude-peer-locks-2` (session 3) landed the acquisition protocol with twelve tests `10dab64`, the config lock `6200fd0`, and the audit creation fix with the lint cleanup `895ece3` (`golangci-lint` now prints nothing repo-wide); `TrackTemp` duplicates the directory descriptor in place of the reference's shared owned fd. Decision 2026-10-05 21:17 JST: the peer release unregisters its emergency entry before `rmdir` (the config lock's ordering) so a signal between the two cannot remove a peer's freshly created lock; recorded as a deliberate divergence |
| W7 | `claude-use-live` (swap, adopt, claude_json rewrite, undo, catch-up) | W6 | `e2e_swap.rs` parity (129 tests) minus RC; byte-exact round trip; refusal matrix. **Done** (session 3, 01:31 to 08:02:02 JST 2026-10-06) as three lanes: `claude-swap-core` (`8662f57` `6a2586f` `2644303` `22c8179` `98e676f`), `claude-use-undo-forget` (`319f164` `f79fc1c` `c4854f5` `1e59353` `e5ec993`), `claude-use-live` (`a65db1b` `f5b1a5b` `13c1db0` `e734409` `6b8b230` `b53f38e` `0e4951a` `59da7da` `7006e83` `d92ea0d` `5f37e6c` `82089e3` `4122a9b` `10363b6` `6c17d73` `57f234d` `c4a0322` `ab989ba` `3245476` `52bcd50`). Coverage: all 129 applicable `e2e_swap.rs` functions and the four `--forget` isolate cases mapped to `e2e_swap.txtar`/`e2e_isolate.txtar`; the 34 automated remote-control action tests and `--restart-remote-control` are deferred to a later wave (typed not-implemented refusal, exit 1) |
| W8 | `codex-home-auth` (home, config.toml, auth_store, credentials, claims, proof/permit/lock), `codex-status-accounts` (pass, status, JSON v2, accounts, watch adapter), `codex-refresh` (oauth classification from W2, refresh state machine, accounts_refresh), `codex-login-import-doctor` (login child, import, doctor table + JSON) | W4, W2 | `e2e_codex_*.rs` parity; `status.v2.json`, `codex-doctor.v1.json` validation. **Done** (sessions 4 and 5, 07:10 to 10:27 JST 2026-10-06): `codex-home-auth` (`61bdb7a` `c242519` `b60467a`), `codex-status-accounts` (`fd518ae` `b278349` `a4d9b11` `a698074` `a27e384` `cd6a417` `47bb541`), `codex-refresh` (`41b18df` `77aafa7` `10520d8` `06a0783`), `codex-login-import-doctor` (`47cee43` `26eec74` `cbe04a4` `4cf7a9a` `04cd365` `c61429f`), `codex-body-race` (`f784164`: the refresh POST body was wiped while net/http could still read it); `fd9d4f3` adds go-toml. Coverage: all 106 `e2e_codex*.rs` tests mapped (103 exact txtar headings + 3 harness cases as real-child unit tests). **Gate** (`verify-w8`/`review-w8` on `f784164`): every section 12 gate green on both tags (7,072 + 7,546 pass events, 19 scripts twice, 0 leaks over 1,374 fixture files); FAIL on three wire items (status `--json` final LF, raw email swap-removal order, child-exit refusal text) + one modernize diagnostic; review added the quadratic duplicate lookup in the ordered decoders. Fixed in `fbf4a1e` `9665fc6` `6ed667b` `708f91f` `08dea5f` (+ test nit `4822c9d`); `review-w8b` APPROVE; `verify-w8b` W8 PASS: both-tag race suites `-count=2` (7,140 + 7,614 pass events, 0 failures), 19 root scripts twice, wire parity 20/20 against the reference binary, schemas and the three goldens valid, 106/106 reference tests mapped, zero sentinel leaks over 1,374 fixture files and 476 sinks. Deliberate divergences recorded in `.omc/handoffs/w8/*-reference-tests.md`: the resend prompt drops `(risk R63)`; marker digests validated as 8 lowercase hex in the reader; the tagged abort seam exits 134; a signal during login removes the whole scratch leaf; vendor number tokens are kept verbatim where the reference canonicalises through f64; refusals exit 2 rather than forwarding the child's code. Follow-ups: go-toml's amd64 SIMD path uses pre-1.27.1 `archsimd` names (Linux builds need `GOENV=off` until the dependency is fixed); `swap_cleanup_script.go` uses chflags (Darwin) |
| W9 | `release-gate` (`scripts/release-gate.sh`), `docs` (README, completions install lines), `parity-signoff` (both binaries on one store), review lanes (`code-reviewer`, `security-reviewer`, `verifier`) | all | gate script green; parity report; reviews accepted. **Done** (session 5, 10:17 to 11:18 JST 2026-10-06): `64894e9` CI race deadline, `dd9dc40` release gate (19 strict seams absent untagged and present tagged, 15 seam file pairs, 4 production controls, four completion parsers, elvish refused), `b059a4d` README, `b371ce9` + `b650179` parity (15 commands per binary; 137 PASS / 0 FAIL / 22 SKIP; the SKIPs are the keychain and endpoint steps the untagged build cannot redirect). Gate fixes: `289df4e` `e71769f` (scripts: Bash-3.2-safe arrays, `GOFLAGS` cleared and the artifact's `-tags=` metadata checked, fail-closed EXIT guards, provenance label, SHA-256 of both executables). Security fixes: `0f1dbb8` (Claude refresh/login POSTs refuse redirects, `GetBody` nil, body close synchronised before the wipe), `934c6b4` (pending/meta/current and partial read buffers wiped), `e43ea9c` (lock body read bounded, no-follow, nonblocking), `308371e` (usage/profile GETs refuse redirects). `c203eef` normalises the clock-relative expiry in the invariance fixture. Reviews: `review-w9` REQUEST_CHANGES → fixed; `security-review-p7` REQUEST_CHANGES → `security-review-p7b` APPROVE (all findings CLOSED and CONFIRMED on pre-fix overlays; govulncheck 0 reachable); `verify-w9` FAIL → `verify-w9b` VERIFIED except the fixture gap; final gate on `308371e` PASS: both-tag build/vet, race suites -count=1 -timeout=30m on a git archive (24 + 25 package ok lines incl. the root scripts at 410 s, 0 failures, no race reports), lint 0 issues on both tags |
| W10 | `parity-vm` (`scripts/parity-vm.sh` host lifecycle: prepare/snapshot/run/compare/reset/destroy with `TART_NO_AUTO_PRUNE=1`; `scripts/parity-vm-guest.sh` for the VM's Terminal.app; `scripts/lib/parity-common.sh` shared with `parity-signoff.sh`), fix lanes `fix-w10` (ownership after verified clone, scalar-type regression FAIL, stale snapshot base refused) and `fix-w10b` (nonce sidecar in the VM directory as creation proof, lock owner record), gates `verify-w10`/`review-w10`/`security-review-w10` and re-gates on each pin | scripts `f46e531`, fixes `ac27460` `c22dca6`; close on `c22dca6` (review-w10d APPROVE, security-review-w10d APPROVE, verify-w10d VERIFIED) | ✅ scripts stage done 2026-10-06 15:42:51 JST; operator keychain run done 2026-10-09 23:28:29 JST for the Go artifact only (`docs/research/agctl-parity.md` "Manual run": six of seven commands exit 0, the undo refuses by the shared discard rule, Claude and Codex real usage success observed; the reference half not run because the default reference executable is a `testing` build with a disabled keychain reader; guest/compare script fixes `c37f763` `c692a46` `b395fc2` `6d114a4` `6d66e37` `77ed8e0`, record `4c40212` `3d7b04a`, verify-w10e2 VERIFIED) |
| W11 | hygiene follow-ups (`hygiene-goexperiment`: `GOENV=off`/`GOWORK=off` + exact-toolchain self-check, then fault stems promoted to strict seams with a constant-block inventory; `hygiene-faultstems`: fault-point names as tagged constants in `internal/runtime/fault`, `FlockENOTSUP`, direct selectors; `hygiene-chflags`: Darwin-only `swap-immutable` with a failing `!darwin` fallback; `hygiene-ci`: shellcheck + release gate on `xcode-27`; `ledger-w9`: w9 ledger gap restored), gates `verify/review/security-review-w11*` | `6ce537d` go-toml pin, `c42db0d` shfmt, `b566bc9` strip-insta SC1007, `31c0732` scripts env isolation, `a150440` testutil, `5707842` ci, `7970620` runtime fault constants, `0387843` secret selectors, `0bc5885` strict fault gate, `a3be959` fault source-contract test, `fc16949` release-gate comment, `ed58fd7` lint directive (hosted macos job green on run 37562905961) | ✅ closed 2026-10-07 11:33:25 JST on `fc16949`: the fault source-contract test landed as `a3be959` (seven gate rounds on fresh verifier/reviewer lanes; the resolver covers the receiver-shape inventory documented at `expressionType` and rejects a dot import of package fault) and the release-gate comment now states the test (`fc16949`) |
| W12 | `faulttypes` (fault source-contract resolver moved from go/ast parser objects to go/types via `golang.org/x/tools/go/packages`: method identity through `types.Info` selections, qualified `fault.<Const>` spelling rule, forwarding helpers to a fixed point over `*types.Func`, fixtures type-checked against the real packages through a shared importer; the former INFO shapes comma-ok declarations and cross-file unqualified method expressions are violations by construction), fix lanes `faulttypes2` (tuple-expanded method expression, module root under `-trimpath`, restored helper-shadowing controls), `faulttypes3` (per-variant `types.Info` keyed by package ID + physical path, module root validated by `modfile.ParseLax` against the module directive with an exact-or-slash package filter, diagnostics sorted by position before compaction), `faulttypes4` (physical positions via `PositionFor(pos, false)` so `//line` directives cannot move a caller out of scope, onto an exempt file or merge two physical call sites; forwarding coverage across retained package variants in both load orders; cwd-only fail-closed root discovery with nested/malformed/missing-directive fixtures); gates `verify-w12a/b/c/d`, `review-w12a/b/c/d` on fresh lanes each round | W11 | one commit `280504e`: 199 table cases per mode (13/94/2/9/57/17/7) plus the real source contract; both-tag tests, `-trimpath`, `-race`, vet, golangci-lint, build green on go1.27.1 (release-gate pin) and go1.27.2 (hosted pairing); `x/tools` v0.51.0 and `x/mod` v0.41.0 direct | ✅ closed 2026-10-09 21:18:34 JST on `280504e` (verify-w12d VERIFIED, review-w12d APPROVE with 0 findings) |

Lane rules: astra `codex-executor` (or `codex-test-engineer` for test-heavy lanes),
English brief, repo root with absolute paths, no `cd`, `/commit` + push when the lane's
gate is green, `shutdown_request` on acceptance. Shared packages (`render`, `testutil`,
`secret` primitives) land before their consumers; later lanes only add files to them.

## 9. Implementation steps (file references)

### W1 foundation

1. `main.go`: build the root `cobra.Command`; `ExecuteContext` with a context cancelled by
   TERM/HUP/INT; after the command returns, wait for an in-flight signal exit
   (`src/main.rs:50-56`); print `agentctl: {err}` and exit via `errs.ExitCode(err)`
   (`src/main.rs:59`); `SilenceUsage`, `SilenceErrors`.
2. `internal/cli`: constructors per group (`newClaudeCmd`, `newCodexCmd`,
   `newCompletionsCmd`); flags and help from `src/cli.rs:380-842`; duration grammar from
   `src/cli.rs:286-318` with a table test; swap exit constants from `src/cli.rs:69-234`;
   `completions` writes with `GenBashCompletionV2`/`GenZshCompletion`/`GenFishCompletion`/
   `GenPowerShellCompletionWithDesc`, treats `EPIPE` as success
   (`src/commands/completions.rs:45-47`), rejects `elvish` with a documented error.
3. `internal/errs`: `AppError` kinds and exit mapping (`src/error.rs:88-160`), `Io`
   displays context only (`:94-100`), partial aggregation helper counting shown failures.
4. `internal/config`: resolution order (`src/config/paths.rs:107-116,137-140`), XDG on
   macOS via `os.LookupEnv("XDG_CONFIG_HOME")` + `os.UserHomeDir` (never
   `os.UserConfigDir`), tree derivation (`:177-312`), identifier validation (`:347-522`),
   mode enforcement (`:74-77`), registry read/update under `.config.lock` with
   `golang.org/x/sys/unix.Flock` and inode-persistent lock files
   (`src/config/mod.rs:83,303`; `src/secret/namespace_lock.rs:59-88,250-294`).
5. `internal/testutil`: mirror `tests/common/mod.rs` (temp config/home/bin tree
   `:133-162`, endpoint close-by-default `:143-154`, USER/LOGNAME pin `:160-161`, env
   scrubbing `:799-903`, registry/credential builders `:431-503`, fake keychain
   install `:529-774`, flock holders `:1193-1255`, bounded polling + pipe draining
   `:1267-1374`) and `tests/common/codex.rs` (`:83-95,340-465`). Golden helper with
   `-update` (`flag.Bool` in the test package; `go test ./... -args -update`),
   Insta-style end-of-file trim comparator, frame-dump adapter for `teatest` output,
   schema validator over embedded `schemas/*.json` (`jsonschema.UnmarshalJSON` →
   `AddResource` → `Compile`). `scripts/strip-insta.sh` converts the 14 `.snap` files to
   `testdata/*.golden` and records the mapping. E2E harness: the fake `security`,
   `codex` and (later) `tmux` executables are the **shell scripts copied verbatim from
   `fixtures/`**, installed by `Params.Setup` into an owned `bin/` dir and pointed at by
   `AGENTCTL_SECURITY_BIN` / `AGENTCTL_CODEX_BIN` (the Rust contract,
   `tests/common/mod.rs:529-553`); they are never reimplemented in Go. `testscript.Main`
   in `TestMain` registers only Go **helper** commands (`flockhold`, `sigterm`,
   `drainpipes`, `mtime`, `waitfor`, `schema`, `golden`). `Setup` also sets
   `AGENTCTL_CONFIG_DIR`, `HOME`, `USER`, the loopback endpoint overrides, and puts the
   built `agentctl` (tagged) first on `PATH`; scripts live in `testdata/script/*.txtar`,
   one per Rust e2e file.
6. `.github/workflows/ci.yaml`: `runs-on: xcode-27` (full suite, both build tags) and
   `ubuntu-26.04` (build + unit only until P9); actions pinned at major; block-style
   sequences; gates from section 12.

### W2 spikes

7. `internal/provider/codex/refreshclass.go` + table test: classify a failed POST into the
   Rust outcome classes (`src/provider/codex/oauth.rs:398-429`: not sent / sent-unknown /
   definite response) using `httptrace.ClientTrace{WroteRequest, GotFirstResponseByte}`,
   `errors.As` over `*url.Error` (`Timeout()`), `*net.OpError`, `context.DeadlineExceeded`,
   `io.ErrUnexpectedEOF`, `*tls.CertificateVerificationError`; per-phase deadlines
   2/3/2/2/8/2 s reproduced with a custom `net.Dialer`, `ResponseHeaderTimeout` and a
   body read deadline; `CheckRedirect` refuses redirects; `Transport` with
   `DisableKeepAlives`, no retries (document that `net/http` retries idempotent requests
   only on reused connections, which keep-alive disablement removes). Fault matrix with
   `httptest.Server` + raw `net.Listener`: refuse, hang before headers, close after request
   written, RST mid-body, TLS failure. Unknown classes never resend.
8. `internal/provider/claude/claudejson.go` + corpus test. `jsontext` does not preserve
   whitespace on re-encoding (`Encoder.WriteValue` reformats), so the edit is a byte
   splice: (a) reproducibility gate, as in Rust (`src/provider/claude/claude_json.rs:277`):
   re-format the untouched document with the same pretty-printer Claude Code uses
   (`jsontext.Value.Format` with 2-space indent) and refuse unless the result is
   byte-identical to the input; (b) locate the `oauthAccount` value and the five cache
   keys (`:292`) with `jsontext.Decoder` (`ReadToken`/`ReadValue`, start offset captured
   before each read, end offset from `InputOffset()`); (c) copy the original bytes
   verbatim outside those spans, encode only the replacement value, drop the deleted
   members together with their separators; (d) unchanged documents return the original
   bytes. Corpus from `src/provider/claude/claude_json_tests.rs:292-1055` (floats,
   escapes, duplicate keys, CRLF, trailing newline, symlinked target).
9. `internal/runtime/proc` Darwin spike: same-UID `claude` process observation and
   own-writer-gone proof (`src/runtime/proc/macos.rs:350,382,416-463`) via
   `unix.SysctlKinfoProcSlice("kern.proc.all")` / `kern.proc.pid.<pid>`. Field map
   (Rust `proc_bsdinfo` → `unix.KinfoProc`): `pbi_status` → `Proc.P_stat` (holder
   state, `src/runtime/proc/macos.rs:91-119`); `pbi_pgid`/`pbi_ppid` → `Eproc.Pgid`/
   `Eproc.Ppid` (`:120-140`); `pbi_start_tvsec`/`tvusec` → `Proc.P_starttime`
   (`:161-187`); `pbi_uid` (effective) → `Eproc.Ucred.Uid`, and the Rust comparison
   against the caller's `getuid()` (`:242-250`) is reproduced with `Eproc.Pcred.P_ruid`
   vs `os.Getuid()` so the real UID is what is compared; name: `proc_name` returns
   `pbi_name` (32 bytes) with `pbi_comm` (16) as fallback (`:454-463`), whereas sysctl
   only exposes `Proc.P_comm` (16 bytes + NUL). The exact match is `claude` (6 bytes), so
   `P_comm` is sufficient; the spike records this limit and tests a 17+ byte process
   name to show the behaviour is a documented non-match, not a crash. Table-test against
   `src/runtime/proc/macos_tests.rs` and a spawned child; `ps` is not acceptable
   (`src/runtime/proc/macos.rs:10-16`). A field that cannot be matched is reported, not
   approximated.
9a. `internal/secret` memguard spike: `Secret` over `memguard.Enclave`; `Open()` →
   `LockedBuffer` → I/O → `Destroy()` in one helper (`WithPlaintext(func([]byte) error)`);
   `memguard.Purge()` wired into `internal/runtime/signals` before `os.Exit` and into the
   normal exit path; measure `LockedBuffer` count against `RLIMIT_MEMLOCK` on macOS arm64
   (16 KiB pages) with the maximum number of simultaneously open buffers the swap path
   needs; confirm `CatchInterrupt` is **not** used (our handler owns signals).

### W3–W7 Claude

10. `internal/secret`: `Secret` type (memguard `Enclave` inside; `WithPlaintext`,
    `Digest()` computed inside the locked buffer, `String()`/`GoString()`/`Format()`/
    `LogValue()`/`MarshalJSON()` on value receivers return `[REDACTED]`; credential
    documents that must carry the token to disk use a separate encoder path, never
    `MarshalJSON`); reader interface + classes Locked/Unavailable/Timeout/NotFound/Other
    (`src/error.rs:50`); `security(1)` read transport with argv exactly as
    `src/secret/security_cli.rs:59-69` and timeouts 2 s/10 s, stdout parsed into the
    enclave and the raw buffer wiped; write transport
    (`src/secret/keychain_write.rs:67-97,495`) with stdin pipe, 1.2 s, 800 ms verify read;
    `secret_file` (exclusive temp, fsync, rename, `src/secret/secret_file.rs:189-328`);
    `file_store` with dirfd traversal (`unix.Openat` with `O_NOFOLLOW`,
    `src/secret/file_store.rs:513,886`); `pending` table (`src/secret/pending.rs`);
    `namespace_lock`; `audit` (`src/secret/audit.rs:599-1149`).
11. `internal/provider/claude`: credentials (`credentials.rs:271,427-524`), namespace
    (`golang.org/x/text/unicode/norm` NFC then SHA-256, `namespace.rs:133-136`), discovery,
    usage (+ 9 usage fixtures as parser vectors), oauth (PKCE with `crypto/rand`, loopback
    on 127.0.0.1, exchange response fixture), account states (`account.rs:218-242`).
12. `internal/render`: lipgloss table with `StyleFunc` for psql look, custom reset
    alignment pass and continuation rows (section 7.4); JSON v1 + isolation report;
    goldens from the eight `agctl__render__table__tests__*.snap`.
13. `internal/commands`: `status` (`src/commands/status.rs:185-219` partial exit; refresh
    barriers `:1181,1463,1565,1847`), `accounts`, `watch` loop (`src/commands/watch.rs`),
    `login`, `import` (plan from `src/config/import.rs`), `isolate` + `export`, `doctor`
    (`src/commands/doctor.rs:184-267,1003-1243`), `use` (`src/commands/use.rs`; swap
    driver `:2322-2520`, RC preflight `:191-214` returns 30 with `unsupported` reason until P8).
14. `internal/tui`: Bubble Tea v2 model from `src/tui/app.rs` (reducer) and `ui.rs`
    (frame); frame-dump goldens from the three TUI snapshots; terminal restore on
    return/panic/signal (`src/tui/mod.rs:121-191`).

### W8 Codex

15. `internal/provider/codex`: `home.rs` (config.toml via go-toml, storage mode, daemon
    evidence), `auth_store.rs` (sole `auth.json` I/O, receipts with explicit
    audited/closed state), `credentials.rs` (ordered document, four-leaf refresh merge,
    unknown members preserved via `jsontext`), `claims.rs`, `proof`/`permit`/`lock`
    (unexported constructors), `refresh.rs` (state machine, markers, floors),
    `oauth.rs` (single POST using W2 classifier), `usage.rs`, `audit.rs`, `login_child.rs`
    (allowlisted env, owned scratch `CODEX_HOME`).
16. `internal/commands/codex`: `pass`, `status` (JSON v2, `status.v2.json`), `watch`
    adapter (never refreshes), `accounts` + `accounts_refresh` (real implementations at
    `src/commands/codex/accounts.rs:90-100`; ignore stale "stub" comments at
    `src/commands/codex/mod.rs:7-18`), `import`, `login`, `doctor` (+ `codex-doctor.v1.json`).

### W9 release

17. `scripts/release-gate.sh`: build with `-trimpath -ldflags='-s -w'` **without** the
    tag; `rg -a -c -F` must find none of the testing names in section 6 nor the witness
    strings; positive controls `AGENTCTL_CONFIG_DIR`, `AGENTCTL_CLAUDE_USER_AGENT`,
    `AGENTCTL_CLAUDE_OAUTH_SCOPES`, `AGENTCTL_CODEX_USER_AGENT` must be present; a tagged
    negative-control build proves each scanned string can appear (mirrors
    `scripts/release-gate.sh:148-295` of the Rust repo). A `go vet`-style source check
    (`go list -tags agentctl_testing -deps` vs untagged) confirms the seam files are the
    only tagged files.
18. `README.md`: commands, completions install lines per shell, elvish unsupported note,
    env var table, store compatibility statement.
19. `scripts/parity-signoff.sh`: `cargo build --release` in the Rust repo, run both
    binaries against one temp store with the fake keychain, diff registry files and
    `status --json` after each write command; report in `docs/research/agctl-parity.md`.

## 10. Testing seams

`cfg(feature = "testing")` → `//go:build agentctl_testing`. Each seam is a pair of files
(`<name>_testing.go` / `<name>_release.go`) exposing one unexported factory; production
code calls the factory and never reads the env var. Seams (from
`docs/research/agctl-inventory-invariants.md` (d)): keychain backend/binary selector,
fault injection (`AGENTCTL_FAULT`, `AGENTCTL_FAULT_RESUME`, pause hooks with 10 s cap and
20 ms poll), endpoint selectors for six URLs, `AGENTCTL_NO_BROWSER`, swap deadline, RC
budget, tmux binary + fake-prefix passthrough, codex binary + fake-prefix passthrough,
unaudited-receipt tracker, lock-order witness. Unit tests prefer injected interfaces
(reader, clock, command runner, HTTP base URL); the tag is for subprocess e2e. A
`TestMain` in the e2e package fails fast when the binary was built without the tag
(the Go counterpart of `tests/feature_guard.rs`).

## 11. Expanded test plan

| Level | What | Tooling | Oracle |
|---|---|---|---|
| Unit | exit mapping, duration grammar, path resolution, identifier confinement, modes, reset formatting, table alignment, credits cell, percentage flooring, NFC namespace hash, 4032-byte boundary, redaction of every secret-owning type, refresh classification, jsontext round trip, pending-replay table, adopt decision table, stale-proof sampling with injected clock | `go test`, `map[string]struct` tables, go-cmp, `t.Context()`, `t.TempDir()` | Rust `*_tests.rs` expectations cited per test |
| Integration | registry + locks on a real temp dir with two subprocesses contending; `security` client against `fixtures/fake-security.sh` (argv log); codex client against `fixtures/fake-codex.sh`; HTTP against `httptest.Server` and raw listeners (fault matrix); crash injection between temp/fsync/rename and marker/POST/save | `testutil.FakeBin`, `httptest`, `os/exec` of the test binary itself | fixtures, schemas, Rust e2e assertions |
| E2E | `testscript` txtar scripts, one per Rust e2e file; `Setup` builds the owned `bin/`, env and loopback endpoints; stdout vs normalised goldens (`cmp` with the trim helper) plus exact-byte checks; `--json` vs schemas (helper command `schema`); exit codes via `exec` / `! exec`; stderr sentinel scans (`! stderr sk-ant`) per `tests/e2e_tracing.rs:50-67,210,223` | `testscript` with registered helper commands | `.snap` bodies, `tests/e2e_*.rs`, `tests/cli_smoke.rs` |
| TUI | reducer transitions (`src/tui/app.rs:126-191`); frame output at 84×20 / 60×8 / 84×16 via `teatest.NewTestModel(..., teatest.WithInitialTermSize(w, h))`, `Send(tickMsg)`, `FinalOutput` through the frame-dump adapter | `teatest/v2` | 4 TUI snapshots |
| Observability | `AGENTCTL_LOG` level switch (invalid → warn); redaction in every attr; no secret in panics; buffered logs flushed after terminal restore; pipe-drain liveness (200,000 stderr bytes, `tests/e2e_tracing.rs:69`) | stderr captured by `testscript` | Rust tracing tests |
| Release | seam absence/positive-control scan; tagged negative control; completions parse in bash/zsh/fish (`-n`) and pwsh if present | `scripts/release-gate.sh` | Rust `scripts/release-gate.sh` |

## 12. Go gate list (every lane, every commit)

```
gofmt -s -l .                                   # prints nothing
gofumpt -w -extra .
modernize -fix -test ./internal/<pkg>/...       # scoped to the edited package only
goimports-rereviser -project-name=github.com/zchee/agentctl -use-cache -cache-fast-skip -rm-unused -set-alias -recursive .
go vet ./...
golangci-lint run ./...
go test -race -count=1 ./...
go test -tags agentctl_testing -race -count=1 ./...
```

## 13. Pre-mortem (3 scenarios)

1. **`use --live` leaves a user without a working Claude Code login.** Cause: a refusal
   fires late or a CAS discard is reported as applied. Prevention: W7 starts only after the
   exit-code table (section 3.1) is enumerated with its triggering conditions and the W2
   jsontext gate is green; adopt decisions are table-tested from
   `src/provider/claude/adopt_tests.rs`; `--undo` runs on the same fixtures; the security
   review lane signs off `internal/secret` and `internal/provider/claude` before P7.
2. **A testing endpoint override or fault hook ships in the release binary.** Cause: an
   env lookup written into production code for convenience. Prevention: paired
   `_testing.go`/`_release.go` files, the release gate's binary scan with positive and
   negative controls, and CI failing the release job on any hit.
3. **Codex refresh re-sends a rotated refresh token and the account is locked out.**
   Cause: Go's error surface differs from `ureq 3.4.1` and an ambiguous send is classified
   as "not sent". Prevention: W2 fault matrix on real sockets, unknown outcomes default to
   no automatic resend, durable marker before POST, floors table-tested
   (`src/provider/codex/refresh_tests.rs:264-1288`).

## 14. Risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| `charm.land` v2 API drift | watch/table lanes blocked | versions pinned in `go.mod` from the research report; one adapter type wraps the table API |
| `encoding/json/v2` formatting differs from `serde_json` (`preserve_order`, `float_roundtrip`) | goldens and byte-exact rewrites differ | `jsontext` token path for documents that must round-trip; goldens compared byte-wise; divergences listed for user sign-off |
| `security(1)` output differences across macOS versions | keychain reads fail | same parsers as Rust; `security-dump.txt` fixture; doctor reports parse failures |
| Display width (CJK, combining, emoji) | misaligned columns | use lipgloss width (research lane verifies East Asian handling); add width cases beyond the ASCII snapshot corpus |
| `sysctl KERN_PROC` lacks a field the Rust libproc path uses | identity proofs weaker than Rust | W2 spike maps every field; a missing field is escalated to the user (fallback route: `purego` + libproc), never approximated |
| memguard: `RLIMIT_MEMLOCK`, 16 KiB Darwin pages, `Purge` ordering with signals | `ENOMEM`/`EPERM` on open, secrets left unpurged on a signal exit | Enclaves (heap, encrypted) by default, one `LockedBuffer` open at a time per operation, `Purge()` in the signal handler before `os.Exit`; W2 spike measures the peak open-buffer count |
| teatest/v2 pseudo-version vs bubbletea v2.0.10 | TUI tests fail to build | W1 verifies the pair; if incompatible, TUI tests fall back to direct `Update`/`View` calls and the user is told |
| Rust comments contradict implementation | wrong behaviour ported | implementation is normative (section 2); divergences recorded in `docs/research/agctl-parity.md` |
| Insta-trimmed snapshots mistaken for stdout | false parity | normalised comparator + exact-byte tests (section 7.5) |
| Parallel lanes editing shared packages | conflicts | shared packages land first; later lanes add files only; `/commit` per lane |
| Worker model not astra | cost/quality | every lane self-reports on line 1 (all four P0 lanes reported astra) |

## 15. Acceptance criteria (testable)

1. Section 12 gates pass on macOS arm64 with go1.27.1 for both build tags.
2. Every subcommand in section 3 has an e2e test; table output equals the corresponding
   `.snap` body under the end-of-file trim rule, and one exact-byte test per table asserts
   the untrimmed stdout (modulo `agctl`→`agentctl`, `AGCTL_`→`AGENTCTL_`).
3. `claude status --json` validates against `status.v1.json`; `codex status --json`
   against `status.v2.json`; `codex doctor --json` against `codex-doctor.v1.json`; the
   internal isolation report against `doctor.v1.json`. `claude doctor` has no `--json`.
4. A table test covers exit codes 0, 1, 2 and every code in section 3.1 with its
   condition; e2e asserts each reachable one (30 asserts the `unsupported` reason until P8).
5. Parity sign-off: Rust `agctl` and Go `agentctl` on one temp store produce identical
   registry files and `status --json` after each write command.
6. `scripts/release-gate.sh` passes (absence of every testing name, presence of the four
   positive controls, tagged negative control).
7. Sentinel scan: no fixture token or `sk-ant` run appears in any captured stdout, stderr
   or golden across the e2e suite at `AGENTCTL_LOG=agentctl=trace`.
8. `completions {bash,zsh,fish,powershell}` parse in their shells; `elvish` exits 2 (cobra
   usage error) with the documented message.
9. Every constant in section 7.2 exists with a test; every invariant in section 7.1 has a
   named test.
10. `fake-security.sh` argv log for `login`, `import`, `use --live` equals the Rust e2e
    expectations; no `delete-generic-password` can appear (no code constructs it).
11. No `omitempty`, `interface{}`, `context.Background()` in tests, or multi-line function
    signatures; `gofumpt -extra`, `modernize`, `goimports-rereviser` clean.
12. `.github/workflows/ci.yaml` runs the gates on `xcode-27`; actions pinned at major.
13. No source comment references agctl or the Rust origin.

## 16. Verification steps (verifier lane)

1. Run section 12 gates from the repo root; attach output.
2. `go test -json ./... | jq` summary; test counts per package vs the Rust e2e table.
3. Run `scripts/parity-signoff.sh`; attach the diff report.
4. Run `scripts/release-gate.sh` on a `-trimpath -ldflags='-s -w'` build.
5. Security review lane (astra) over `internal/secret`, `internal/provider/*`,
   `internal/config` locks, `internal/runtime/signals`.
6. Inspect changed files for `TODO`, `t.Skip`, stub bodies before accepting any wave.

## 17. Execution rules for workers

- Spawn with the Agent tool: distinct `name`, no `isolation`, no `model`; types from
  `~/.claude/agents/codex-*` (`executor`, `test-engineer`, `code-reviewer`, `verifier`,
  `security-reviewer`, `critic`, `architect`); briefs in English; replies to the user in
  Japanese.
- Run from `/Users/zchee/go/src/github.com/zchee/agentctl` with absolute paths; never `cd`.
- Every timestamp from `date`; no plan markers (wave/lane IDs) in code, comments or commit
  messages; commit messages state the behaviour or constraint.
- `/commit` (gpg-signed, `-F` file, intent line, 72 columns gated with `exit 1`; trailers
  Codex + Fable + session, section 2) and `git push origin main` when a lane's gate is
  green; one commit per task; `git pull --rebase origin main` before every push, never a
  force-push.
- Package layout facts the lanes must respect: `//go:embed` cannot cross package
  directories and `testdata/` cannot be a package, so the schemas and fixtures live in root
  packages `schemas/` and `fixtures/` with an `embed.go` each (embed drops the executable
  bit; the install helper restores 0755 on the three scripts); goldens under
  `testdata/golden/` are located through a repo-root helper in `internal/testutil`. E2E
  files carry `//go:build agentctl_testing` so the untagged gate stays green; one
  `TestMain` per package (the root package's belongs to the e2e harness).
- Shut each lane down in the turn its report is accepted; check for stray `.omc`
  directories (`fd -H -I -t d '^\.omc$'`) before every commit.
- Lanes share ONE working tree and ONE git index. Learned 2026-10-05 when `cd81e64`
  (a cli commit) swallowed fourteen staged testutil files: every lane commits with
  `git commit --gpg-sign -F "$MSG" --only -- <its own paths>` so the index contents of other
  lanes never ride along, and runs `gofumpt -w -extra` and `goimports-rereviser` on its own
  paths only (`gofumpt -w -extra <files>`; `goimports-rereviser ... <dir>`), never `.`,
  because the repo-wide run reformats other lanes' in-flight files.
- Model self-identity: two foundation lanes reported `claude-fable-5` as their self-identity
  while the environment named `claude-gpt-6-astra-fast[1m]`; the user chose to continue
  with the frontmatter as authoritative and have every lane keep self-reporting on line 1.
- Deliver reports via SendMessage to the lead's teammate name (`team-lead`), not `main`.

## 18. Confidence scores

| Dimension | Score | Reason |
|---|---|---|
| Performance | 0.85 | I/O-bound CLI; bounded 4-worker passes; no hot loops; Go startup is not the bottleneck (keychain child spawn is) |
| Scalability | 0.80 | account counts are tens; single JSON registry by compatibility design |
| Reliability | 0.65 | credential-path parity rests on three spikes (httptrace, jsontext, Darwin proc) and on reproducing 2,046 lines of peer-lock protocol; mitigated by the Rust test corpus (~409 e2e + sibling unit tests) |
| Cost effectiveness | 0.75 | nine waves on astra lanes; fixtures/schemas/snapshots reuse removes most oracle authoring |

## 19. Judgement calls settled during planning (2026-10-05)

| Question | Options offered | Answer |
|---|---|---|
| Table rendering | lipgloss/v2/table vs `text/tabwriter` | lipgloss/v2/table |
| Worker model for `codex-explore`/`codex-writer` | rewrite to astra vs accept luna-fast vs do not use | accept luna-fast |
| Security review routing | astra `codex-security-reviewer` vs global opus rule | astra |
| Planning-phase commits | tracked copy under `docs/` vs `git add -f .omc` vs none | tracked copy under `docs/plans`, `docs/research` |
| Darwin process observation | purego+libproc vs `sysctl KERN_PROC` vs cgo | `sysctl KERN_PROC` |
| Secret memory | own `Secret` type with wipe vs `negrel/secrecy` vs memguard | memguard Enclave + momentary LockedBuffer |
| `AGENTCTL_LOG` grammar | slog levels vs full `RUST_LOG` | slog levels only |
| Test tooling | teatest + own harness vs teatest + testscript vs no teatest | teatest/v2 + testscript |
| 73-column commit subject | amend + force-with-lease vs leave | amended |
| Plan approval (execution session) | approve via team vs ralph vs critic first vs changes | approve via team |
| Worker model after the 15:30 frontmatter change | restore `astra-ultrafast` vs keep `astra-fast` | keep `astra-fast` as the files say |
| Codex trailer on lane commits | add `Co-Authored-By: Codex` vs Fable + session only | add it |
| W2 start | run the spikes now alongside W1 (eight lanes) vs wait for the W1 gate | run now; the W1 gate still bounds W3+ |
| Two lanes self-identifying as Fable | continue with the frontmatter as authoritative vs investigate the gateway routing first | continue; report any lane that names Fable |
| Golden file names (lane question, answered by the naming rule) | keep `agctl__` prefix vs strip it vs re-slug | strip the crate prefix only; `testdata/golden/MAPPING.md` records the correspondence |
| W3 start | start the four read-path lanes while `fix-w1` runs vs wait for the W1 close and the last spike | start now; W1 close bounds W4 |
| Early start of W3-independent lanes after the W1 close | `claude-file-store` (W5) + `claude-peer-locks` (W6) + `runtime-core` (the `internal/runtime` row of section 4, which had no lane) now vs only the first and third vs wait for W3 | all three now (seven lanes); W4 waits for W3 |
| Signal exit when the command ignores cancellation past the 10 s deferral | force exit 128+n without `Purge()` (reference behaviour, warning logged) vs wait for the command vs a join barrier inside `secret` | force exit without `Purge()`; the cooperative path keeps the single deferred `Purge()`; cleanup callbacks run FIFO as in the reference |
| memguard under a finite `RLIMIT_MEMLOCK` (v0.23.0 deadlocks in purge after a key-view allocation failure; macOS default is unlimited) | conditional GO with a startup budget check that fails fast vs NO-GO and an own wiped `Secret` type vs conditional GO with a non-mlock fallback | conditional GO: `secret.EnsureLockedMemoryBudget()` refuses with exit 1 below the measured threshold; follow-up recorded in the spike document; the check and `Purge()` are wired into `main.go` by the foundation fix lane |
| Vendor usage number tokens in `codex status --raw` (W8 gate observation) | re-emit the vendor's bytes verbatim vs canonicalise through f64 as the reference's serde_json does | verbatim: lossless for long decimals and large integers; byte-equal on canonical bodies; recorded divergence on non-canonical spellings |
| Lock-order parity for `lock_tests.rs` (W8) | document a thread-local-only divergence vs wire the existing `internal/runtime/lockorder` witness | wire it: `Lock.Context()` carries ownership, `config.UpdateRegistry` checks before blocking; the thread-affinity case is deferred |
| Refresh marker reader on malformed digests (W8) | accept any string as the serde reader does vs reject non-8-hex in the reader | reject in the reader (unavailable, member named, value never shown); doctor still validates what it prints |
| Redirects on authenticated Claude requests (P7 security review) | follow with header stripping as the reference's ureq 3.4.1 default does vs refuse every redirect | refuse (`http.ErrUseLastResponse`) on the token POSTs and the bearer GETs: Go's net/http keeps `Authorization` for the same hostname on another port or scheme, and a replayable POST body would re-send the grant; the original 3xx is classified as the existing HTTP failure |
| Release tooling must not pass vacuously (W9 gate) | trust `set -euo pipefail` vs require the terminal summary | both scripts carry an EXIT trap that turns any exit without the final summary into exit 1; parity rejects an artifact whose `go version -m` carries `-tags=` |

Lead decisions (not asked, recorded for review): Rust implementation normative over its
comments; testing endpoints fail closed for both providers; `time` with the system zone
database; `gofrs/flock`, `go-runewidth`, `x/net/http2` not used.

## 20. ADR

- **Decision**: faithful module-by-module port (Option A), Go-native replacements for
  crate-carried behaviour proven by spikes first, Rust implementation normative over its
  comments.
- **Drivers**: zero data loss on live credentials; mechanical parity proof; small parallel
  waves.
- **Alternatives considered**: provider-generic redesign (B); wrapper over the Rust binary (C).
- **Why chosen**: oracles map to the Rust structure; redesign risk would sit on the
  credential path; a wrapper is not a port.
- **Consequences**: Rust-shaped interfaces survive (typestates as unexported
  constructors); explicit lifecycle tokens replace `Drop`; later refactor toward a generic
  core is possible once e2e goldens exist.
- **Follow-ups**: P8 remote-control restart; P9 Linux; intentional corrections of the
  comment/implementation mismatches; revisit the generic core after P7.

## 21. Changelog

- 2026-10-05 14:53:47 JST: initial skeleton from handoff decisions and user answers.
- 2026-10-05 (after `34585b7`): merged the three inventory reports (exit codes, env vars,
  invariants, budgets, seams, oracles, rendering, Linux/RC status); added section 19.
- 2026-10-05 (after the module research commit): added section 4.1 (module versions),
  rewrote the `~/.claude.json` step as a span splice, Darwin proc via `sysctl`, memguard
  secret model and spike, testscript/teatest test plan, `AGENTCTL_LOG` grammar; section 19
  now records the answers instead of the questions.
- 2026-10-05 (after `3a35bd7`): fake executables are the verbatim fixture scripts, Go
  registers only helper commands; sysctl field map spelled out (real UID via
  `Eproc.Pcred.P_ruid`, 16-byte `P_comm` limit); remaining crate mappings listed in 4.1.
- 2026-10-05 16:50:36 JST: plan approved by the user (team); P0 closed, P1/W1 started;
  worker model kept at the frontmatter's `astra-fast`; Codex trailer added to lane
  commits; package-layout facts for embed, build tags and `TestMain` recorded in 17.
- 2026-10-05 17:37:20 JST: W2 spikes started alongside W1 on the user's instruction to maximise
  parallelism (eight lanes); one verdict file per spike instead of a shared one.
- 2026-10-05 17:53:23 JST: foundation lanes finished and shut down; verify/review lanes started;
  shared-index commit rule and the Fable self-identity note added to section 17.
- 2026-10-05 17:54:11 JST: Darwin process-observation spike returned GO; recorded in the wave table.
- 2026-10-05 18:07:42 JST: config-rewrite and secret-memory spikes returned GO; recorded in the wave table.
- 2026-10-05 18:15:41 JST: memguard spike downgraded to a conditional GO (startup memlock budget check);
  decision recorded in section 19.
- 2026-10-05 18:20:45 JST: foundation verification and review results recorded; fix lane started.
- 2026-10-05 18:37:57 JST: read-path lanes started while the foundation fix lane runs (seven lanes).
- 2026-10-05 18:39:21 JST: all four spikes GO (memguard conditional); P2 closed. The refresh-transport
  files were committed by the lead because the lane refused to commit on main.
- 2026-10-05 18:49:33 JST: foundation fix lane finished (ten commits); re-verification lane started.
- 2026-10-05 19:32:04 JST: W1 closed by re-verification; P1 done. File-store, peer-lock and runtime lanes
  started early on the user's decision (seven lanes with the read-path four).
- 2026-10-05 19:36:34 JST: read-path lanes finished; verification and review lanes started; the status
  wave started with a token-refresh lane split out of login.
- 2026-10-05 19:39:01 JST: signal-exit decision recorded (forced exit skips Purge after the deferral).
- 2026-10-05 19:44:32 JST: read-path verification (one blocker, to the status lane) and review (approve,
  two moves to a fix lane) recorded.
- 2026-10-05 21:01:02 JST: session restart requested by the user during W4; in-flight files and the
  resume order recorded in docs/handoffs/agctl-go-port-session2-handoff.md.
- 2026-10-05 21:22:15 JST: session 3 resumed after the restart; the in-flight commits landed by the lead
  (cleanup FIFO `9e288a8`, forced signal exit + handler wiring + smoke exit 2 `4efe497`, audit
  required members `eaf44b8`, status printer `25c36f5`); three resume lanes running; the
  peer-release ordering decision recorded in the W6 row.
- 2026-10-05 21:48:51 JST: every W4 lane landed (status `aa87d5e` `ce31a3a`, accounts-read `a0c501f`
  `8370554`); the early peer-lock lane landed acquisition `10dab64`, config lock `6200fd0` and the
  audit creation fix `895ece3`, lint clean repo-wide; verification and review lanes started on the
  `895ece3` snapshot. Join semantics settled as equal to the reference.
- 2026-10-05 22:21:23 JST: W4 gate verdict recorded (FAIL / REQUEST_CHANGES, refresh integration the
  main blocker); fix round started as two lanes and W5 started in parallel as three lanes on the
  user's instruction; handler composition moved to a per-family registry; ten round-2 commits landed.
- 2026-10-05 22:35:28 JST: watch, import/accounts-write and the secret fix lane finished (all commits
  listed in the W4/W5 rows); testscript commands register per family (`0d1b712`); login has one
  script commit left; the status fix lane owes the migrated/pending paths and the nine e2e cases.
- 2026-10-06 00:23:38 JST: the status fix lane finished (migrated refresh `e6e9854`, containment `f46b155`,
  flock seam `9108486`, nine status e2e cases + `e2e_refresh.txtar` `9ea9c2d`) and login's
  `e2e_login.txtar` landed (`3b9b525`); every W4 finding has a closing commit; `verify-w5` and
  `review-w5` started on the `9ea9c2d` snapshot covering the W4 fix round and all of W5.
- 2026-10-06 00:56:54 JST: the W4-fix + W5 gate returned REQUEST_CHANGES with every runtime gate green; the four
  findings landed (`7b8201b` `5d3cc81` `6e3fac3` `cfb218c`) and `verify-w5b` re-verifies the fixes on
  `cfb218c`. Three commit messages were reworded to drop plan identifiers and the history re-signed
  (`0c2eac9` became `132500c`); fixture, schema and script comments say the reason in words (`ea59d67`).
  W6 started as two lanes (`claude-isolate-export`, `claude-doctor`).
- 2026-10-06 01:13:09 JST: W4 and W5 closed: the re-verification of the fix round passed every gate and its one
  remaining item, a real login-child cancellation test, landed as `8a4d99a`. W6 first commits landed
  (session seed and canonical config rendering `edd3e98`, doctor report `f61facd`).
- 2026-10-06 01:29:48 JST: W6 landed as ten commits from two lanes (isolate/export `edd3e98` `a00da26` `045c1dc`
  `ca10dd7` `64873ef` `6952073`; doctor `f61facd` `2235fb9` `aa8f403` `c636314`); `verify-w6` and
  `review-w6` started on the `6952073` snapshot.
- 2026-10-06 01:56:45 JST: W6 closed after the doctor heartbeat-baseline fix `ac7b86f` passed re-verification; W7
  started as three lanes and landed its first commits (`8662f57` `6a2586f` `2644303` `22c8179`
  `319f164` `a65db1b` `f79fc1c` `f5b1a5b`); workers now run on the sol-fast model by the user's instruction.
- 2026-10-06 08:02:02 JST: W7 closed. Swap core, adoption, `.claude.json` rewrite, the shared forward/undo engine, undo, forget and the complete `e2e_swap.rs` CLI mapping landed as 30 commits (last `52bcd50`); the review on `59da7da` found an unknown-org audit-identity defect (`5f37e6c`) and the missing testing-only swap-deadline override (`4122a9b`), the inherited doctor identity-oracle follow-up closed in `82089e3`; verification on `be43c6c` PASSED every section 12 gate on both tags: wire parity 20/20 byte-equal against the reference binary, 129/129 reference headings, static gates and hygiene clean, the release race suite and the tagged race suite twice each (the tagged suite needs `-timeout=30m` because `e2e_swap` takes about 410 s per repetition under `-race`; 6010 pass events, 0 failures). Commit trailers drop the session link from this point on. Remote control stays deferred.
- 2026-10-06 10:27:53 JST: W8 closed. Codex provider and command layers landed as 23 commits from four lanes plus one fix lane (last `f784164`); the gate on `f784164` found three wire discrepancies and one modernize diagnostic, the review a quadratic duplicate lookup; fixed in `fbf4a1e` `9665fc6` `6ed667b` `708f91f` `08dea5f`; `review-w8b` APPROVE; `verify-w8b` on `08dea5f` W8 PASS: both-tag race suites `-count=2` (7,140 + 7,614 pass events, 0 failures), 19 root scripts twice, wire parity 20/20 against the reference binary, schemas and the three goldens valid, 106/106 reference tests mapped, zero sentinel leaks over 1,374 fixture files and 476 sinks. W9 started as three lanes.
- 2026-10-06 11:18:40 JST: P7 closed on `308371e`. W9 release lanes landed `64894e9` `dd9dc40` `b059a4d` `b371ce9` `b650179`; gate and security fixes `289df4e` `e71769f` `0f1dbb8` `934c6b4` `e43ea9c` `308371e` `c203eef`; `security-review-p7b` APPROVE, `verify-w9b` VERIFIED except the fixture gap fixed in `c203eef`; final gate PASS: both-tag build/vet, race suites -count=1 -timeout=30m on a git archive (24 + 25 package ok lines incl. the root scripts at 410 s, 0 failures, no race reports), lint 0 issues on both tags. Full release parity sign-off stays INCOMPLETE pending the manual keychain run (user decision).
- 2026-10-06 15:42:51 JST: W10 parity-vm scripts stage closed on `c22dca6` (`f46e531` scripts, `ac27460` `c22dca6` fixes; gates APPROVE/APPROVE/VERIFIED). Rename `e2e_test.go` -> `main_test.go` landed as `ed878de`. History rewritten earlier the same day (old `e0b90a1` = `b2550d9`).
- 2026-10-07 04:46:39 JST: W11 hygiene wave landed through `0bc5885` (nine commits, each gated by fresh verifier/reviewer lanes, security review on the scripts and CI changes). Open: fault source-contract test, blocked on worker routing.
- 2026-10-07 11:33:25 JST: W11 closed on `fc16949`. The fault source-contract test landed as `a3be959` after seven fresh verifier/reviewer rounds
  (five fix lanes); the resolver's supported-shape inventory is documented at `expressionType` and two INFO shapes (comma-ok
  declarations, cross-file unqualified type method expressions) plus the `go/types` migration are recorded as follow-ups.
- 2026-10-07 11:50:46 JST: the hosted macOS Lint step failed on `a3be959` (staticcheck SA1019 on one `map[*ast.Object]int`; golangci-lint
  had not been in the gate chain); `ed58fd7` adds the reasoned nolint directive and run 37562905961 is green on macos (Lint, release
  gate and Test); the linux job's Test step stays red as before W11 (P9 input).
- 2026-10-09 21:18:34 JST: W12 closed on `280504e`. The fault source-contract resolver moved from go/ast parser objects to go/types
  (four gate rounds on fresh verifier/reviewer lanes; fix lanes closed a tuple-expanded method expression, module root under
  `-trimpath`, per-variant `types.Info` with a validated module root and exact package filter, and `//line`-proof physical
  positions with cwd-only root discovery); 199 table cases per mode plus the real source contract; `x/tools` v0.51.0 and
  `x/mod` v0.41.0 become direct. Local toolchain drift found and worked around, not changed in the repository: the exec
  teardown test and the release gate pin `GOTOOLCHAIN=go1.27.1` by name.
- 2026-10-09 21:19:08 JST: the remote-control restart design is recorded in the P8 row from the isolated credential-swap spike of 2026-10-07:
  no disconnect step (Claude Code drops the bridge itself on the account change while requests keep succeeding), reconnection
  through a Claude Code mod with file-polling transport, tmux only as the fallback for sessions without the mod, mods pinned at
  Claude Code 2.1.287 or newer. Whether claude.ai/code shows the pre-swap conversation under the new account stays unverified.
- 2026-10-09 21:48:41 JST: every by-name `GOTOOLCHAIN=go1.27.1` pin (README, release gate, parity scripts, exec teardown test) moved to
  go1.27.2 as `909acd2` after a native go1.27.2 gate (shellcheck, actionlint, vet, golangci-lint v2.14.0, build, release gate,
  repo-wide `-race` in both modes). Hosted run 37929142512/37929134629 on `280504e`: Vet, Lint and the release gate green on
  macos; the macos Test step failed twice on the tagged suite's codex login child-reap case (`login_child_process_test.go:190`,
  the subtest already seen flaking on `8613a14`), the user chose to keep observing; linux Test red as before (P9 input). W10: the
  parity VM base `agentctl-e2e-base` is prepared with the go1.27.2 artifact in work directory
  `.omc/artifacts/parity-vm/2026-10-09_21-25-32-operator`; the operator GUI login and snapshot are next. The spike's keychain
  item was deleted on the user's decision.
- 2026-10-09 23:28:29 JST: the W10 operator keychain run completed for the Go artifact only (the user chose not to debug the reference: its default
  executable is a `testing` build whose keychain reader is disabled). Facts folded into the parity scripts: a forward swap onto the
  single account's newer grant is `applied` and discards the displaced grant in both implementations, so the undo refuses; the final
  Codex status is the partial exit 2 because the guest never logs the vendor CLI in. Record in `docs/research/agctl-parity.md`
  (`4c40212`, `3d7b04a`), verified by verify-w10e2; full parity sign-off stays incomplete on the reference axis.
- 2026-10-10 00:02:52 JST: a security review of everything after `13c9b46` (run on the user's instruction on a Grok reviewer; the first two
  attempts fell back to another model until the agent definition named the gateway id and a five-tool allowlist kept it under
  the gateway's 350-tool limit) found one MEDIUM: the fault guard's module-root walk started from the unresolved working
  directory, so a decoy module reached through a symlink could capture the scan. Fixed as `1b37ffa` (symlinks resolved before
  the walk, three root regressions; 202 table cases per mode), verified by verify-w12e and the security re-review (APPROVE WITH
  FINDINGS); the guest parity script's refusal match was anchored to the three complete messages (`71a161c`, `57b3684`).
- 2026-10-11 00:09:22 JST: the remote-control restart landed on the mod design as nine commits (`af1094a` refusal model, `836ee7f` provider rules,
  `deaedc6` commands wiring with the `AGENTCTL_REMOTE_CONTROL_TIME_SCALE` testing seam, `cfa2602` the mod and the root
  marketplace, `67fa790` the contract record, `c148690` README, `05198bd` the scripted-mod txtar arms, `99c9c2b` and `8de1d8c`
  the first fix round). Decisions made in execution: the swap is refused on a bridged session the mod cannot answer; the poll
  survives `/clear`; a rejection written before the body is readable carries an empty action; entries leave the mod's table only
  once expired; agentctl accepts only complete answers. Round-1 gate reports under `.omc/handoffs/w13/`. Open: the operator run
  on Claude Code 2.1.296 (procedure in the contract record), the claude.ai history check, the tmux fallback (deferred by decision).
