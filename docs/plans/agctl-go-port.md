# Plan: port agctl (Rust) to agentctl (Go)

- Status: **approved 2026-10-05 (user, "approve via team"); P1 in progress**
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
| P1 | Foundation: module skeleton, `internal/cli` tree + duration grammar + completions, `internal/errs`, `internal/config` (paths, registry, locks), `internal/testutil`, fixtures/schemas/goldens copied, CI | 🔶 in progress (W1 started 2026-10-05) | — |
| P2 | Spikes with go/no-go gates: refresh-POST outcome classification on `net/http`+`httptrace`; byte-exact `~/.claude.json` edit by span splicing; Darwin process observation via `sysctl KERN_PROC`; memguard lifecycle under the signal handler and mlock limits | 🔶 in progress (W2 started 2026-10-05, in parallel with W1) | — |
| P3 | Claude read path: `security(1)` reader, credentials, namespace, discovery, usage + cache, render tables/reset/JSON v1, `status`, `accounts list/show` | 🔜 | — |
| P4 | Claude `watch` (Bubble Tea v2), `login` (PKCE + loopback), `import --from keychain`, `accounts remove/relocate/forget/unforget`, file store + pending replay | 🔜 | — |
| P5 | Claude `use` (isolate), `exec`, `env`, `doctor` (incl. `--remove-stale`), then `use --live` / `--undo` / `--forget` (peer locks, swap, adopt, audit, `~/.claude.json`) | 🔜 | — |
| P6 | Codex: home/config.toml, auth_store, credentials, claims, `status/watch/accounts/import/doctor`, login child, refresh state machine + resend policy | 🔜 | — |
| P7 | Release gates: seam-absence proof, completions for four shells, README, parity sign-off against the Rust binary on one store | 🔜 | — |
| P8 (later) | `--restart-remote-control`: tmux transport, RC attestation, fake-tmux + 10 screen fixtures, version pin policy | 🔜 not scheduled | — |
| P9 (later) | Linux: process backend (procfs), platform refusals (keychain unsupported, `--remove-stale` exit 1), CI on `ubuntu-26.04`; Phase 2 credentials stay NO-GO until re-decided | 🔜 not scheduled | — |

Current point: **P1 / W1 and P2 / W2 running together (eight lanes); next boundary is the W1 exit gate, then the four spike verdicts.**

### Wave table (P1–P7)

| Wave | Lanes (parallel) | Depends on | Exit gate |
|---|---|---|---|
| W1 | `foundation-cli` (cobra tree, flags, duration grammar, exit constants, completions), `foundation-errs-config` (errs, paths, registry, modes, `.config.lock`, namespace lock), `foundation-testutil` (fixtures/schemas/goldens copy + header-strip script, temp tree, fake-bin install, golden/schema helpers), `foundation-ci` (`.github/workflows/ci.yaml`) | — | section 12 gates green; `agentctl --help` and `completions` goldens match `tests/cli_smoke.rs` modulo binary name. **Lanes done 2026-10-05** (cli `1992cca` `c1e0c04` `bd8b9de` `cd81e64` `06fe00f` `e870694`; errs-config `670e226` `0e67fe1` `e75b7ff` `f88a166`; testutil `727ca91` `d2ba9ee` `19dcadc` `4b2393d` `53b62a5` `a9275e5`, helpers rode in `cd81e64`; ci `6aa7acb` `a9aa959`); `verify-w1` and `review-w1` lanes running |
| W2 | `spike-httptrace`, `spike-jsontext`, `spike-darwin-proc`, `spike-memguard` | W1 for the gate; started in parallel with W1 on 2026-10-05 by the user's decision (eight lanes at once) | table tests green; one verdict file per spike, `docs/research/agctl-spike-{httptrace,jsontext,darwin-proc,memguard}.md`; no-go stops the dependent waves and goes back to the user |
| W3 | `claude-secret-read` (security_cli, location, reader), `claude-credentials-namespace` (credentials, namespace NFC+SHA-256, claims), `claude-usage` (usage HTTP + model + cache), `render-table` (table, reset, row) | W1 | unit + golden tests; `fake-security.sh` argv log equals Rust e2e expectations |
| W4 | `claude-status` (discovery, status pass, JSON v1, partial exit), `claude-accounts-read` | W3 | e2e goldens (normalised + exact-byte); `status.v1.json` validation |
| W5 | `claude-watch` (tui + watch loop + log buffer), `claude-login` (PKCE, loopback, browser opener), `claude-file-store` (secret_file, file_store, pending, audit), `claude-import-accounts-write` | W4 | e2e + frame-dump goldens; `tests/e2e_login.rs`, `e2e_import.rs`, `e2e_accounts.rs` parity |
| W6 | `claude-isolate-export` (use/exec/env, session seed), `claude-doctor` (incl. `--remove-stale`, held-lock records), `claude-peer-locks` (claude_lock, config_lock, foreign_activity) | W5, W2 | exit-code table (0/1/2 + 10–30 where reachable); `e2e_isolate.rs`, `e2e_doctor*.rs` parity |
| W7 | `claude-use-live` (swap, adopt, claude_json rewrite, undo, catch-up) | W6 | `e2e_swap.rs` parity (129 tests) minus RC; byte-exact round trip; refusal matrix |
| W8 | `codex-home-auth` (home, config.toml, auth_store, credentials, claims, proof/permit/lock), `codex-status-accounts` (pass, status, JSON v2, accounts, watch adapter), `codex-refresh` (oauth classification from W2, refresh state machine, accounts_refresh), `codex-login-import-doctor` (login child, import, doctor table + JSON) | W4, W2 | `e2e_codex_*.rs` parity; `status.v2.json`, `codex-doctor.v1.json` validation |
| W9 | `release-gate` (`scripts/release-gate.sh`), `docs` (README, completions install lines), `parity-signoff` (both binaries on one store), review lanes (`code-reviewer`, `security-reviewer`, `verifier`) | all | gate script green; parity report; reviews accepted |

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
