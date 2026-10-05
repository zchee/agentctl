# Spike verdict: memguard secret memory on macOS arm64

- Captured: 2026-10-05 18:01:40 JST (`date` in the command that captured the
  platform facts below)
- Platform: macOS 27.2, darwin/arm64, go1.27.1, `github.com/awnumar/memguard`
  v0.23.0 (`golang.org/x/sys` v0.48.0)
- Code: `internal/secret/secret.go`, `internal/secret/purge.go`; evidence in
  `internal/secret/secret_test.go`, `internal/secret/purge_test.go`
  (`TestLockedBufferBudget` prints the measurements with `t.Logf`)

## Verdict: GO, conditional on the mlock budget

The enclave model holds on this platform: tokens stay encrypted at rest in
process memory, plaintext exists only inside a guarded, mlocked, read-only
buffer opened around one operation and destroyed on return, error and panic
alike, and every formatting and serialization hook Go 1.27 consults returns
`[REDACTED]`. On the default macOS limits the mlock budget is a non-issue at
the swap path's scale.

The condition: memguard has **no recoverable path** when `mlock` fails — it
panics through a purge that has been observed to deadlock (measured below:
under a 256 KiB limit the fifth simultaneous open fails, and the process
dies messily or hangs). The design must therefore (a) keep the number of
simultaneously open plaintext buffers small — the swap path needs 8, one at
a time in practice — and (b) treat a finite, small `RLIMIT_MEMLOCK` as a
fail-fast startup condition, not a runtime branch: a `Getrlimit` check at
process start that refuses to proceed (or at minimum warns) when the limit
is finite and below a safety floor (1 MiB covers 64 open buffers at 16 KiB
pages, an order of magnitude above need). Whether that check ships, and
whether its failure is fatal or a warning, is an acceptance decision raised
with the lead; the alternative reading of these numbers is a NO-GO until
exhaustion handling is replaced or patched upstream. This document's
recommendation is the conditional GO with the startup check, because the
deadlock is unreachable under the platform's default (unlimited) limit and
the check turns the one dangerous configuration into a clean refusal.

## Measured numbers

From `TestLockedBufferBudget` on this machine (fresh run, `-race`):

| Measurement | Value |
|---|---|
| `RLIMIT_MEMLOCK` (cur and max, `unix.Getrlimit`) | `0x7fffffffffffffff` — unlimited |
| Page size (`unix.Getpagesize`, matches `sysctl hw.pagesize`) | 16384 bytes (16 KiB) |
| Locked bytes per open buffer (token-sized payload) | one 16 KiB inner page (guard pages are mapped but not mlocked) |
| Simultaneous buffers the swap path needs | 8 (incoming access+refresh, displaced access+refresh, keychain stdin line, adopted-copy occupant access+refresh, staged reversal copy), tested at 16 for a 2x margin |
| Buffers opened simultaneously without failure | 1024 (probe cap; no failure observed) |
| First N at which opening fails, default limits | not reached at cap 1024 (~16 MiB locked) |
| First N at which opening fails, `RLIMIT_MEMLOCK` lowered to 256 KiB in a child | **5** — four buffers opened, the fifth open failed (race-instrumented child; the limit's 16 pages are shared with memguard's session-key buffers and the per-open transient key view, so the ceiling is below the naive 16-page count) |

## Failure mode under memlock exhaustion

When `mlock` fails, `memguard` does not return an error: `core.NewBuffer`
calls `core.Panic`, which runs a session purge and then panics. Observed
with `RLIMIT_MEMLOCK` lowered to 256 KiB, where the fifth simultaneous open
fails (per-open progress printed unbuffered by the child proves the count):

- under `-race`, the child process died with a goroutine dump (exit status 2);
- in a plain build, the purge-under-panic path **deadlocked** on the session
  key's mutex and the process had to be killed (the upstream repository ships
  `examples/deadlock/` reproducing this class of hang).

Consequence: an mlock failure is a process-fatal event, not a branch, and on
the hang path not even a clean fatal one — which is why the verdict above
asks for the fail-fast `Getrlimit` startup check instead of claiming sizing
alone suffices. Sizing bounds the demand (8 buffers x 16 KiB = 128 KiB, one
at a time in practice); the check refuses the configurations in which that
demand could still cross a finite limit. The constrained probe in the test
is deadline-bounded, kills its child on timeout, and logs rather than
asserts, so the suite cannot hang on this path.

## API facts confirmed with the vendored source

- `memguard.NewEnclave(src)` wipes `src` after sealing and returns nil for
  empty input; `NewSecret` therefore wipes unconditionally and rejects empty
  input before sealing (an audit-grade secret of length zero is a caller bug).
- `Enclave.Open` returns an error only for decryption failure (the session
  key was purged); allocation failures panic as above. `Open` also opens one
  transient 32-byte key-view buffer per call, so each `WithPlaintext` briefly
  locks two buffers.
- `LockedBuffer`s returned by `Open` are frozen (read-only pages): a callback
  that writes into the slice faults. Callbacks treat the plaintext as
  read-only; the keychain line builder appends into its own buffer.
- `memguard.Purge` destroys every live buffer and rotates the session key, so
  it invalidates existing `Secret`s process-wide, and a second call is a
  no-op. Verified by `TestPurge`.
- After the callback the plaintext mapping is unmapped: a retained alias
  faults on access (`TestSecretPlaintextDestroyed` proves this in a child
  process for return, error and panic exits).
- Importing memguard disables core dumps at init (`memcall.DisableCoreDumps`),
  which is desirable here and costs nothing.
- The session key is re-keyed every 500 ms by a background goroutine the
  library starts on first use; it is invisible to callers but shows up in
  goroutine dumps.

## Signals: CatchInterrupt/CatchSignal are not used

`memguard.CatchSignal` calls `signal.Reset()` before `signal.Notify`, which
would silently remove the program's own handlers, and its handler exits with
status 1 — both incompatible with the exit-status contract (143/129/130).
Neither function is called anywhere in this module.
`TestPackageKeepsDefaultSignalDisposition` proves it behaviorally: a child
process that seals, uses and purges a secret still dies **from SIGINT**
(`WaitStatus.Signaled()`, not an exit code), so the default disposition
survived the import. Review rule: `memguard.Catch` must never appear outside
this document.

## Purge wiring the lead must add (deferred to the signal/exit integration)

`secret.Purge()` must run on every exit path, after the last secret use and
before the process exits:

1. **Normal exit**: at the end of the run function, after the command tree
   returns and before the exit status is returned.
2. **Error exit**: the same site covers it when every exit flows through one
   return path; any later `os.Exit` call site added elsewhere must purge
   first.
3. **Signal exit**: in the signal goroutine, after child teardown and
   emergency cleanup, immediately before the `128+signal` exit. Purge is
   idempotent, so the signal path and the normal path may both run it.

4. **Startup budget check** (the verdict's condition): read
   `RLIMIT_MEMLOCK` with `unix.Getrlimit` during bring-up and refuse to
   proceed — or at minimum warn — when the limit is finite and below the
   1 MiB floor, because an mlock failure later is a panic that can deadlock,
   not an error a command can report.

Not wired by this change: `main.go` is owned elsewhere; the only exported
surface is `secret.Purge`.

## json/v2 hook findings (go1.27.1)

`encoding/json/v2.Marshal` consults, in precedence order: `MarshalJSONTo`
(`json.MarshalerTo`), `MarshalJSON` (`json.Marshaler`), `AppendText`
(`encoding.TextAppender`), `MarshalText` (`encoding.TextMarshaler`). `Secret`
implements **all four** on value receivers — plus `String`, `GoString`,
`Format` and `slog.LogValue` — so by-value fields, pointer fields, `any`
values, map keys and text contexts all render `[REDACTED]`. Verified through
`fmt` (every verb, flag, width and precision), slog text and JSON handlers
(top level and inside groups), `json.Marshal` of value and pointer fields,
and `errors.New(fmt.Sprint(secret))`.

The one deliberate exposure path is `AppendPlaintextTo`, inside
`WithPlaintext`, for credential documents that must carry the token to their
store. It is the only place plaintext leaves the enclave; its godoc says so
and tells the caller to wipe the returned slice after the I/O.

## Documented limits (outside the guarantee)

- **Copies made by other packages.** Bytes handed to `net/http`, an encoder,
  or a child process pipe during the `WithPlaintext` window are ordinary GC
  memory: the runtime may copy them, and they are wiped only where the
  consumer wipes them. The guarantee covers the at-rest representation, not
  the I/O call's transient copies.
- **GC-managed intermediates.** The input to `NewSecret` and the output of
  `AppendPlaintextTo` are wiped explicitly, but any intermediate the caller
  built before sealing (string concatenation, JSON decoding) is unmanaged.
  Callers must seal as early as possible and decode directly into buffers
  they wipe.
- **A purged process keeps running.** `Purge` makes old secrets undecryptable
  but does not stop code from sealing new ones; exit paths must not do secret
  work after their purge.
- **mlock covers residency, not ptrace.** A same-UID debugger can still read
  the plaintext during the open window; the threat model is swap, core dumps
  and accidental serialization, not a hostile root.
