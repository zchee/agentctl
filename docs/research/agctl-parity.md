# Release binary parity

## Verdict

**Reachable checks PASS; full release parity sign-off is incomplete.**

The installed `scripts/parity-signoff.sh` run measured at
2026-10-06 10:27:16 JST returned exit 0 with 137 PASS checks, 0 FAIL checks,
and 22 explicit SKIPs. The untagged release build is an additional PASS
line, not included in that counter. Fifteen commands ran against each
binary. Every compared file had an empty normalized byte diff.

An exit 0 proves only the checks that ran. It does not waive skipped
Claude status, credential writes, or HTTP comparisons. No testing tag was
used to make the Go release binary accept fixture overrides.

The manual run of 2026-10-09 (section "Manual run") executed the skipped
keychain commands against a real keychain for the Go artifact only: six of
the seven commands exit 0, the undo refuses by the rule both
implementations share, and real usage success is observed for Claude and
Codex. The reference side of those commands was not run, so the sign-off
stays incomplete on the parity axis while the Go keychain path now has
real-environment evidence.

## Reproduction and binary identities

Run from the Go module root:

```sh
GOTOOLCHAIN=go1.27.1 /absolute/path/to/agentctl/scripts/parity-signoff.sh
AGCTL_BIN=/absolute/path/to/agctl scripts/parity-signoff.sh
```

Required tools: bash, Go, jq, perl, diff, cmp, git, and shasum. `TMPDIR`
selects the retained evidence directory's parent. The reference override
must be an absolute executable path. The script never builds the Rust
project; selecting a Rust release build instead of the supplied debug
binary is the operator's choice.

| Binary | Identity |
|---|---|
| Reference | `/Users/zchee/rust/src/github.com/zchee/agctl/target/debug/agctl`, `agctl 0.1.0` |
| Reference source checkout | `dbf6aeab84dfbe2465de496316d7bf6780940a60` |
| Reference executable SHA-256 | `dd14f021b21d696e90d62e7ef153e0ec10b8d8a9ff8647221896cc607644d736` |
| Go source at build | `b059a4da0c022b5414bb1e0b395f79637606951a` |
| Go build | `GOTOOLCHAIN=go1.27.1 go build -trimpath -ldflags='-s -w'`, untagged |
| Go executable SHA-256 | `a7400c3d559b3023f7c31d59aab2688ab54fedff827d23622c44d20907b249be` |
| Go version output | `agentctl version (devel)` |

Executable hashes and version strings were measured at
2026-10-06 10:28:44 JST. A checkout commit identifies the reference source,
not proof of how its pre-existing executable was built; the executable
hash pins the actual artifact compared.

## Store setup and release boundary

Each implementation receives one fresh temporary store, HOME, source
Codex home, and fake-bin directory. Their trees are alternately moved to
the same absolute `active` path before commands run. This preserves stored
namespace spellings and their Claude hash without normalizing paths or
rewriting either implementation's registry between commands.

The seed registry contains one owned Claude account and one owned Codex
account. Synthetic Codex grants are derived from
`fixtures/codex/auth-codex-format.json`; their JWT expiration is set to 1.
An imported source home uses file storage and a distinct account. Before
status, the script verifies that all remaining owned Codex accounts have
`refresh=never`. There is no vendor request or loopback endpoint exercise.
The first command changes the seeded owned account from automatic refresh
to `never`; no status is run until that command succeeds.

Both trees install copies of `fixtures/fake-security.sh` and
`fixtures/fake-codex.sh` through PATH, plus a no-op Claude executable for
isolated use. The command environment is cleared with `env -i` and sets
only HOME, USER, LOGNAME, PATH, LANG, TMPDIR, CODEX_HOME, and `--config-dir`.
The fake keychain does **not** intercept the release reader: it pins
`/usr/bin/security`, and `AGENTCTL_SECURITY_BIN` is testing-only. The
script does not set it. Login and keychain-dependent commands are skipped
instead of touching the operator's keychain. The fake Codex executable is
available through production PATH lookup but is not invoked by the
reachable sequence; Codex login needs an unavailable keychain seam.

Release boundaries are defined by
`internal/secret/keychain_backend_release.go`,
`internal/provider/codex/login_child_release.go`,
`internal/provider/codex/oauth_endpoint_release.go`,
`internal/provider/codex/usage_endpoint_release.go`, and
`internal/provider/claude/oauth_endpoint_release.go`. The corresponding
Rust keychain boundary is `src/secret/mod.rs:79-86,334-348`:
`AGCTL_SECURITY_BIN` exists only with the testing feature and the normal
reader uses the absolute system binary. Testing endpoints are similarly
feature-gated, for example `src/provider/codex/usage.rs:138-148`.

## Compared observations and normalization

After **each** command, both implementations' exit codes are checked
against the expected code. The script captures the registry, Codex status
v2 document and exit code, both audit logs, isolated Claude session seed,
stdout, and stderr. Status documents must have `version=2` and an array of
rows; exit 0 or 2 is permitted, and the two exits must match. This fixture
reports exit 2 on every status pass because grants are expired.

The six byte comparisons per command are:

| Capture | Normalization | Why |
|---|---|---|
| `store/config.json` | Every string-valued `created_at` becomes `<clock>` | Reference import records its wall clock; invocations do not share an instant |
| Codex `status --json --all` | String-valued `generated_at` and `fetched_at` become `<clock>`; standalone `agctl` becomes `agentctl` | Pass/cache clocks and the deliberate executable rename |
| `store/codex/writes.jsonl` | String-valued `ts` becomes `<clock>`; numeric `agctl_pid` becomes `0` | Audit timestamp and writer PID are invocation-specific |
| `store/claude/keychain-writes.jsonl` | None | No keychain write is reachable; both files are absent |
| Isolated session `.claude.json` | None | The same home seed and isolated account produce the same bytes |
| Command stdout | Standalone `agctl` becomes `agentctl` | Deliberate executable rename |

Normalization uses lexical Perl replacements, not JSON reserialization.
It retains member order, whitespace, number spellings, trailing newlines,
identity values, paths, and all non-clock audit fields. No PID other than
`agctl_pid` is changed. Absent files receive the same `<absent>` sentinel;
absence equality does not prove a write occurred. Stderr is retained for
diagnosis but not asserted byte-equal. Each command also compares status
exit files with `cmp`.

The owned removal additionally proves the auth file is absent and the
Codex audit contains `outcome=delete` and the owned user identity on both
sides. This prevents an absent-log comparison from substituting for a
successful credential deletion.

## Executed sequence

In the commands below, `$owned` is
`owned-user/22222222-3333-4444-8555-666666666666`, `$imported` is
`user-0001/11111111-2222-4333-8444-555555555555`, and `$source` is the
shared active source-home path. Every row includes all six byte
comparisons above and the subsequent status exit comparison.

| Command | Reference / Go exit | Registry | Status v2 | Audits, session, stdout | Claude status v1 |
|---|---|---|---|---|---|
| `codex accounts set "$owned" --refresh never` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `claude use claude-account --yes --json` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `claude use --forget claude-account --yes` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `claude use --undo --yes` with no swap history | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `claude accounts forget Claude Code-credentials-deadbeef` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `claude accounts unforget Claude Code-credentials-deadbeef` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `codex import --from codex-home --codex-home "$source"` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| Same import again | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `codex accounts set "$imported" --refresh never` | 2 / 2, read-only refusal | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `codex accounts forget "$owned"` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `codex accounts unforget "$owned"` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `codex accounts forget "$imported"` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `codex accounts unforget "$imported"` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |
| `codex accounts remove "$owned" --delete-secret --yes` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS, deletion witnessed | SKIP |
| `codex accounts remove "$imported"` | 0 / 0 | PASS | PASS, exit 2 / 2 | PASS | SKIP |

The service name containing a space is one quoted argument in the script.
The history-free undo is not evidence of successful live or namespace
reversal; those branches remain skipped.

## Skipped comparisons

| Comparison | Count | Reason |
|---|---|---|
| Claude status v1 after each command | 15 | Release reader cannot use the fake keychain; no system-keychain status is run |
| `claude login` | 1 | Fake keychain cannot capture the release writer; login also needs browser/network interaction |
| `claude import` | 1 | Release keychain enumeration cannot be redirected |
| `claude use --live` | 1 | Credential reads and writes cannot be redirected |
| `claude use --undo` actual reversal | 1 | No safely-created credential swap history exists |
| `claude doctor` | 1 | Release credential inspection cannot be redirected |
| `codex login` | 1 | Login install confirmation inspects the system keychain; fake Codex alone is insufficient |
| Usage/refresh endpoints | 1 | Release endpoint overrides are compiled out; no vendor request is made |

These 22 SKIPs are explicit in the script output. A separate tagged
fixture comparison could test these paths, but it would not be parity
against the shipped untagged Go artifact. A real-keychain manual run in a
disposable environment is a separate operator-approved check, not silently
performed here.

## Manual run (2026-10-09, Go implementation only)

The keychain steps that the automated sign-off skips were executed once
against a real macOS keychain in a disposable tart guest, for the Go
implementation only. The reference half of the comparison was not run:
the reference executable the scripts default to is a `testing`-feature
build whose keychain reader is disabled by construction, its guest
sequence stopped at `claude import --from keychain` with exit 1 and the
message `the keychain is not available (disabled)`, and the operator
chose not to repeat the sequence with a release build of the reference.
This section therefore records real-keychain evidence for the Go
artifact, not parity.

| Item | Value |
|---|---|
| Host work directory | `.omc/artifacts/parity-vm/2026-10-09_21-25-32-operator` (untracked; raw captures stay there and in the guest) |
| Guest image | `macos-golden-gate-xcode`; macOS 27.0 (26A428); Claude Code 2.1.267; codex-cli 0.155.1; jq 1.8.2 |
| Base snapshot | `agentctl-e2e-base`, measured 2026-10-09 22:43:31 JST after one GUI Claude Code login with the single disposable account |
| Go artifact | `GOTOOLCHAIN=go1.27.2 go build -trimpath -ldflags='-s -w'`, untagged, module `v0.0.0-20261009123533-909acd21ce70` (commit `909acd2`), SHA-256 `bfb2eeaba0e20791a725452dafb4da5cb6974f433c82f385b25dbe89b1b0b197` |
| Reference artifact copied | SHA-256 `dd14f021b21d696e90d62e7ef153e0ec10b8d8a9ff8647221896cc607644d736` (the same debug executable as the automated run; sequence aborted at import, see above) |
| Guest sequence | `scripts/parity-vm-guest.sh go` at the state of commit `c692a46`, copied into the clone `agentctl-parity-go` after `reset go` |
| Comparison | `scripts/parity-vm.sh compare` at the state of commit `6d114a4`, rerun on the host at 2026-10-09 23:18:24 JST |

Per-command results of the Go sequence (the status columns are the exit
codes of `claude status --json --all` and `codex status --json --all`
captured after each command):

| Command | Exit | Claude status | Codex status | Recorded fact |
|---|---|---|---|---|
| `claude login --manual --label parity` | 0 | 0 | 2 | one owned row for the disposable account |
| `claude import --from keychain` | 0 | 0 | 2 | the keychain item is the same account; no row added |
| `claude use <account> --live --yes --json` | 0 | 0 | 2 | `applied`, digest `449dedb2` (GUI login item) replaced by `66c57eb2` (the manual login's newer grant) |
| `claude use --undo --yes --json` | 1 | 0 | 2 | refused: the displaced grant was discarded, nothing to put back |
| `claude doctor` | 0 | 0 | 2 | keychain unlocked, one live and one owned Claude row, both `ok` |
| `codex login --no-refresh --label parity` | 0 | 0 | 2 | one owned Codex row |
| `codex accounts set <account> --refresh auto` | 0 | 0 | 2 | owned Codex row `ok`; the guest's own Codex CLI stays `needs_login` (live row), which is the partial exit 2 |

Comparison output restricted to the Go side: 9 PASS (six commands, the
completed guest sequence, Claude and Codex real usage success), 2 RECORDED
(the applied forward swap and the refused undo), 0 FAIL. The 74 FAIL
lines of the full output are the absent reference captures.

Two facts about the single-account design surfaced during the run and
were folded into the scripts rather than treated as defects of either
implementation:

- A forward swap onto the account's own newer grant is `applied`, not
  `already_active`, in both implementations (Go
  `internal/commands/use_live_engine.go`, reference `src/commands/use.rs`:
  the item's expiry must be at least the incoming credential's for the
  short circuit), and both discard the displaced older grant (Go
  `internal/provider/claude/adopt.go`, reference
  `src/provider/claude/adopt.rs`), so the undo that follows has nothing
  to put back and refuses with exit 1. A reversal test needs two
  disposable accounts; this run records the refusal (`c37f763`,
  `c692a46`, `b395fc2`).
- The guest never logs the vendor Codex CLI in, so the final Codex status
  is the partial exit 2 with the live row at `needs_login`; the owned row
  is the one whose real usage success the run observes (`6d114a4`).

Still not run: the reference half of these seven commands, a forward/undo
pair between distinct accounts, and refresh coverage (deferred until a
disposable grant legitimately expires).

## Manual follow-up to complete the skipped coverage

This follow-up was **not run**. It requires explicit operator approval,
disposable macOS user/keychain environments, test vendor accounts, real
Claude/Codex executables, and browser interaction. A different HOME alone
is not a separate keychain. Never run it against a daily-use account.

Restore the same disposable starting state separately for the reference
and Go executable. Choose `$bin` as one absolute executable path and
`$store` as its fresh store. Run the following sequence for each, resolving
`$claude_id` and `$codex_id` from that implementation's registry after
login/import, and recording exit codes at every command:

```sh
"$bin" --config-dir "$store" claude login --manual --label parity
"$bin" --config-dir "$store" claude import --from keychain
"$bin" --config-dir "$store" claude use "$claude_id" --live --yes --json
"$bin" --config-dir "$store" claude use --undo --yes --json
"$bin" --config-dir "$store" claude doctor
"$bin" --config-dir "$store" codex login --no-refresh --label parity
"$bin" --config-dir "$store" codex accounts set "$codex_id" --refresh auto
```

After each command capture `config.json`, both audit logs, credential
file presence and modes, and both `claude status --json --all` and
`codex status --json --all`. Run a nontrivial forward/undo pair between
distinct disposable accounts; an already-active outcome is not a reversal
test. Reuse only the documented clock/PID/name normalization, and record
all other differences rather than widening normalization. Keep raw audit
and credential evidence local; do not paste secrets into the report.

The final status must observe real usage success. To cover a refresh,
wait for a legitimately expired disposable grant, then run Codex status
again and compare grant replacement and refresh marker settlement.
Separately-authorized Claude credential refresh coverage likewise needs
an expired disposable grant. Do not replay ambiguous refresh tokens merely
to manufacture a test. Vendor-generated identities, grants, usage counters,
and audit digests can differ between independently-authorized sessions;
record that limitation instead of calling their byte differences parity
failures or normalizing them away. Thus live checks alone do not replace
controlled tagged endpoint fixtures for identical response assertions.

## Recorded divergences

No unexplained divergence appeared in the reachable byte comparisons.
The following previously recorded differences are not converted into
PASS claims for skipped scenarios:

| Difference | Reference evidence | Treatment |
|---|---|---|
| User-facing executable is `agentctl`, not `agctl` | `src/cli.rs:340`, `src/provider/codex/account.rs:381-382` | Normalize standalone name only in stdout and status; on-disk `agctl_pid` and compatibility spellings remain unchanged |
| Refresh resend question omits `(risk R63)` | `src/commands/codex/accounts_refresh.rs:246-249` includes the parenthetical | Deliberate omission of an internal identifier; retain the warning about revoking the grant; not reached here |
| Vendor number tokens are preserved verbatim, rather than canonicalized through f64 | `src/provider/codex/usage.rs:409-410,538-541` parses into serde_json Value and retains the object; `Cargo.toml:34` enables preserve_order/float_roundtrip | The prior binary comparison observed `12.5000` / `1.2500e+2` canonicalization in Rust; Go retains them. This script never normalizes numbers and does not exercise vendor usage responses |
| Signal during Codex login removes the auth-free scratch leaf in Go | `tests/e2e_codex_login.rs:1080-1087` requires auth removal but leaves a directory because the signal path never unwinds | Stronger cleanup, not a requirement to recreate residue; login signal path not reached here |
| Claude config rewrite checks full bytes rather than a SHA-256 equality gate | `src/provider/claude/claude_json.rs:891,940` rewrite recheck | Stronger recheck; no live rewrite reached here |
| Adopted-store cleanup retains a directory descriptor and rechecks its inode | `src/secret/file_store.rs:708-717` adopted commit lifecycle | Go refuses a replaced namespace instead of writing into a replacement; no adopted write reached here |
| Rust destructor cleanup becomes explicit deferred discard in Go | `src/secret/file_store.rs:626-635` staged adoption destructor | Ownership-language translation with emergency cleanup; no staged adoption reached here |
| Refresh marker digest fields are validated as eight lowercase hex characters in Go | `src/provider/codex/auth_store.rs:1204-1214,1254` serde marker types | Invalid-input fail-closed tightening; malformed markers not reached here |
| Tagged crash fixture exits 134 rather than receiving SIGABRT | `src/provider/codex/refresh.rs:728,1000` testing abort seams | Testing-only translation; excluded from the release binary comparison |

The reference holds the Codex namespace lock across a refresh POST; Go
matches it. The login child's refusal exits 2 rather than forwarding its
exit 17; this also matches the reference. Neither is a production parity
exception. Previously fixed wire defects (Codex status final LF, raw email
swap-removal ordering, and child-exit refusal punctuation) remain
requirements, not approved normalizations. The byte comparison preserves
line endings and order, while the raw vendor and child-exit paths are not
exercised by this sequence.

## Retained evidence

Final run output:

`/private/tmp/claude-501/-Users-zchee-go-src-github-com-zchee-agentctl/1e896178-a09d-4440-9b39-a03a830515b8/scratchpad/parity-installed.log`

Captures, raw files, normalized files, per-command exits, stderr, empty
diffs, stores, and the actual Go release binary:

`/private/tmp/claude-501/-Users-zchee-go-src-github-com-zchee-agentctl/1e896178-a09d-4440-9b39-a03a830515b8/scratchpad/agentctl-parity.Ka3fk4/`

Shell validation: `shellcheck scripts/parity-signoff.sh` and
`bash -n scripts/parity-signoff.sh` both exit 0. The script exits nonzero on
any FAIL. A negative control using `AGCTL_BIN=/usr/bin/false` returned
exit 1 with 60 PASS checks, 107 FAIL checks, and 22 SKIPs. It produces
command, registry, missing-status, stdout, and audit failures instead of
passing an empty or invalid status document. Its full output is retained
as `parity-installed-negative.log` in the same scratchpad.
