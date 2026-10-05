# agctl inventory: modules, crate map, CLI wiring

- Captured: 2026-10-05 15:00:00 JST (`date` in the command that assembled this file)
- Lane: read-only `analyst` worker (`~/.claude/agents/codex-analyst.md`), Rust source `/Users/zchee/rust/src/github.com/zchee/agctl` at branch `rc-restart`, commit `dbf6aeab84dfbe2465de496316d7bf6780940a60`
- Verbatim worker report, delivered by SendMessage and recovered from the worker transcript; nothing in it was executed by the lead.

---

Model: `claude-gpt-6-astra-ultrafast[1m]` is the runtime-reported model; I cannot independently identify the model behind the provider alias. This is not a verified Opus self-identification.

## Rust inventory — part (a)

Source: `/Users/zchee/rust/src/github.com/zchee/agctl`, clean branch `rc-restart`, commit `dbf6aeab84dfbe2465de496316d7bf6780940a60` (observed with `git rev-parse HEAD` and `git status --short --branch`). Read `AGENTS.md`, `Cargo.toml`, and every included file's header/main types/imports. No writes, builds, tests, live credentials, or network calls.

Method: `fd -e rs . src --exclude '*_tests.rs' --exclude 'snapshots'` and `wc -l`; import/attribute/header scans checked against source. LOC means physical newline count, including comments and any test-only helpers inside these files, NOT executable production LOC.

Table notation: paths are relative to source root. A responsibility cites the file's header at `:1` unless another line is shown. Import `crate@N` means an actual `use crate::...` at that file's line N; `—` means no external `use` statement, NOT no fully-qualified dependency calls. `T` is **exact positive `#[cfg(feature = "testing")]` count / all cfg attributes mentioning that feature**, including negative/compound forms; `@N` is the first such attribute. OS column counts only local target_os conditions; parent-gated modules are identified below. This avoids calling test helpers production code merely because their filename is not `_tests.rs`.

### Root
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/cli.rs:1 | 878 | CLI arguments, duration parser, swap exit constants | clap@24; thiserror@29 | — | 0/0 |
| src/error.rs:1 | 175 | Process error vocabulary and exit mapping | thiserror@31 | — | 0/0 |
| src/main.rs:1 | 160 | Parse, logging, signals, dispatch, process exit | clap@24; tracing_subscriber@25 | — | 0/0 |
| **Root subtotal (3)** | **1,213** | | | | |

### Commands
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/commands/accounts.rs:1 | 1063 | Claude account inspection, hiding, removal, relocation | tabled@41 | — | 1/2@1014 |
| src/commands/completions.rs:1 | 57 | Generate completions from the CLI definition | clap@8 | — | 0/0 |
| src/commands/doctor.rs:1 | 1686 | Claude store diagnosis and fenced stale-lock cleanup | serde_json@68 | — | 0/0 |
| src/commands/export.rs:1 | 408 | Session environment output and child execution | — | — | 0/0 |
| src/commands/import.rs:1 | 162 | Execute Claude import plan and registry update | — | — | 0/0 |
| src/commands/isolate.rs:1 | 864 | Create/reuse/remove isolated session directories | — | — | 0/0 |
| src/commands/login.rs:1 | 626 | Claude PKCE login and owned credential installation | — | — | 3/4@147 |
| src/commands/mod.rs:1 | 216 | Command modules, prompting and terminal attestation | rustix@118 | — | 0/0 |
| src/commands/status.rs:1 | 2189 | Claude discovery, bounded usage pass, refresh and report | jiff@50; serde_json@52; tracing@54 | — | 1/2@424 |
| src/commands/use.rs:1 | 4587 | Isolated launch, live swap, undo and session removal | — | — | 2/4@671 |
| src/commands/watch.rs:1 | 557 | Shared asynchronous watch loop and Claude adapter | crossterm@55; jiff@61; ratatui@62 | — | 0/0 |
| **Commands direct subtotal (11)** | **12,415** | | | | |

### Codex commands
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/commands/codex/accounts.rs:1 | 437 | Codex account operations; delegate refresh-policy actions | — | — | 0/0 |
| src/commands/codex/accounts_refresh.rs:1 | 569 | Change refresh policy; explicit resend/floor reset | jiff@54 | — | 0/0 |
| src/commands/codex/doctor.rs:1 | 834 | Inspect Codex home, namespaces, locks, markers and audit | jiff@48 | — | 0/0 |
| src/commands/codex/import.rs:1 | 325 | Read foreign Codex home and record metadata | — | — | 0/0 |
| src/commands/codex/login.rs:1 | 480 | Coordinate scratch vendor login and verified installation | — | — | 0/0 |
| src/commands/codex/mod.rs:55 | 86 | Codex dispatch and process-environment constructor | — | — | 0/0 |
| src/commands/codex/pass.rs:1 | 1121 | Shared read-only Codex discovery/usage workers | jiff@45; serde_json@46 | — | 0/0 |
| src/commands/codex/status.rs:1 | 380 | Codex status pass, allowed refresh orchestration, output | jiff@55; serde_json@57 | — | 0/0 |
| src/commands/codex/watch.rs:1 | 170 | Codex adapter to shared watch loop; never refresh | — | — | 0/0 |
| **Codex commands subtotal (9)** | **4,402** | | | | |

### Configuration
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/config/codex.rs:1 | 109 | Codex registry records, account kinds, refresh policy | serde@34 | — | 0/0 |
| src/config/import.rs:1 | 468 | Pure Claude import decisions and registry plan | — | — | 0/0 |
| src/config/mod.rs:1 | 470 | Versioned shared registry and serialized updates | serde@50 | — | 1/1@303 |
| src/config/paths.rs:1 | 534 | Resolve store and derive authorized state/cache paths | etcetera@60 | — | 0/0 |
| **Configuration subtotal (4)** | **1,581** | | | | |

### Provider shared
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/provider/mod.rs:1 | 259 | Provider identity, usage/auth traits and fetch errors | — | — | 0/0 |
| **Provider direct subtotal (1)** | **259** | | | | |

### Claude provider
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/provider/claude/account.rs:1 | 329 | Claude row state, visibility and degradation vocabulary | — | — | 0/0 |
| src/provider/claude/adopt.rs:1 | 434 | Pure decision to preserve displaced swap credentials | — | — | 0/0 |
| src/provider/claude/claude_json.rs:1 | 1070 | Peer-locked oauthAccount update and cache cleanup | rustix@63; serde_json@68; sha2@70 | — | 0/0 |
| src/provider/claude/credentials.rs:1 | 596 | Credential parsing, lossless merge, digests, keychain line | secrecy@46; serde@48; serde_json@50; sha2@52 | — | 0/0 |
| src/provider/claude/discovery.rs:1 | 705 | Combine registry and keychain discovery into Claude rows | — | — | 0/0 |
| src/provider/claude/live_sessions.rs:1 | 452 | Read bounded session registry observations for RC | serde@13 | — | 0/0 |
| src/provider/claude/mod.rs:1 | 58 | Claude provider module/export boundary and user agent | — | — | 0/0 |
| src/provider/claude/namespace.rs:1 | 335 | Exact Claude namespace/keychain service naming | sha2@57; unicode_normalization@59 | — | 0/0 |
| src/provider/claude/oauth.rs:1 | 1327 | PKCE, authorize/callback, token grants, profile client | base64@63; rand@65; secrecy@66; serde@67; serde_json@68; sha2@70; url@72 | — | 6/9@138 |
| src/provider/claude/remote_control.rs:1 | 866 | Operator-attested Remote Control disconnect/reconnect | serde@10 | — | 1/2@213 |
| src/provider/claude/swap.rs:1 | 566 | Pure swap phase/refusal/outcome vocabulary | — | — | 0/0 |
| src/provider/claude/usage.rs:1 | 762 | Claude usage HTTP, parsing and refresh/profile seams | jiff@63; serde_json@64 | — | 2/3@122 |
| **Claude provider subtotal (12)** | **7,500** | | | | |

### Codex provider
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/provider/codex/account.rs:1 | 474 | Codex states, credits and presentation row | jiff@17 | — | 0/0 |
| src/provider/codex/audit.rs:1 | 529 | Consume write receipts and append Codex event log | jiff@28; serde@29 | — | 0/0 |
| src/provider/codex/auth_store.rs:1 | 1765 | Sole auth.json I/O boundary; writes and refresh markers | jiff@67; rustix@68; secrecy@70; serde@71 | — | 15/15@318 |
| src/provider/codex/claims.rs:1 | 157 | Decode allowlisted ID claims and access-token expiry | base64@33; serde_json@35 | — | 0/0 |
| src/provider/codex/credentials.rs:1 | 934 | Ordered auth document, validation and refresh merge | jiff@49; secrecy@51; serde_json@53; sha2@55 | — | 0/0 |
| src/provider/codex/discovery.rs:1 | 228 | Plan Codex sources and identify orphan namespaces | — | — | 0/0 |
| src/provider/codex/home.rs:1 | 603 | Resolve Codex home, storage mode and daemon evidence | jiff@38; rustix@40; serde@42; sha2@43 | — | 0/0 |
| src/provider/codex/lock.rs:1 | 111 | Acquire Codex namespace lock and produce guard proof | — | — | 0/0 |
| src/provider/codex/login_child.rs:1 | 956 | Bounded vendor login child with scratch home/allowlist | rustix@42 | — | 4/5@69 |
| src/provider/codex/mod.rs:1 | 103 | Codex modules, provider adapter, user-agent constant | — | — | 0/0 |
| src/provider/codex/oauth.rs:1 | 434 | Single refresh POST and transport-outcome classification | jiff@64; secrecy@65; serde_json@66; ureq@399 | — | 3/4@85 |
| src/provider/codex/permit.rs:1 | 61 | Unforgeable-at-module-boundary refresh-send permission | — | — | 0/0 |
| src/provider/codex/proof.rs:1 | 383 | Ownership, login, guard and post-child proof types | — | — | 2/3@171 |
| src/provider/codex/refresh.rs:1 | 1146 | Locked, durable-marker refresh/recovery state machine | jiff@59 | — | 0/0 |
| src/provider/codex/usage.rs:1 | 763 | Codex usage HTTP and normalized limits/credits parsing | jiff@68; serde_json@69 | — | 3/4@139 |
| **Codex provider subtotal (15)** | **8,647** | | | | |

### Rendering
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/render/codex_doctor.rs:1 | 550 | Codex doctor table and versioned JSON document | serde@24 | — | 0/0 |
| src/render/json.rs:1 | 554 | Claude v1 JSON status and doctor/isolation documents | jiff@30; serde@31; serde_json@32 | — | 0/0 |
| src/render/json_v2.rs:1 | 370 | Provider-neutral v2 JSON status representation | jiff@38; serde@39; serde_json@40 | — | 0/0 |
| src/render/mod.rs:1 | 145 | Presentation-only Claude status row/report boundary | jiff@22 | — | 0/0 |
| src/render/reset.rs:1 | 202 | Local-zone reset timestamp/countdown formatting | jiff@47 | — | 0/0 |
| src/render/row.rs:1 | 202 | Provider-neutral TUI row/source traits | jiff@28 | — | 0/0 |
| src/render/table.rs:1 | 531 | Claude/Codex status tables and layouts | jiff@54; tabled@56 | — | 0/0 |
| **Rendering subtotal (7)** | **2,554** | | | | |

### Runtime
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/runtime/cleanup.rs:1 | 338 | Emergency temporary-file, terminal and child registry | — | — | 0/0 |
| src/runtime/coordinator.rs:1 | 564 | Bounded pass workers, cancellation, child ownership | — | — | 0/0 |
| src/runtime/fault.rs:1 | 219 | Build-gated deterministic fault/pause injection | — | — | 3/10@61 |
| src/runtime/lock_order.rs:1 | 98 | Testing-only thread-local Codex/config lock-order witness | — | — | 0/0 |
| src/runtime/log_writer.rs:1 | 185 | Buffer stderr logs while TUI owns terminal | tracing_subscriber@50 | — | 0/0 |
| src/runtime/mod.rs:1 | 21 | Runtime modules and feature-gated lock-order witness | — | — | 1/1@15 |
| src/runtime/proc.rs:1 | 244 | OS-selected process/holder observations and capabilities | rustix@240 (test-only) | linux/macOS/other refusal@11-23; macOS cfg!@31 | 0/0 |
| src/runtime/signals.rs:1 | 249 | TERM/HUP/INT cancellation, child teardown and exit | rustix@61; signal_hook@63 | — | 0/0 |
| src/runtime/tmux.rs:1 | 436 | Bounded tmux commands, captures and pane checks | — | — | 4/4@25 |
| src/runtime/tty.rs:1 | 68 | Bounded readiness on own terminal | rustix@8 | — | 0/0 |
| **Runtime direct subtotal (10)** | **2,422** | | | | |

### OS process backends
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/runtime/proc/classification.rs:1 | 23 | Pure process-name classification | — | — (parent-gated) | 0/0 |
| src/runtime/proc/linux.rs:1 | 380 | Bounded procfs observation/validation | procfs_core@16; rustix@20 | — (parent Linux) | 0/0 |
| src/runtime/proc/macos.rs:1 | 481 | Darwin process ABI observation and identity | rustix@49 | — (parent macOS) | 0/0 |
| **OS backend subtotal (3)** | **884** | | | | |

### Secret/storage
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/secret/audit.rs:1 | 1201 | Append-only swap/write/lock audit and recovery history | jiff@57; rustix@58; serde@63 | — | 0/0 |
| src/secret/backend.rs:1 | 33 | Fixed platform capability/refusal decisions | — | macOS/Linux@26-30; macOS cfg!@8,14 | 0/0 |
| src/secret/claude_lock.rs:1 | 2046 | Peer Claude directory-lock protocol and holder evidence | — | — | 0/0 |
| src/secret/config_lock.rs:1 | 449 | Peer .claude.json lock with heartbeat/retry rules | rustix@52 | — | 0/0 |
| src/secret/fake_reader.rs:1 | 135 | Test-only scripted KeychainReader | — | — | 0/0 |
| src/secret/fake_security.rs:1 | 171 | Testing-feature fake security executable fixture | — | — | 0/0 |
| src/secret/file_store.rs:1 | 1472 | Claude credential file read/write/pending recovery | rustix@80; serde@86 | — | 0/0 |
| src/secret/foreign_activity.rs:1 | 163 | Decide whether foreign use bars namespace writes | — | — | 0/0 |
| src/secret/held_locks.rs:1 | 224 | Read persistent held-lock evidence for diagnosis | rustix@33; serde@34 | — | 0/0 |
| src/secret/keychain_write.rs:1 | 639 | Authorized stdin-only keychain write transport | — | macOS compound@484 | 2/6@67 |
| src/secret/location.rs:1 | 140 | Resolve authoritative keychain/file credential source | — | — | 0/0 |
| src/secret/mod.rs:1 | 388 | Reader interface, error classes and platform selection | — | macOS/Linux@76,334,352,358,372 | 6/9@58 |
| src/secret/namespace_lock.rs:1 | 453 | Persistent-inode single-writer flock and holder record | rustix@45; serde@49 | — | 0/0 |
| src/secret/pending.rs:1 | 424 | Provider-neutral staged-credential replay/discard rules | serde@42 | — | 0/0 |
| src/secret/secret_file.rs:1 | 426 | Shared exclusive-temp/fsync/rename credential primitive | — | — | 0/0 |
| src/secret/security_cli.rs:1 | 406 | Bounded read-only security(1) child transport | — | — | 0/1@207 |
| **Secret/storage subtotal (16)** | **8,770** | | | | |

### TUI
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/tui/app.rs:1 | 197 | Pure watch state reducer/events/effects | jiff@28 | — | 0/0 |
| src/tui/fixtures.rs:1 | 113 | Test-only deterministic shared TUI row builders | jiff@13; serde_json@14 | — | 0/0 |
| src/tui/mod.rs:1 | 207 | Terminal entry/restore, panic and cleanup hooks | crossterm@53; ratatui@60 | — | 0/0 |
| src/tui/ui.rs:1 | 194 | Pure watch frame/gauge rendering | jiff@34; ratatui@35 | — | 0/0 |
| **TUI subtotal (4)** | **711** | | | | |

### Usage
| Path | LOC | Responsibility | External use imports | OS cfg | T |
|---|---:|---|---|---|---|
| src/usage/cache.rs:1 | 242 | Per-account raw-response cache and stale fallback | serde@38; serde_json@40 | — | 0/0 |
| src/usage/mod.rs:1 | 16 | Shared normalized usage/cache module boundary | — | — | 0/0 |
| src/usage/model.rs:1 | 306 | Normalized optional percentages, windows, money, credits | jiff@28; serde_json@29 | — | 0/0 |
| **Usage subtotal (3)** | **564** | | | | |
| **GRAND TOTAL (98 files)** | **51,922** | | | | **60 direct / 89 total feature cfg attributes** |

Rollups without double-counting: commands including Codex = 16,817 LOC; provider including both providers = 16,406 LOC; runtime including OS backends = 3,306 LOC.

Parent gates: `secret/fake_reader.rs` is `cfg(test)` via `src/secret/mod.rs:61-62`; `secret/fake_security.rs` is feature-gated via `src/secret/mod.rs:58-59`; `tui/fixtures.rs` is `cfg(test)` via `src/tui/mod.rs:46-47`; `runtime/lock_order.rs` is feature-gated via `src/runtime/mod.rs:15-16`. Linux/macOS backend selection is `src/runtime/proc.rs:11-23`. Other operating systems hit `compile_error!` at `src/runtime/proc.rs:18-19`.

Parts (b), (c), and requirements gaps follow separately.

---

Model: runtime-reported `claude-gpt-6-astra-ultrafast[1m]` (provider identity not independently verified).

## Dependency map — part (b)

Versions below are **the manifest's declared requirements**, not Cargo.lock resolutions. Only `ureq = "=3.4.1"` is an exact pin; a bare `"0.23.1"` is still a Cargo compatibility requirement. No lockfile/version research or web lookup was performed. Go candidates are implementation options for the separate research lane to validate, not researched compatibility claims.

### User-decided mappings (do not reopen)
| Rust crate / requirement | Actual use / source | Go mapping |
|---|---|---|
| clap `4`, features `derive,env` (Cargo.toml:18) | Entire CLI and env-backed global config option, src/cli.rs:24-29,339-348; parse at src/main.rs:39 | `github.com/spf13/cobra`; explicitly no Elvish |
| clap_complete `4` (Cargo.toml:19) | Completion shell enum and generation, src/cli.rs:375; src/commands/completions.rs:38-47 | Cobra-generated completions; remove Elvish from the compatibility surface |
| crossterm `0.29` (Cargo.toml:20) | Input events, raw mode, alternate screen and cursor restoration, src/commands/watch.rs:55-60; src/tui/mod.rs:53-59 | `charm.land/bubbletea/v2` + `charm.land/lipgloss/v2` |
| ratatui `0.30` (Cargo.toml:27) | Terminal backend and frames/gauges, src/tui/mod.rs:60-61; src/tui/ui.rs:35-41 | `charm.land/bubbletea/v2` + `charm.land/lipgloss/v2` |
| tabled `0.22` (Cargo.toml:37) | Status/account table construction, src/render/table.rs:56-57; src/commands/accounts.rs:41-42 | `charm.land/lipgloss/v2/table` |
| toml `1.1.6`, defaults off, features `parse,std` (Cargo.toml:39) | Read Codex config keys through DeTable/DeValue, src/provider/codex/home.rs:257-266 | `github.com/zchee/go-toml` |
| serde_json `1`, features `preserve_order,float_roundtrip` (Cargo.toml:34) | Versioned reports and mutable credential documents; src/render/json.rs:30-33; src/provider/codex/credentials.rs:1-10,53-54 | `encoding/json/v2` + `encoding/json/jsontext`; explicitly preserve unknown members/order/number fidelity where required, not plain map decode/re-encode |
| insta `1`, dev (Cargo.toml:58) | Table/TUI snapshots, src/render/table_tests.rs:127,145,158; src/tui/ui_tests.rs:9 | `testdata/*.golden` + `go-cmp`, explicit `-update` |
| jsonschema `0.56`, dev (Cargo.toml:59) | Validate output against shipped schemas, src/render/json.rs:338,537; src/render/json_v2.rs:353; src/render/codex_doctor.rs:533 | `github.com/santhosh-tekuri/jsonschema/v6` |
| Keychain transport — **not a Cargo dependency** | Already a bounded `security(1)` subprocess reader/writer, src/secret/security_cli.rs:1-8; src/secret/keychain_write.rs:1-9 | `os/exec` invoking `security(1)`, no cgo; retain split read/write APIs and stdin-only writes |

### Other production dependencies (standard-library option first)
| Rust crate / requirement | Actual use / source | Standard library → third-party option; tradeoff / decision |
|---|---|---|
| anyhow `1` (Cargo.toml:16) | No `anyhow` reference found anywhere in src/ or tests/; only manifest occurrence | Omit. If future context wrapping is needed: `errors` + `fmt.Errorf("...: %w", err)` → `github.com/cockroachdb/errors`; the latter adds richer diagnostics but has no current source use to justify a dependency |
| base64 `0.23.1` (Cargo.toml:17) | URL-safe unpadded PKCE and JWT payload decode, src/provider/claude/oauth.rs:63-64; src/provider/codex/claims.rs:33-34 | `encoding/base64` → `github.com/segmentio/asm/base64`; std is sufficient for these small inputs, SIMD alternative needs measurement and strict decoder parity |
| etcetera `0.11` (Cargo.toml:21) | XDG config resolution on **macOS and Linux**, src/config/paths.rs:98-116 | `os.LookupEnv`, `os.UserHomeDir`, `filepath` implementing the existing XDG rule → `github.com/adrg/xdg`; third party centralizes XDG semantics. Do **not** substitute `os.UserConfigDir()` alone on macOS. Cache is under config root, not OS cache dir: src/config/paths.rs:200-215 |
| hex `0.4.3` (Cargo.toml:22) | Namespace hash suffix, credential hex line, temporary filenames, src/provider/claude/namespace.rs:133-136; src/provider/claude/credentials.rs:292; src/provider/claude/claude_json.rs:1035 | `encoding/hex` → `github.com/tmthrgd/go-hex`; std avoids a tiny-input optimization dependency |
| jiff `0.2` (Cargo.toml:23) | Parsed timestamps, local zone/countdowns, persisted refresh state, src/render/reset.rs:47-49; src/provider/codex/auth_store.rs:67; src/provider/codex/credentials.rs:49-50 | `time` (+ `time/tzdata` if self-contained zone data is required) → `github.com/jonboulle/clockwork` for controllable clocks; clockwork complements, not replaces, parsing/time-zone logic. **JUDGEMENT NEEDED:** supported timestamp lexical/range parity and bundled tzdata policy |
| libc `0.2` (Cargo.toml:24) | Darwin proc_listpids/proc_pidinfo/proc_name ABI, src/runtime/proc/macos.rs:350,382,416-445,463 | `os`/`syscall` cover fragments but no equivalent standard high-level libproc API → `golang.org/x/sys/unix`, potentially `github.com/ebitengine/purego` for missing Darwin symbols. **JUDGEMENT NEEDED:** exact no-cgo process backend. Replacing with `ps` is not equivalent: source explicitly documents its sandbox exec failure, src/runtime/proc/macos.rs:10-16 |
| open `5.4.3` (Cargo.toml:25) | Detached browser opening for Claude login, src/commands/login.rs:170 | `os/exec` with platform opener → `github.com/pkg/browser`; small explicit platform layer vs maintained browser abstraction |
| rand `0.10.2` (Cargo.toml:26) | PKCE/state bytes, random temp names, retry jitter, src/provider/claude/oauth.rs:383; src/secret/file_store.rs:993; src/secret/config_lock.rs:426 | `crypto/rand` for credential-related randomness, `math/rand/v2` only for jitter → `golang.org/x/exp/rand` only as a noncryptographic jitter alternative; not a PKCE replacement, no need over std |
| rustix `1`, features `event,fs,param,process,termios` (Cargo.toml:28) | dirfd-relative I/O, flock, process IDs/signals, terminal poll/flush; src/secret/file_store.rs:80-85; src/secret/namespace_lock.rs:45-48; src/runtime/tty.rs:8-11; src/runtime/signals.rs:61-62; src/commands/mod.rs:118-124 | `os`, `os/exec`, `os/signal`, `syscall` where available → `golang.org/x/sys/unix`; low-level flags/dirfd/termios semantics favor x/sys. Optional `github.com/gofrs/flock` only if persistent inode, timeout, and descriptor behavior match. A generic lock package does not replace the peer-directory-lock protocol |
| rustls `0.23.45`, defaults off (Cargo.toml:29-31) | Identify TLS errors nested inside I/O errors, src/provider/codex/oauth.rs:415-429; test TLS server, src/provider/codex/oauth_tests.rs:526-532 | `crypto/tls`, `crypto/x509`, `errors.As` → `github.com/refraction-networking/utls` only if a nonstandard TLS stack is actually needed. **JUDGEMENT NEEDED:** classify Go transport failures by the same resend contract; uTLS is not required by present source |
| secrecy `0.10` (Cargo.toml:32) | SecretString/explicit exposure and zeroizing buffers for credentials/HTTP, src/provider/claude/credentials.rs:46-47; src/provider/codex/credentials.rs:51-52; src/provider/codex/oauth.rs:336 | Private `[]byte` wrappers, explicit exposure, redacted String/GoString, overwrite on release → `github.com/awnumar/memguard`; **JUDGEMENT NEEDED:** required memory-erasure/locking guarantee in GC-managed Go. Neither ordinary Go strings nor redacted logging alone reproduce Rust drop/zeroization semantics |
| serde `1`, feature `derive` (Cargo.toml:33) | Typed config/report serialization/deserialization, src/config/mod.rs:50-51; src/config/codex.rs:34-35; src/render/json.rs:31 | Go structs and explicit `encoding/json/v2` methods/tags → already-selected `github.com/zchee/go-toml` for the TOML side; no generic derive dependency needed. Preserve absence/null/default behavior explicitly |
| sha2 `0.11.0` (Cargo.toml:35) | SHA-256 namespace/credential digests and PKCE, src/provider/claude/namespace.rs:133-135; src/provider/claude/credentials.rs:52-53; src/provider/claude/oauth.rs:70-71 | `crypto/sha256` → `github.com/minio/sha256-simd`; std fits small blobs, alternate only for measured bulk-hash benefit |
| signal-hook `0.4` (Cargo.toml:36) | Dedicated TERM/HUP/INT handling and teardown ordering, src/runtime/signals.rs:63-66,117-133 | `os/signal` + explicit cancellation/coordinator cleanup → `github.com/oklog/run` as lifecycle coordination helper; helper reduces orchestration but does not preserve exit/child semantics automatically |
| thiserror `2` (Cargo.toml:38) | Typed application/domain errors and contextual messages, src/error.rs:31,88-143; src/secret/claude_lock.rs:258-259 | Concrete error types + `errors.Is/As`, `fmt.Errorf` → `github.com/cockroachdb/errors`; standard idioms preserve typed decisions without added stack machinery |
| tracing `0.1.44` (Cargo.toml:40) | Structured diagnostics, including row failures and cleanup, src/commands/status.rs:54; src/runtime/cleanup.rs:332 | `log/slog` → `go.uber.org/zap`; standard structured logs suffice, zap is an optimization/ecosystem option |
| tracing-subscriber `0.3.23`, feature `env-filter` (Cargo.toml:41) | RUST_LOG filter, stderr sink, terminal buffering, src/main.rs:66-90; src/runtime/log_writer.rs:50 | `log/slog` handler + RUST_LOG compatibility parser and existing buffer policy → `go.uber.org/zap` with custom filter/sink adapter; **JUDGEMENT NEEDED:** full RUST_LOG directive compatibility versus documented subset. Neither default logger reproduces it |
| unicode-normalization `0.1` (Cargo.toml:42) | NFC normalization **before namespace hashing**, src/provider/claude/namespace.rs:59,133-136,330 | No NFC normalization API in standard library → `golang.org/x/text/unicode/norm`; needed to preserve exact names; do not substitute rune iteration or path cleaning |
| ureq **`=3.4.1`**, feature `json` (Cargo.toml:43-47) | Claude/Codex usage, profile/token clients; per-phase deadlines, no redirects on refresh, exhaustive outcome categorization; src/provider/claude/oauth.rs:407-408,470-471; src/provider/claude/usage.rs:132,146-154; src/provider/codex/usage.rs:255,283-296; src/provider/codex/oauth.rs:233-246,398-429 | `net/http`, `net`, `httptrace`, explicit transports/deadlines → `resty.dev/v3`; wrapper conveniences add policy surfaces but do not prove single-send behavior. **JUDGEMENT NEEDED:** Go's possible internal request retries, redirects, TLS error wrapping, per-phase timeout equivalence and ambiguous-send classification. Preserve semantic outcomes, not Rust variant names |
| url `2` (Cargo.toml:48) | Authorization URL construction/encoding and callback parsing, src/provider/claude/oauth.rs:72 | `net/url` → `github.com/goware/urlx`; standard is enough, extra normalization risks changing callback/authorization semantics |
| procfs-core `0.18`, defaults off; **Linux-only** (Cargo.toml:50-51) | Parse already-bounded/validated stat/status procfs bytes, src/runtime/proc/linux.rs:1-4,16-21 | `os.ReadFile`/bounded reads plus a local parser → `github.com/prometheus/procfs`; reusable parser vs exact visibility/input-bound requirements. **JUDGEMENT NEEDED:** whether candidate APIs preserve limits and avoid prohibited process-argument/environment collection |

### Remaining dev dependencies
| Crate / requirement | Source use | Standard library → third-party option; tradeoff |
|---|---|---|
| rustix `1`, feature `pty` (Cargo.toml:54) | Real PTY tests for terminal readiness/RC, src/runtime/tty_tests.rs:14-25; tests/e2e_swap/remote_control.rs:33-45 | `os`/`syscall` manual PTY setup → `github.com/creack/pty` and x/sys/unix; creack reduces platform-specific test scaffolding |
| assert_cmd `2.2.2` (Cargo.toml:55) | Real binary invocation/assertions, tests/cli_smoke.rs:23; tests/common/mod.rs:57 | `testing` + `os/exec` → `gotest.tools/v3/icmd`; explicit subprocess control vs concise assertions |
| flate2 `1.1.10` (Cargo.toml:56) | Gzip response fixtures, src/provider/codex/oauth_tests.rs:12-13; src/provider/codex/usage_tests.rs:6-7 | `compress/gzip` → `github.com/klauspost/compress/gzip`; standard suffices for test fixtures |
| httpmock `0.8` (Cargo.toml:57) | Local usage/token endpoint expectations, tests/e2e_status.rs:32-34; src/provider/claude/oauth_tests.rs:16-17 | `net/http/httptest` + real loopback/TLS handlers → `github.com/jarcoal/httpmock`; prefer real HTTP transport tests, since interception cannot validate send/timeout/TLS behavior. Reconcile with the port's test policy rather than automatically copying mocks |
| predicates `3.1.4` (Cargo.toml:60) | stdout/stderr substring expectations, tests/cli_smoke.rs:24; tests/e2e_keychain.rs:28 | `strings`/`bytes`/`regexp` + `testing` → already-selected `github.com/google/go-cmp/cmp` for structured comparisons; no new matcher DSL required |
| rcgen `0.14.10`, defaults off, features `crypto,ring` (Cargo.toml:61-63) | Generate self-signed TLS test certificate, src/provider/codex/oauth_tests.rs:523-532 | `crypto/x509`, `crypto/ecdsa`, `crypto/rand` (or httptest TLS for default certificate) → `go.step.sm/crypto/x509util`; standard is sufficient, third party adds certificate-template convenience |
| tempfile `3.27.0` (Cargo.toml:64) | Isolated real filesystem trees/files, src/secret/namespace_lock_tests.rs:7; src/commands/mod_tests.rs:125 | `testing.T.TempDir`, `os.CreateTemp`, `os.MkdirTemp` → `github.com/rogpeppe/go-internal/testscript` for script-test workdirs; std for units, third party only if a script harness is desired |

### Feature gates
`testing = []` is the only manifest feature and enables no optional dependency by itself (Cargo.toml:7-13). It gates fake children, URL overrides, injected faults/pause points, and lock-order witnesses. Linux alone adds procfs-core (Cargo.toml:50-51). The release prohibition is explicit in Cargo.toml:7-11 and AGENTS.md:31-33; reproduce that build separation in Go rather than an always-compiled runtime switch.

There are **no direct dependencies** named dirs, home, fs2, flock, nix, time, or chrono in this manifest: the actual counterparts are etcetera, rustix/libc, and jiff (Cargo.toml:15-64).

---

Model: runtime-reported `claude-gpt-6-astra-ultrafast[1m]` (provider identity not independently verified).

## CLI wiring — part (c)

### Startup and top-level routes
`main` calls `Cli::parse()`, initializes tracing, creates cancellation state, installs signal handling, dispatches, defers to an in-flight signal exit, then converts the result into a process exit (`src/main.rs:38-63`). CLI parse can terminate before logging/signals exist. Signal-install failure explicitly exits fatal (`src/main.rs:43-47`).

| Parsed command | Dispatch / implementation |
|---|---|
| `claude ...` | `src/cli.rs:355-358` → `dispatch_claude`, `src/main.rs:102,136` |
| `codex ...` | `src/cli.rs:361-364` → `dispatch_codex`, `src/main.rs:103,115` |
| `completions <shell>` | `src/cli.rs:367,370-375` → `commands::completions::run`, `src/main.rs:104-107`; generation and silent broken-pipe handling at `src/commands/completions.rs:38-51` |

### Claude routes
| CLI command | Implementation dispatch |
|---|---|
| `claude status` | `commands::status::run`, `src/main.rs:138` |
| `claude watch` | `commands::watch::run`, `src/main.rs:139` |
| `claude login` | `commands::login::run`, `src/main.rs:140-142` |
| `claude import` | `commands::import::run`, `src/main.rs:143-145` |
| `claude doctor` | `commands::doctor::run`, `src/main.rs:146-148` |
| `claude accounts list/show/remove/relocate/forget/unforget` | `commands::accounts::run`, `src/main.rs:149-151`; nested routes `src/commands/accounts.rs:102-122`; argument definitions `src/cli.rs:470-511` |
| `claude use` | `commands::use::run`, `src/main.rs:152`; isolated launch / live / undo / forget share this command, `src/cli.rs:564-621` |
| `claude exec` | `commands::export::run_exec`, `src/main.rs:153-155`; argv after `--` is required, `src/cli.rs:642-644` |
| `claude env` | `commands::export::run_env`, `src/main.rs:156-158`; shell choices zsh/bash/fish, `src/cli.rs:555-562,667-669` |

`isolate.rs` and `export.rs` are implementation modules, not top-level `isolate`/`export` CLI verbs (`src/cli.rs:380-403`; `src/commands/isolate.rs:1-10`; `src/commands/export.rs:1-10`).

### Codex routes
| CLI command | Implementation dispatch |
|---|---|
| `codex status` | Direct `commands::codex::status::run`, `src/main.rs:117-119` |
| `codex watch` | Direct `commands::codex::watch::run`, `src/main.rs:120-122` |
| `codex login` | Fallback `commands::codex::run` at `src/main.rs:123`, then `login::run`, `src/commands/codex/mod.rs:59` |
| `codex import` | Same fallback, then `import::run`, `src/commands/codex/mod.rs:60` |
| `codex doctor` | Same fallback, then `doctor::run`, `src/commands/codex/mod.rs:62` |
| `codex accounts list/show/remove/forget/unforget/set/refresh` | Same fallback, then `accounts::run`, `src/commands/codex/mod.rs:61`; inner routing `src/commands/codex/accounts.rs:78-101` |
| `codex accounts set <id> --refresh <auto\|never>` | `accounts_refresh::set`, `src/commands/codex/accounts.rs:90-92`; args `src/cli.rs:803-809,837-842` |
| `codex accounts refresh <id> [--resend\|--reset-floor] [--yes]` | `accounts_refresh::refresh`, `src/commands/codex/accounts.rs:93-100`; args `src/cli.rs:813-827` |

`commands::codex::run` also has status/watch arms (`src/commands/codex/mod.rs:57-58`), but normal main dispatch catches them first. **Do not port stale stub comments:** `src/commands/codex/mod.rs:7-18,49-50` still talks about stubs; executable routes at `src/commands/codex/accounts.rs:90-100` call real implementations. `src/main.rs:113-114` and `src/cli.rs:676-681` also carry outdated staging descriptions.

### Exit contract
| Producer | Exact behavior / source |
|---|---|
| Success | `EXIT_OK = 0`, `src/error.rs:34`; normal `Result<(), AppError>` commands mapped to 0 by `src/main.rs:106,118,121,138-158` and `src/commands/codex/mod.rs:57-62` |
| Fatal | `EXIT_FATAL = 1`, `src/error.rs:37`; `AppError::Config` and `Io`, `src/error.rs:152-154`; signal installation failure at `src/main.rs:47` |
| Degraded / refused | `EXIT_PARTIAL = 2`, `src/error.rs:40`; Keychain/Http/Auth/Refused/Partial variants, `src/error.rs:155-159` |
| Status partial | Visible failure count produces `AppError::Partial` after output, `src/commands/status.rs:219`; Codex counterpart `src/commands/codex/status.rs:194` |
| Parser errors | Clap usage error 2 is explicitly documented separately from application partial 2, `src/cli.rs:42-48`; parsing is `src/main.rs:39` |
| Interactive child | `claude use` / `exec` return child code rather than fixed 0, `src/main.rs:129-132,152-154`; normal child code forwarded, signal termination mapped to `128 + signal`, fallback 1 at `src/commands/export.rs:317-322` |
| Process signals | TERM/HUP/INT → 143/129/130 after child teardown and emergency cleanup, `src/runtime/signals.rs:103-110,117-133`; main waits for the signal exit at `src/main.rs:50-56` |
| Live-swap outcome | Return `report.outcome.exit_code()` at `src/commands/use.rs:450,481,4266`; mappings at `src/provider/claude/swap.rs:265-278,447-456`; preflight RC refusal directly returns 30 at `src/commands/use.rs:213` |

All named swap constants live in `src/cli.rs`:
| Code | Constant | Meaning | Definition |
|---:|---|---|---|
| 10 | REFUSED_A | Compromised held lock | src/cli.rs:73 |
| 11 | REFUSED_C | CLAUDE_CODE_OAUTH_TOKEN overrides stores | src/cli.rs:76 |
| 12 | REFUSED_D | Credential exceeds keychain stdin line bound | src/cli.rs:79 |
| 13 | REFUSED_E | Live-undo namespace environment mismatch | src/cli.rs:90 |
| 14 | REFUSED_F | Displaced credential cannot be preserved | src/cli.rs:93 |
| 15 | PRECONDITION | Selected namespace is not owned | src/cli.rs:98 |
| 16 | BUSY | Peer lock remains held | src/cli.rs:101 |
| 17 | DISCARDED | Item changed while held | src/cli.rs:104 |
| 18 | UNKNOWN | Write result cannot be determined | src/cli.rs:107 |
| 19 | WRITE_FAILED | Write child returned failure | src/cli.rs:117 |
| 20 | CANCELLED | Confirmation declined/unavailable | src/cli.rs:129 |
| 21 | NEEDS_REFRESH | Incoming migrated credential needs in-place refresh | src/cli.rs:144 |
| 22 | AUDIT_REFUSED | Live write cannot obtain durable audit | src/cli.rs:161 |
| 23 | LIVE_UNREACHABLE | Live store cannot be resolved | src/cli.rs:171 |
| 24 | LIVE_ITEM_ABSENT | Live keychain item absent | src/cli.rs:185 |
| 27 | LIVE_UNDO_ITEM_CHANGED | Undo sees a third account | src/cli.rs:194 |
| 29 | IDENTITY_UNAVAILABLE | Current credential identity unproved | src/cli.rs:206 |
| 30 | RC_NOT_DISCONNECTED | RC disconnect/TTY/platform precondition failed | src/cli.rs:212 |

Applied/already-active outcomes are 0 (`src/provider/claude/swap.rs:449`). Refusal B is deliberately only a warning/0 (`src/cli.rs:52,58-60`). Codes 25/26/28 are retired, not available for reuse (`src/cli.rs:55,203-205`).

### Global flag and production environment
Only one **explicitly global** option exists: `--config-dir <DIR>` ↔ `AGCTL_CONFIG_DIR` (`src/cli.rs:342-344`). It is accepted before/after subcommands (`src/cli.rs:6-11`). Resolution is CLI override > environment > XDG config root plus `agctl` (`src/config/paths.rs:107-116,137-140`), including XDG semantics on macOS (`src/config/paths.rs:98-101`). Clap derives help/version from `#[command(name = "agctl", version, ...)]` at `src/cli.rs:340`; there is no explicit global verbose/debug flag.

| Exact variable | Source / meaning |
|---|---|
| AGCTL_CONFIG_DIR | Store root override, src/cli.rs:343; src/config/paths.rs:80,108 |
| RUST_LOG | Logging filter; unset/invalid → warn; stderr only, src/main.rs:66-90 |
| AGCTL_CLAUDE_USER_AGENT | Claude provider HTTP agent, src/provider/claude/mod.rs:40; consumed by src/provider/mod.rs:83 |
| AGCTL_CODEX_USER_AGENT | Codex provider HTTP agent, src/provider/codex/mod.rs:63; consumed by src/provider/mod.rs:83 |
| AGCTL_CLAUDE_OAUTH_SCOPES | Replaces requested login scopes, src/provider/claude/oauth.rs:135,387-394 |
| CLAUDE_SECURESTORAGE_CONFIG_DIR | Claude credential namespace, src/provider/claude/namespace.rs:65,109-113 |
| CLAUDE_CONFIG_DIR | Claude configuration directory, src/provider/claude/namespace.rs:68,109-113 |
| CLAUDE_CODE_OAUTH_TOKEN | Environment credential override / live-swap refusal; src/provider/claude/namespace.rs:72; src/cli.rs:74-76. Explicitly removed from isolation exports via src/commands/export.rs:44 |
| CODEX_HOME | Codex home input, defined src/provider/codex/home.rs:53; read only by command-layer constructor src/commands/codex/mod.rs:73-77 |
| HOME | Claude/Codex fallback home, src/provider/claude/namespace.rs:113; src/commands/codex/mod.rs:76 |
| USER, LOGNAME | Keychain account name, first successful value or empty, src/secret/mod.rs:383 |
| PATH | Resolve tmux/vendor codex children, src/runtime/tmux.rs:248-252; src/provider/codex/login_child.rs:678-679 |
| CODEX_API_KEY, CODEX_ACCESS_TOKEN, CODEX_REFRESH_TOKEN_URL_OVERRIDE, CODEX_APP_SERVER_LOGIN_CLIENT_ID | **Presence-only doctor diagnostics**, not agctl endpoint controls: src/commands/codex/doctor.rs:113-117,180 |

Child environment is another compatibility surface, not additional agctl global options:
- `codex login` passes exactly `HOME`, `PATH`, `TMPDIR`, `LANG`, `TERM`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY`, lowercase `http_proxy`, `https_proxy`, `no_proxy`, `all_proxy`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, plus nonempty `LC_`-prefixed names (`src/provider/codex/login_child.rs:120-139,209-236`). `CODEX_HOME` is replaced with the owned scratch path, never inherited (`:220-222,236`).
- tmux child receives `PATH`, `HOME`, `TMUX`, `TMUX_TMPDIR` (`src/runtime/tmux.rs:375-376`).
- XDG variables consulted transitively by etcetera, or browser-opener environment behavior, were not dependency-source audited; do not infer exhaustive transitive env compatibility from this direct-source inventory.

### Build-gated environment controls
These must not become production runtime switches:
| Names | Source |
|---|---|
| AGCTL_CLAUDE_TOKEN_URL, AGCTL_CLAUDE_AUTHORIZE_URL, AGCTL_CLAUDE_PROFILE_URL | src/provider/claude/oauth.rs:138-154,484-505 |
| AGCTL_CLAUDE_USAGE_URL | src/provider/claude/usage.rs:122-123,163-168 |
| AGCTL_CODEX_TOKEN_URL | src/provider/codex/oauth.rs:85-86,268-273 |
| AGCTL_CODEX_USAGE_URL | src/provider/codex/usage.rs:139-140,311-316 |
| AGCTL_KEYCHAIN_BACKEND | macOS testing-only, src/secret/mod.rs:76-77,334-345 |
| AGCTL_SECURITY_BIN | src/secret/mod.rs:80-81,341-345; writer src/secret/keychain_write.rs:515-520 |
| AGCTL_CODEX_BIN | src/provider/codex/login_child.rs:69-70,673-674 |
| AGCTL_NO_BROWSER | src/commands/login.rs:147-148,161-163 |
| AGCTL_FAULT, AGCTL_FAULT_RESUME | src/runtime/fault.rs:61-67,104-113,144-148,178-184 |
| AGCTL_SWAP_DEADLINE_MS | src/commands/use.rs:682-684 |
| AGCTL_RC_BUDGET_MS | src/provider/claude/remote_control.rs:213-215 |
| AGCTL_TMUX_BIN | src/runtime/tmux.rs:25-26,244-245 |
| AGCTL_FAKE_CODEX_* | Testing-only child pass-through prefix, src/provider/codex/login_child.rs:147-148,224-227 |
| AGCTL_FAKE_TMUX_* | Testing-only child pass-through prefix, src/runtime/tmux.rs:27-28,380-382 |

Fake security fixture controls (not production flags) documented at `src/secret/fake_security.rs:17-26`: `AGCTL_FAKE_SECURITY_LOG`, `AGCTL_FAKE_SECURITY_SLEEP`, `AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT`, `AGCTL_FAKE_SECURITY_PREFLIGHT_STDERR`, `AGCTL_FAKE_SECURITY_DUMP`, `AGCTL_FAKE_SECURITY_DUMP_EXIT`, `AGCTL_FAKE_SECURITY_ITEMS`, `AGCTL_FAKE_SECURITY_FIND_EXIT`, `AGCTL_FAKE_SECURITY_WRITE_EXIT`, `AGCTL_FAKE_SECURITY_STDERR`. The module embeds `fixtures/fake-security.sh` (`src/secret/fake_security.rs:78`); that external script was not part of the src-file inventory.

## Analyst Review: Go port requirements

### Missing Questions
1. Is the port's public binary/store/env identity still `agctl`/`AGCTL_*`, or deliberately renamed? Go module name alone does not decide persisted/user-facing compatibility. Existing binary and default store names are `agctl` (`src/cli.rs:340`; `src/config/paths.rs:140`).
2. Does no-cgo cover only the decided keychain child, or also Darwin process inspection? This blocks choosing the process backend (`src/runtime/proc/macos.rs:350,431,463`).
3. Is parity exact for RUST_LOG directives, timestamp ranges, table bytes and legacy duration bounds, or only selected semantic cases? These determine adapters and golden acceptance. Existing duration parser is explicit (`src/cli.rs:286-318`), logging uses EnvFilter (`src/main.rs:79-90`).

### Undefined Guardrails
1. Freeze OS support to macOS/Linux and existing platform refusals unless explicitly expanded: unsupported OS compile failure `src/runtime/proc.rs:18-19`; RC macOS capability `:31`; keychain/peer-removal capabilities `src/secret/backend.rs:8,14`.
2. Preserve source bounds initially: 4 pass workers (`src/runtime/coordinator.rs:68`), cache TTL 300s (`src/usage/cache.rs:48`), watch floor 60s/default 300s (`src/cli.rs:33,446,751`), TUI log buffer 256KiB (`src/runtime/log_writer.rs:53`), read/preflight 2s and dump 10s (`src/secret/security_cli.rs:6-7`). Increasing these is a separate behavior change.
3. Separate production and test-seam binaries; do not replace compile-time exclusions with configuration checks (Cargo.toml:7-13).

### Scope Risks
1. A direct file-by-file translation mistakes 51,922 physical lines for production logic. The count includes extensive historical commentary and cfg-only helpers; parent gates are listed in part (a).
2. Recreating retired staging/stub behavior from old headers instead of executable routes regresses landed Codex commands (`src/commands/codex/mod.rs:7-18` versus `src/commands/codex/accounts.rs:90-100`).
3. Generic replacement libraries can change contracts: native macOS config paths, permissive Go duration parsing, generic OAuth form encoding, transport retries and serialization order are not interchangeable with the current behavior (`src/config/paths.rs:98-116`; `src/cli.rs:286-318`; `src/provider/claude/oauth.rs:4-10`; `src/provider/codex/oauth.rs:233-246,398-429`; `src/provider/codex/credentials.rs:1-10`).

### Unvalidated Assumptions
1. HTTP replacement preserves single-send/ambiguous outcome semantics — validate with real local sockets/TLS and phase-specific failures, not only mocked RoundTripper results (`src/provider/codex/oauth.rs:398-429`).
2. Go encoding preserves credential unknown fields, order and numeric values — validate round-trip fixtures independently from schema validation (`src/provider/codex/credentials.rs:5-10`; Cargo.toml:34).
3. Go package boundaries can reproduce Rust proof/lifetime constraints — specify which constraints remain structural and which require runtime state/negative tests (`src/provider/codex/proof.rs:1-9`; `src/provider/codex/permit.rs:1-10`).
4. Candidate dependencies support the chosen Go/platform policy — the separate research lane must verify APIs, licenses, versions and no-cgo support; no web claims are made here.

### Missing Acceptance Criteria
1. A command matrix checks every route above, unknown-argument rejection, `--config-dir` in every global position, stdout/stderr separation, all exit codes, and the deliberate Elvish exclusion.
2. Golden/schema tests cover Claude status v1, Codex status v2, doctor/isolation documents and TUI/table layouts, with normal test runs never updating fixtures (`src/render/json.rs:1-6,338,537`; `src/render/json_v2.rs:1-9,353`; `src/render/codex_doctor.rs:1-6,533`).
3. Fault matrices prove zero retry after ambiguous sends, persisted marker-before-send ordering, stale-cache behavior, unchanged foreign stores and child/temporary-file cleanup (`src/provider/codex/oauth.rs:6-8,398-429`; `src/secret/pending.rs:1-10`; `src/runtime/signals.rs:125-133`).
4. Production artifacts neither expose nor act on the testing controls catalogued above; test artifacts default away from real services when overrides are absent (`src/provider/codex/oauth.rs:262-266`; `src/secret/mod.rs:327-333`).
5. Run OS-specific gates on both supported platforms; Linux must retain unsupported keychain/RC/peer-removal outcomes rather than silently enabling broader behavior (`src/secret/backend.rs:8-17`; `src/runtime/proc.rs:31`).

### Edge Cases
1. Duration grammar: Rust accepts bare `300` and `2h`, rejects `10ms`, `2h30m`, fractions/negative values, trims outer whitespace, checks u64 second overflow (`src/cli.rs:286-318`). `time.ParseDuration` alone changes this grammar and has a smaller representable duration range; choose/document the range contract.
2. Canonically equivalent Unicode paths must hash to the same existing namespace; ordinary path cleanup is not NFC (`src/provider/claude/namespace.rs:133-136`).
3. A broken completions pipe is silent success (`src/commands/completions.rs:45-47`), while other output I/O errors can be fatal (`src/error.rs:154`).
4. An incoming process signal must not lose to main's ordinary command-exit path; the Rust code deliberately defers (`src/main.rs:50-56`).
5. Missing percentage is not zero and displayed percentages are floored, not rounded (`src/usage/model.rs:5-15`).

### Recommendations
- Resolve binary/store compatibility, Darwin process binding and transport outcome acceptance before estimating implementation work.
- Use the frozen executable behavior as the baseline; treat dependency replacement as semantic adaptation, not a name substitution.
- Keep the user's decided mappings closed. Prioritize tests for persistence, transport and process boundaries; those are the highest parity uncertainty.

### Open Questions
- [ ] Preserve `agctl`, `AGCTL_*` and existing disk paths, or specify an explicit migration? — Prevents accidental user-visible rename/data split.
- [ ] Must Darwin process observation also work without cgo? — Needed to select the libproc/sysctl bridge.
- [ ] Which parser/logging/rendering differences are accepted beyond removal of Elvish? — Determines strict compatibility fixtures.
- [ ] What is the required secret-memory handling guarantee in Go? — Redaction alone is not deterministic buffer erasure.

### Gaps / things I could not determine
- No cargo build/test, runtime CLI probing, benchmark, installed-vendor check or full protocol/security review was performed. This was a read-only source inventory.
- Candidate Go package availability/API/maintenance/license and resolved Cargo.lock versions were not researched, by design.
- Direct env reads were inventoried; transitive library env reads and external fixture-script contents were not fully audited.
- Full-body behavior was traced only where needed for dispatch/dependency/requirements conclusions; header/main-type summaries are not a proof of each module's full behavior.
- Go target is `module github.com/zchee/agentctl`, `go 1.27` (`/Users/zchee/go/src/github.com/zchee/agentctl/go.mod:1-3`). `main.go` is not literally empty: it contains a license header and `package main`, but no main function (`/Users/zchee/go/src/github.com/zchee/agentctl/main.go:1-15`).
- Nothing was written or edited, and no work remains in this inventory lane.

---

