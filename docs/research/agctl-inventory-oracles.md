# agctl inventory: test oracles, Linux/RC status, rendering rules

- Captured: 2026-10-05 15:00:00 JST (`date` in the command that assembled this file)
- Lane: read-only `analyst` worker (`~/.claude/agents/codex-analyst.md`), Rust source `/Users/zchee/rust/src/github.com/zchee/agctl` at branch `rc-restart`, commit `dbf6aeab84dfbe2465de496316d7bf6780940a60`
- Verbatim worker report, delivered by SendMessage and recovered from the worker transcript; nothing in it was executed by the lead.

---

Model: claude-gpt-6-astra-ultrafast[1m] (environment-reported model identifier).

Inventory report (e), source root `/Users/zchee/rust/src/github.com/zchee/agctl`, clean branch `rc-restart`, HEAD `dbf6aeab84dfbe2465de496316d7bf6780940a60` (git inspection). No files edited; no cargo/build/test commands run.

## (e) Reusable oracles

There are 41 files under `fixtures/`: 14 Claude, 14 Codex, 10 RC screen files and 3 executable stand-ins. All fixture DATA bytes can be reused without language conversion; SCRIPT bytes can be reused on Unix if the Go harness preserves the injection contract and installs their shell-tool prerequisites. Expiry-bearing credential fixtures are fixed parser/serialization vectors, not always-fresh credentials; the Rust E2E harness creates dynamic credentials via `blob`/`fresh_at` (`tests/common/mod.rs:1073-1116`).

### Stand-ins and injection
| Path | Purpose/injection | Go byte reuse |
|---|---|---|
| `fixtures/fake-security.sh` | Fakes `show-keychain-info`, `dump-keychain`, `find-generic-password`, and strict one-line `-i` writes; logs argv, indexes items by both account/service, rejects deletes. `Fixture::with_keychain` writes executable `bin/security` and sets `AGCTL_SECURITY_BIN`, `AGCTL_FAKE_SECURITY_LOG`, `_ITEMS`, `_DUMP` (`tests/common/mod.rs:529-553`). NOT a PATH replacement in the normal harness. `_SLEEP`, `_PREFLIGHT_EXIT`, `_FIND_EXIT`, `_WRITE_EXIT`, `_DUMP_EXIT`, stderr and marker controls live in the script. | Yes; needs POSIX shell/core tools and `xxd` OR Perl for hex writes (`fixtures/fake-security.sh:140-147`). |
| `fixtures/fake-codex.sh` | Fakes vendor login, auth.json, ordinary tmp/arg0 residue, daemon/held-lock/symlink/keychain-gain/failure cases; logs argv boundaries, cwd and environment names. `CodexFixture` writes its own executable and sets `AGCTL_CODEX_BIN` (`tests/common/codex.rs:83-95`). Child receives isolated CODEX_HOME; `_AUTH`, `_LOG`, `_EXIT`, `_SLEEP`, `_DAEMON_DIR`, `_NO_RESIDUE`, `_HELD_LOCK`, `_KEYCHAIN_GAIN[_ACCOUNT]`, `_ODD_LOCK`, `_TOUCH`, `_SENTINEL` controls (`fixtures/fake-codex.sh:20-45`). | Yes; Perl required for real held-lock branch, `awk`/sort and standard Unix utilities otherwise (`:62-83,120-148`). |
| `fixtures/fake-tmux.sh` | Fakes pane-state replies, visible screen capture, send-keys and atomic session-registry changes; never calls real tmux. E2E `wire_tmux` writes a wrapper, copies `fixtures/rc-screens/*`, sets `AGCTL_TMUX_BIN` and `AGCTL_FAKE_TMUX_*` (`tests/e2e_swap/remote_control.rs:212-257`). Production resolves tmux on PATH; override is testing-only (`src/runtime/tmux.rs:239-260`). | Yes for later RC phase; Go harness must recreate wrapper, pane/state/registry files. Needs Perl/JSON::PP, awk, shasum, cut, cp/mv (`fixtures/fake-tmux.sh:24-27,35-59,74-121`). |

### Claude data: every path under `fixtures/claude/`
- `credentials-old-blob.json`: legacy credential without tokenAccount (`tests/common/mod.rs:72-73`; `src/provider/claude/credentials_tests.rs:10-16`).
- `credentials-new-blob.json`: identified credential, refresh expiry, profile and optional members (fixture `:1-30`; same credentials test loader).
- `exchange-response.json`: OAuth exchange response shape (`src/provider/claude/oauth_tests.rs:24`; E2E reuse `tests/e2e_status.rs:1181`).
- `security-dump.txt`: service-list parser, live/suffixed/foreign/unrelated records (`src/secret/security_cli_tests.rs:19-20,38`; fake `_DUMP` at `fixtures/fake-security.sh:28-31`).
- `security-find.txt`: compact credential returned by find-generic-password (path; `src/secret/security_cli_tests.rs:12-16` fixture loader).
- `usage-2026-09-08.json`: captured normalized+legacy windows and disabled credits; `USAGE_BODY` (`tests/common/mod.rs:75-76`), served by local HTTP fixtures (`tests/e2e_status.rs:46-49`).
- `usage-empty-limits.json`: empty normalized/legacy limits.
- `usage-extra-usage-enabled.json`: metered credits, decimal units/rounding.
- `usage-extra-usage-unlimited.json`: null ceiling, known used amount.
- `usage-extra-usage-unmeasured.json`: enabled credits, missing used amount.
- `usage-legacy-only.json`: no usable normalized limits; legacy-window fallback.
- `usage-no-credits.json`: neither extra_usage nor spend.
- `usage-spend-without-extra-usage.json`: spend exists but extra_usage absent; n/a plus warning.
- `usage-unknown-kind.json`: preserve unfamiliar monthly_foo window.
All nine usage files are direct parser inputs (`src/provider/claude/usage_tests.rs:13-32`); HTTP injection is `AGCTL_CLAUDE_USAGE_URL`, `_TOKEN_URL`, `_AUTHORIZE_URL`, `_PROFILE_URL` (`tests/common/mod.rs:407-414`). No fixture-specific environment variable is required for in-process parser tests. All 14 reusable as-is as data.

### Codex data: every path under `fixtures/codex/`
- `auth-apikey.json`: API-key-only auth; `auth-codex-format.json`: ordinary serialized ChatGPT auth/JWT shape; `auth-unknown-members.json`: unknown top-level/nested members and order preservation (`src/provider/codex/credentials_tests.rs:25,35,59,351,386`). All direct loader inputs (`src/provider/codex/testkit_tests.rs:210-214`); an E2E copy may become fake child's `_AUTH` source or isolated auth.json.
- `config-auto.toml`, `config-ephemeral.toml`, `config-file.toml`, `config-keyring.toml`: each store policy; auto also custom base URL.
- `config-malformed.toml`: malformed TOML with a sentinel; `config-mcp-key.toml`: valid unrelated MCP key/sentinel. Loader writes them as isolated `config.toml` (`src/provider/codex/home_tests.rs:52,58,170`).
- `usage-2026-09-16.json`: captured weekly plus Spark additional limits and balance; `usage-credits-absent.json`: no credits plus swapped primary/secondary window durations; `usage-credits-unlimited.json`: unlimited credits/limit reached; `usage-no-rate-limit.json`: no normal rate-limit windows; `usage-sentinel-email.json`: raw-email redaction vector (`src/provider/codex/usage_tests.rs:22-27`).
All 14 reusable as-is. HTTP tests set `AGCTL_CODEX_USAGE_URL` and `_TOKEN_URL` themselves (`tests/e2e_codex_status.rs:133-134`; `tests/e2e_codex_import.rs:114-115`); those defaults are NOT supplied centrally by `CodexFixture::new`.

### RC screen data: every path under `fixtures/rc-screens/`
`c20-panel.txt` expected Disconnect / Show QR / focused Continue panel; `capture_ambiguous.txt` truncated input; `capture_invalid.bin` exact bytes FF FE; `dialog_conflict.txt` wrong option order; `draft_visible.txt` nonempty input; `echoed-prompts-draft.txt` earlier transcript prompts plus real draft; `echoed-prompts.txt` earlier transcript prompts plus empty real input; `empty-prompt.txt` empty real input; `mode_visible.txt` INSERT-mode footer; `stash_visible.txt` stashed-message banner. All reusable as-is; preserve NBSP after the real `❯` input marker versus ASCII space after echoed prompts (`src/runtime/tmux.rs:183-210`). `_SCREEN` supplies capture bytes (`fixtures/fake-tmux.sh:76-83`); parser/tests consume the files (`src/runtime/tmux_tests.rs:8`; E2E `remote_control_more.rs:554-556,623-624`).

### Schemas: all JSON Schema draft 2020-12, reusable byte-for-byte
| File | Current output and switch | Validators |
|---|---|---|
| `schemas/status.v1.json` | `agctl claude status --json`; version 1, no version-selector flag | `src/render/json_tests.rs` via `json::assert_valid` (`src/render/json.rs:324-347`); command report validation `src/commands/status_tests.rs:1657`. |
| `schemas/status.v2.json` | `agctl codex status --json`; version 2; supports shared provider envelope internally but DOES NOT replace Claude v1 | `tests/e2e_codex_status.rs:184-214` (`ac102_status_json_validates_against_v2_and_carries_no_needle`), raw case `:264-269`; `src/render/json_v2_tests.rs:49`. |
| `schemas/doctor.v1.json` | Internal Claude isolation/isolation_policy report ONLY; **no Claude doctor --json flag** | `src/render/json_tests.rs:629-678`; `src/commands/doctor_tests.rs:1494-1539` (`mcp_credential_entries_counts_only_servers_carrying_env_or_headers`). Schema description explicitly says not CLI reachable. |
| `schemas/codex-doctor.v1.json` | `agctl codex doctor --json`, version 1, no version-selector flag | `tests/e2e_codex_doctor.rs:119-130,295` (`the_report_names_every_item_and_validates_against_the_schema`); `src/render/codex_doctor_tests.rs:140-145`; Linux identity case `tests/e2e_linux_process.rs:205-220`. |
`src/cli.rs:407-440,543-551,711-744,870-874` fixes these flag surfaces. Schema validation establishes shape, not JSON member order/whitespace.

### Snapshot inventory and byte verdict
8 `src/render/snapshots/*.snap`, 0 `tests/**/*.snap`; additionally 3 `src/tui/snapshots/*.snap` and 3 `src/provider/codex/snapshots/*.snap` = 14 total.
All render snapshot basenames start `agctl__render__table__tests__`:
| Suffix | Producing test in `src/render/table_tests.rs` |
|---|---|
| `two_accounts.snap` | `two_healthy_accounts_render_as_one_aligned_table`, :124-127 |
| `hidden_footer.snap` | `hidden_rows_are_summarised_in_a_footer`, :131-145 |
| `all_rows.snap` | `all_reveals_the_hidden_rows_and_drops_the_footer`, :149-158 |
| `continuation_rows.snap` | `ac4_an_unknown_window_gets_a_continuation_row_naming_its_kind`, :162-177 |
| `no_numbers.snap` | `a_row_with_no_numbers_shows_em_dashes_rather_than_zeroes`, :181-189 |
| `degraded_states.snap` | `degraded_states_render_with_their_notes`, :193-219 |
| `credits_cells.snap` | `the_credits_cell_covers_every_state_the_column_can_reach`, :239-298 |
| `by_identity_kind_column.snap` | `the_kind_cell_reads_live_and_owned_for_a_folded_row_and_the_plain_kind_otherwise`, :480-487 |
Pinned clock/zone: `src/render/table_tests.rs:17-31` (fixed timestamp, +09:00).

Strip the initial exact `---` line and all following lines through the next exact `---` line; preserve EVERY subsequent byte. Do not assume a fixed 4/5-line header: assertion_line is optional. Example `two_accounts.snap:1-5`:
```
---
source: src/render/table_tests.rs
assertion_line: 127
expression: render(&report)
---
```
**Not generally raw-stdout bytes.** Pinned Insta 1.48.0 (`Cargo.lock:1557-1559`) does global trim_end and CRLF→LF (`/opt/local/rust/cargo/registry/src/index.crates.io-1949cf8c6b5b557f/insta-1.48.0/src/snapshot.rs:753-765`). Table preserves right padding and CLI appends LF (`src/commands/status.rs:213`; tabled 0.22.0 `src/tables/table.rs:873-888`). `two_accounts.snap` last line ends at `ok`, not the trailing spaces the table emits. Seven render snapshots require either Insta-equivalent normalization in the Go comparator OR regeneration/restoration of final padding for raw stdout. `hidden_footer.snap` ends in nonpadded footer: header-stripped body with its existing final LF is structurally eligible for direct stdout reuse, conditional on identical injected report; not runtime-verified here. Don't silently trim every line: interior trailing padding is part of the snapshots.

TUI three filenames have prefix `agctl__tui__ui__tests__` and suffixes `two_accounts_render_as_expected.snap` (84×20), `a_degraded_account_shows_its_badge_and_its_state.snap` (84×20), `an_empty_display_says_so_rather_than_showing_nothing.snap` (60×8); corresponding test names at `src/tui/ui_tests.rs:46,53,75`. They are quoted row-wise TestBackend Display dumps, **not** ANSI stdout. Reuse exact bodies only with a compatible frame-dump adapter (preserve spaces inside quotes); stripping header alone does not create a terminal-stream oracle.

Codex additional group:
- `agctl__provider__codex__account__tests__codex_table.snap`: `src/provider/codex/account_tests.rs:256-277`; footer-ending table, same conditional direct reuse as hidden_footer.
- `agctl__provider__codex__account__tests__codex_watch_two_rows.snap`: same file :281-296; 84×16 TestBackend dump, adapter required.
- `agctl__provider__codex__usage__tests__f78_capture_json_v2.snap`: `src/provider/codex/usage_tests.rs:174-186`; only `{windows, credits}` pretty-JSON fragment, NOT full CLI report; exact fragment reuse yes, raw CLI stdout no.

### `tests/common/**` to mirror in testutil
`mod.rs`: isolated temp config/home/bin/keychain-item tree (:133-162); endpoint close-by-default guards and browser suppression (:143-154); USER/LOGNAME pin (:160-161); captured-output/raw-process launch and env scrubbing (:799-903); registry/credential builders and 0600/0700 modes (:431-503); symlinked live-store/config layouts (:247-340); fake keychain allowlists/logs/items (:529-774); real kernel flock checks/holders (:1193-1255); bounded predicate polling, SIGTERM and concurrent pipe draining (:1267-1374); interactive manual-login driver (:1600-1728); append-only per-run aggregate argv log (:1545-1593). No injected E2E clock: `now_ms`, `fresh_at`, `expired_at` use wall time (:915-920,1073-1083); mtime helpers backdate real files (:379-395).
`codex.rs`: wraps those roots, installs owned fake executable, refuses CODEX_HOME injection and verifies every child HOME/seam (:83-95,188-194,277-330); JWT/auth helpers (:481-492), credential/namespace/marker paths (:505-535); stdout/stderr sentinel scans + audited-receipt check + no password lookup + optional AGCTL_E2E_TRACE_DIR captures (:340-465); `now_s` is real wall time (:476-479).

### Every E2E file (source declaration counts, not executed/pass counts)
| File under `tests/` | Commands / contract | Fixtures | #[test] count |
|---|---|---|---:|
| e2e_accounts.rs | Claude accounts remove/relocate/forget/unforget/list, status, doctor/removal | common, generated registry/files/locks, fake security in relevant cases | 11 |
| e2e_codex.rs | Claude status/accounts/doctor/use undo provider isolation; Codex parser/refusal surfaces | common, OLD_BLOB, mixed registries | 6 |
| e2e_codex_accounts.rs | Codex list/show/remove/forget/unforget | CodexFixture, generated auth/manifests | 9 |
| e2e_codex_accounts_refresh.rs | Codex accounts set/refresh; non-TTY refusal, zero POSTs | CodexFixture, local HTTP, markers | 7 |
| e2e_codex_doctor.rs | Codex doctor text/JSON | CodexFixture, fake security, auth/config/marker/sentinel manifests, schema | 13 |
| e2e_codex_import.rs | Codex import --from codex-home, subsequent status | CodexFixture, generated homes/auth, local HTTP, fake security in relevant cases | 11 |
| e2e_codex_login.rs | Codex login, child lifecycle/residue/failures | fake codex, auth/JWT files, fake security in relevant cases | 28 |
| e2e_codex_login_confirm.rs | Codex login replacement refusal/orphan adoption | fake codex, generated owned/scratch auth | 3 |
| e2e_codex_status.rs | Codex status text/JSON/raw; watch interval/non-TTY boundaries | CodexFixture, HTTP, generated usage/auth/markers, schema | 29 |
| e2e_doctor.rs | Claude env setup; doctor isolation/audit sections | common, generated session/config/audit files, no fake security | 6 |
| e2e_doctor_stale.rs | Claude doctor --remove-stale | common, held-record/lock/symlink fixtures | 5 |
| e2e_import.rs | Claude import --from keychain dry-run/idempotence | common + fake security | 5 |
| e2e_isolate.rs | Claude exec/env/use --forget | common + generated capture executable/session trees; no keychain | 21 |
| e2e_keychain.rs | Claude status/doctor/accounts list/import; stand-in write/aggregate argv invariants | common + fake security | 6 |
| e2e_linux_process.rs | Claude doctor/removal/import/status; Codex doctor JSON | common, real /proc and /bin/sleep, generated records, schema, fake-security sentinel | 3 |
| e2e_lock.rs | Source/build artifact contract scans, not a behavioral command suite | production sources and compiled artifact | 10 |
| e2e_login.rs | Claude manual login/cancellation | common LoginSession + local HTTP + generated credentials | 12 |
| e2e_refresh.rs | Claude status-driven refresh, races, signals, pending resolution | common + local HTTP + fake security in migration cases | 28 |
| e2e_remote_control_platform.rs | Claude use --live/--undo --restart-remote-control --json Linux refusal | common only | 1 |
| e2e_status.rs | Claude status table/JSON/raw/filter/cache/refresh | common + fake security + local HTTP; USAGE_BODY, OLD_BLOB, exchange-response.json | 22 |
| e2e_swap.rs | Claude use live/undo/catch-up; related status/accounts/doctor checks | common + fake security + local HTTP + generated live symlink/config trees | 129 |
| e2e_swap/remote_control.rs | RC disconnect/reconnect + swap | inherited swap harness, fake tmux/wrapper/screens, synthetic session registry/PTY peers | 11 |
| e2e_swap/remote_control_more.rs | RC rejection/timeout/attestation/recovery variants | same RC harness | 25 |
| e2e_tracing.rs | Claude status/refresh/login; Codex refresh trace/output containment; pipe draining | common/Codex helpers + HTTP + fake security | 5 |
Counts from `rg -c '#\[(tokio::)?test\]'` per listed path: source-only inventory, platform cfgs reduce active sets. Whole-file macOS gates: import, keychain, swap (+ its RC modules), doctor_stale, codex_login_confirm; Linux-only: linux_process and remote_control_platform (each file :1). Other files contain narrower cfg routing.

Extra reusable contract sources: `tests/cli_smoke.rs:65-255` (help/version/subcommand/interval/table/JSON/completions/broken-pipe assertions); `tests/feature_guard.rs` (testing-feature guard). Five scripts need Go-specific rewriting, not verbatim execution: `docs-gate.sh` prose checks; `release-gate.sh` testing-seam absence + production-name presence; `phase3-greps.sh` nonvacuous planted source/log checks; `phase3-structural.sh` Rust compile-fail plants; `phase3-version-gate.sh` older Rust binary rejects registry v2 with no writes. Sources: script headers; release seams at :149-198; old baseline `317722b` at version gate :35.

---

Inventory report (f)/(g) and requirement gaps. One correction to (e): `fixtures/claude/security-find.txt` is reusable reference output, but exact-name search found NO current Rust source/test consumer; do not claim its loader is actually invoked. The generic loader exists, but only `security-dump.txt` is named in `security_cli_tests.rs`.

## (f) Linux status and Remote Control

### Linux: landed process backend, not completed credential port
- `docs/plans/agctl-linux-support.md:24-29` records Phase 1 process/backend/build/test routing complete on measured hosts, hosted-CI half deferred; Phase 2 file-only credentials/commands/certification not started. `:41-62` explicitly says Phase 2 entry NO-GO and a successful compile is not a Linux-support declaration.
- Current source has target-selected `runtime/proc/{linux,macos}.rs`; unsupported other OSes get compile_error (`src/runtime/proc.rs:11-24`). Linux uses bounded /proc stat/status/comm observations and rereads identity around multi-file sampling (`src/runtime/proc/linux.rs:42-44,76-81,159-177,215-234`), with procfs-core as Linux-only dependency (`Cargo.toml:50-51`). Public process API remains shared (`src/runtime/proc.rs:93-128`).
- Exact `claude`, same real UID, not argv/environment/executable-name inference (`src/runtime/proc.rs:26-27,121-127`; Linux plan :213-249). Versioned identity includes boot UUID, PID namespace device/inode, start ticks, real UID; own-writer-gone requires valid same-domain record plus kill-zero ESRCH, not a zombie or start mismatch (`src/runtime/proc/linux.rs:262-287,343-375`).
- Linux negative/incomplete sweeps become Unreadable, never removal permission (`src/secret/backend.rs:20-32`). Peer lock removal is disabled independently (`:13-18`). `doctor --remove-stale` exits 1 with `stale lock removal unsupported on this platform: peer visibility is unproved`; `--yes`, absent paths and held-record-attested paths do not bypass it (`tests/e2e_linux_process.rs:80-158`). Existing records are not migrated on read; incompatible identity reports unknown, including Codex JSON lock=null + notes (`:160-226`). Fresh own locks/kernel flock are distinct and work (`:148-157`).
- Keychain boundary refuses Linux, regardless of AGCTL_KEYCHAIN_BACKEND/AGCTL_SECURITY_BIN (`src/secret/mod.rs:351-374`; `tests/e2e_linux_process.rs:229-257`). This is NOT the later real Linux file backend. The E2E explicitly expects unsupported live status/import and no fake-security invocation.
- Important later correction: the `.storage-write.lock` naming fix DID land after the Linux plan's initial prerequisite text. `docs/plans/agctl-storage-write-lock-name.md:4,22-32,531` records doctor-only legacy notice/refusal, synthetic-only macOS witnesses, six macOS gates, independent acceptance; Linux was NOT rerun. `docs/re-verify.md:288-290` repeats that caveat. So do not report the naming fix as still absent; do report that its evidence does not certify Linux Phase 2 or real vendor mutual exclusion.
- Remaining NO-GO reasons/scope: Linux file operations need the separately authorized implementation/certification and forced-refresh checkpoint (`Linux plan:41-50,465-488`); old process visibility cannot authorize peer-lock reclamation (`:284-300`). Proposed Linux Codex login is deliberately unsupported before child spawn because managed keyring policy defeated argv file-mode override (`:570-588`, `docs/re-verify.md:1214-1282`), not because Codex has no Linux keyring. This is a future-phase contract, not a claim every part is already implemented.

### --restart-remote-control: supervised tmux interaction, not process restart
- CLI option requires live pass (`--live` or `--undo`) and conflicts with `--yes`/`--forget` (`src/cli.rs:582-586`). macOS + terminal stdin AND stderr required; refusal occurs before store reads and returns exit 30 (`src/commands/use.rs:191-214`; `tests/e2e_remote_control_platform.rs:11-29`). Linux `tty_foreground` yields None and ancestor check fails closed (`src/runtime/proc/linux.rs:31-39`); Linux RC is explicitly excluded (`Linux plan:156-157`).
- Live-target session registry scan selects bridged sessions with validated pane IDs; namespace targets and catch-up do not act (`src/commands/use.rs:814-830,1618-1621`; `src/provider/claude/remote_control.rs:490-515`).
- Before swap: validate all candidates, ask fresh per-pane/per-input-group operator attestation, type `/remote-control Enter`, allow panel settle, then type `Up Up Enter` to select Disconnect. Read registry until each candidate is disconnected or gone; any failure prevents swap. (`remote_control.rs:267-274,401-465,570-726`; `src/runtime/tmux.rs:57-79`.)
- Captured screen is a rejection filter, NOT authorization. Require compatible current registry state, known foreground TTY, non-vim config, no copied/dead/synchronized pane, no draft/stash/mode/dialog ambiguity, then human y; reread registry to invalidate stale consent before send (`remote_control.rs:294-341,344-368,401-465`).
- After all swap locks drop: item+config applied => reconnect; unchanged/refused/failed paths => best-effort restore; unknown write or incomplete config => recovery instructions, no automatic reconnect; already-active => no action (`remote_control.rs:112-129,728-842`). Reconnect waits 25 s empirically, then `/remote-control Enter`, confirms a continuous bridge for 25 s. This proves bridge liveness only, not intended account or preserved history (`:56-59,67-68,761-805`).
- Bounds: disconnect stage 30 s, >=5 s viable remainder; each attestation up to 30 s within stage; 250 ms polling, 1 s quiet/panel settle; tmux calls 2 s, capture <=64 KiB (`remote_control.rs:38-65`; `tmux.rs:20-24,363-399`).
- Pin caveat: RC_MIN_VERSION AND RC_LAST_VERIFIED_VERSION are `(2,1,281)` (`remote_control.rs:62-65`). Older/malformed versions rejected (`:625-628`); newer ones warn once per distinct version, not strict ceiling (`:518-539`). The source explicitly calls LAST_VERIFIED provisional pending supervised operator check. `AGENTS.md:45-49` requires checklist + supervised check on upgrades before bump. Later synthetic echoed-prompt fixtures say 2.1.287; they do NOT certify that version. Do not describe 2.1.281 as a proof-backed universal version lock.
- Machine output contains only 11 integer RC counters, not session names/screens/pane paths (`remote_control.rs:70-95`; platform E2E :25-27). Fake tmux and 10 screens from (e) are the later-phase contract oracles, not real-vendor proof.

## (g) Rendering rules

### Column sets / layout
| Output | Ordered columns / format | Anchor |
|---|---|---|
| Claude status | Account, Org, Plan, 5h, Weekly, Fable (weekly), Credits, 5h reset, Weekly reset, State | src/render/table.rs:74-85 |
| Claude status --by-identity | Same, insert Kind after Plan; folded kind live+owned; JSON unaffected | table.rs:87-148; src/cli.rs:428-431 |
| Codex status | Account, Plan, Kind, 5h, Weekly, Credits, 5h reset, Weekly reset, State | table.rs:101-110 |
| Claude accounts list | Id, Account, Org, Kind, Source, State, Location | src/commands/accounts.rs:75-79,180-240 |
| Codex accounts list | NOT tabled: `id  kind  email` per line, optional `  (forgotten)`; no headings or width alignment; missing email '-' | src/commands/codex/accounts.rs:163-185 |

Status + Claude accounts list use tabled Style::psql (ASCII header separator and vertical |, no outer box), default global left alignment, one space horizontal cell padding, no horizontal/vertical trimming (`table.rs:266-275,337-346`; `accounts.rs:191-209`; pinned tabled source `/opt/local/rust/cargo/registry/src/index.crates.io-1949cf8c6b5b557f/tabled-0.22.0/src/tables/table.rs:873-888`). There is no terminal-width reflow logic in those renderers. Hidden rows omitted by default; one footer `N entry/entries hidden (--all)`; --all reveals rows and removes footer (`table.rs:199-205,222,349-353`). Empty Claude/status reports retain headings (table.rs:278-285); empty Codex accounts list instead prints no-accounts/no-accounts-shown guidance (:167-174).

### Reset alignment and continuation rows
- Two-pass per RESET COLUMN: collect all visible row reset slots, including continuations, find max natural length, render countdown at left and `(absolute)` at right with at least one space; never justify missing em dash/blank as a date (`table.rs:287-330,446-509`; `reset.rs:82-105`). Columns independent. `6d5h   (Mon 02:00 PM)` vs `19h32m (Wed 04:32 AM)` is asserted (`table_tests.rs:425-452`).
- Absolute formats: same local calendar day `4:15 PM`; different day less than seven elapsed days `Sun 02:00 PM`; >=seven days either direction `Sep 16 02:00 PM`. Month-day unpadded, prefixed clock hour padded; local-zone comparison, checked arithmetic (`reset.rs:53-69,164-197`). Past countdown is `now` (`:117-120`).
- Unknown/additional windows follow parent account as continuation rows, first cell two spaces + `↳ ` + window label; percentage goes in Weekly, reset in Weekly reset, 5h reset BLANK (not em dash); all identity/kind cells blank. Unknown kind name remains visible (`table.rs:237-250,310-316,386-415`).
- Missing normal values are em dash, percentages floored. Claude credits: unavailable n/a, disabled off, known used/limit `[amount] / [amount] (p%)`, unlimited `[amount] / Unlimited`, enabled without used amount em dash (`table.rs:410-444,524-526`).

### Color and Unicode
- No application `NO_COLOR` handling or `--no-color` option exists in the inspected source/CLI. Table code emits plain strings; watch UI applies no explicit color styles and uses default Ratatui widgets (`src/render/table.rs`, `src/tui/ui.rs:151-178`, `src/cli.rs`). IsTerminal checks elsewhere are prompt/RC guards, not table color switches. Don't invent color flags during parity work; Clap help coloring and diagnostic ANSI are separate.
- Table geometry uses terminal display width via pinned papergrid UnicodeWidthStr, not UTF-8 byte length (`Cargo.lock:2200-2203`; `/opt/local/rust/cargo/registry/src/index.crates.io-1949cf8c6b5b557f/papergrid-0.18.0/src/util/string.rs:17-48`). Ratatui text similarly uses UnicodeWidthStr (`.../ratatui-core-0.1.2/src/text/span.rs:272,379`; `.../src/text/line.rs:442,568-570`). Reset's internal justification counts Rust chars (Unicode scalars), safe for current ASCII-formatted countdown/date text (`reset.rs:100-104`, `table.rs:469-475`). A Go `len(s)` is NOT a correct general column-width replacement; rune count alone also is not display width.

### watch behavior
- Generic provider-neutral App<TuiRow>, header + top-aligned variable-height account blocks + help/hidden footer; no table layout. Gauge labels reserve 10 cells, gauge bars display p%; only measured windows earn gauges. Claude order 5h/weekly/Fable/credits, block height gauges+3; selected title begins ▸ (`src/render/row.rs:41-46,131-184`; `src/tui/ui.rs:47-67,119-178`).
- Header counts SHOWN rows, last-fetch age, fetching OR next-fetch countdown, stale indicator; footer `q quit · r refresh · ↑↓ select`, hidden hint names provider's status --all. Empty display says `no accounts to show` (`ui.rs:70-123`; row.rs:127-129`). Detail includes badge, full state, optional note and next reset (`row.rs:113-125,156-169`).
- `--interval` default 300 s, floor 60 s at parser and run boundary (`src/cli.rs:33,444-447`; `src/commands/watch.rs:135-147`). Scheduled passes may use 300 s cache; r forces no-cache (`watch.rs:262-265`). This floor is for configured scheduled cadence; manual r is not a 60 s throttle.
- First pass immediately due; one worker pass at a time; next scheduled after completion + interval; r during in-flight coalesces into a forced next pass (`watch.rs:356-417,430-435`). HTTP timeout 10 s; pass deadline interval−5 s (min valid 55 s), checked overflow (`:97-112,535-552`).
- Keys (press events only): q/Esc/Ctrl-C/Ctrl-D quit, r refresh, Up/k and Down/j selection; clamps at ends, no wrap. No mouse action; resize/mouse cause next loop redraw but are otherwise ignored (`watch.rs:299-333`; `src/tui/app.rs:156-191`). Current implementation does not scroll selection into view; extra blocks fall off/clipped to terminal (`ui.rs:126-140`). Do not claim mouse/scroll support merely from global TUI preferences.
- 250 ms key poll; worker uses try_recv, never joins; q cancels, drains <=250 ms, invokes emergency cleanup (`watch.rs:112-126,395-405,466-477`). Failed pass returns None and preserves rows, distinct from empty success (`:168-179,234-242`; App rows/stale state `app.rs:126-168`).
- Raw mode + alternate screen + hidden cursor restored on ordinary return, panic, and registered signal cleanup; logs buffered while frame active then flushed after restore (`src/tui/mod.rs:1-41,121-168,177-191`).

## Analyst gaps / recommendations
1. Freeze whether table parity means raw stdout bytes or Insta-normalized renderer output; use existing snapshots as normalized oracles plus separate exact-byte stdout tests, rather than blindly rename files. TUI parity should be a cell/frame contract, not identical terminal escape sequences across implementations.
2. Keep Linux and RC out of early-phase parity claim explicitly, while retaining unsupported-platform boundaries and catalogued fixtures. Do not implement unlanded Linux-plan prose as though it described current source.
3. Test schemas verbatim, provider-specific report versions, independent reset alignment including continuations, missing-vs-zero values, explicit clocks/zones, and watch cancellation/cache/scheduling separately.
4. Add explicit width cases (CJK, combining, emoji) to validate chosen Go width library against Rust dependencies; current snapshot corpus primarily exercises ASCII with a few symbols.
5. No runtime test results claimed by this lane. Remaining unknowns: exact raw stdout parity has not been executed; real hosted-CI status and supervised RC operator certification were not independently retrieved; full source scripts need Go-specific redesign, not port-by-regex. Large docs/scripts were inspected at referenced contract sections, not executed.

Read-only inventory is complete; ready for acceptance and shutdown.

---

