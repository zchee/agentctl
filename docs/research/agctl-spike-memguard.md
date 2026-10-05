# Spike verdict: memguard secret memory on macOS arm64

- Platform facts captured: 2026-10-05 18:01:40 JST (`date` in the command
  that captured them): macOS 27.2, darwin/arm64, go1.27.1.
- Dependency: `github.com/awnumar/memguard` v0.23.0;
  `golang.org/x/sys` v0.48.0.
- Implementation: `internal/secret/secret.go`, `purge.go`, `secret_budget.go`.
  Evidence: their sibling test files, including `TestLockedBufferBudget`.

## Verdict: GO, conditional on the startup budget check

Keep memguard. Before the first secret is sealed or opened, the executable
must call `secret.EnsureLockedMemoryBudget()` and refuse startup on error.
A finite soft `RLIMIT_MEMLOCK` below **64 system pages** returns a typed
`*errs.ConfigError`, classified as fatal exit status **1**. At 16 KiB pages
this requires **1,048,576 bytes (1 MiB)**. `RLIM_INFINITY` passes. Failure to
read the limit returns a fatal `*errs.IOError`. A warning is not sufficient.

The error names the observed limit, required bytes and pages, and a shell
remedy (`ulimit -l 1024` in KiB on the measured host); if the hard limit is
lower, an administrator must raise it before restarting. The check reads
only the soft limit and page size: it neither raises limits nor reserves
memory. Its boundary tests inject those values through an unexported pure
helper; they never lower the test process's limit.

This check prevents an already-known inadequate configuration from reaching
memguard's unsafe allocation-failure path. It does **not** prove that a later
allocation cannot fail, even with an unlimited limit. Other locked-memory
users, system resource exhaustion, larger payloads, or unbounded concurrent
opens remain outside the sizing assumptions below. `main.go` wiring is a
separate integration task; exporting the check alone does not satisfy this
condition.

## Measured numbers and threshold derivation

`go test -race -count=1 -v -run '^TestLockedBufferBudget$' ./internal/secret`
produced these observations on the host above:

| Measurement | Value |
|---|---|
| Soft and hard `RLIMIT_MEMLOCK` | `9223372036854775807` (`0x7fffffffffffffff`), unlimited on this host |
| System page size | 16,384 bytes |
| Intended simultaneous-open capacity, including margin | 16 token-sized buffers; all opened successfully |
| Default-limit headroom probe | 1,024 simultaneously open buffers, the probe cap; no failure observed |
| Constrained child's soft and hard limits | 262,144 bytes (256 KiB, 16 pages) |
| Constrained child's last successful open | **4** |
| First failing open in that child | **5**, during the transient session-key view allocation |
| Constrained child outcome in the recorded race run | `fatal error: all goroutines are asleep - deadlock!`, exit status 2 |

A token-sized plaintext occupies one locked inner page, but that is not the
whole process budget. Memguard also holds session-key buffers and allocates
transient key views. The exact observed four-buffer ceiling is an empirical
result, not a proof that each additional buffer always costs four pages.

The startup policy deliberately uses the empirical allowance of
`16 pages / 4 successful retained opens = 4 pages per open`, multiplied by
an **eight-buffer capacity floor** and a **2x overlap margin**:

```text
required bytes = system page size × 4 × 8 × 2
               = system page size × 64
16 KiB pages: 1,048,576 bytes
 4 KiB pages:   262,144 bytes
```

Eight is a conservative capacity floor, not an exact count of live tokens.
The frozen reference's credential operation owns incoming and displaced
access/refresh pairs, can build a keychain input line, and may temporarily
re-read an adopted occupant's pair. Its `StagedAdoption` holds paths and a
cleanup token, not another in-memory credential copy; its undo record holds
identity. Sealed values do not each imply an open plaintext buffer. The Go
`Equal` operation opens two buffers, and callbacks can overlap across
callers, so claiming "only one buffer at a time" would also be incorrect.

The 64-page floor budgets for at most 16 overlapping token-sized opens and
allows overhead rather than equating 64 pages with 64 open buffers. It is a
conservative policy derived from this measurement, not an allocation
reservation or a verified cross-platform upper bound. Integration must keep
concurrent opens bounded and revisit the budget for payloads larger than
one page, additional locked-memory consumers, or a different platform.
The 4 KiB boundary is tested arithmetically, not measured on another host.

## Why the failure cannot be recovered in-process

The constrained child printed successful opens 1 through 4, then stopped
inside its fifth `Enclave.Open`. Its stack shows:

```text
Coffer.View (holding the coffer mutex)
  -> NewBuffer(32) -> failed memory lock
  -> core.Panic -> core.Purge
  -> Coffer.Destroyed -> lock the same coffer mutex
```

The purge attempts to reacquire the non-reentrant mutex already held by the
same goroutine. `core.Panic` runs purge **before** invoking Go's built-in
`panic`, so an outer `recover` cannot execute: stack unwinding has not begun.
The background rekey goroutine is blocked on the same mutex. The recorded
race child terminated with the runtime deadlock diagnostic; an earlier
plain run hung and required termination. Neither is a recoverable error
return. The existing exhaustion probe runs only in a deadline-bounded child,
kills it on timeout, and logs its outcome. A passing parent measurement test
does not mean the child's exhaustion handling passed.

## API facts confirmed in the dependency's module-cache source

- `NewEnclave(src)` wipes the input and rejects empty data. `NewSecret`
  unconditionally wipes its input and returns an error for empty input.
- `Enclave.Open` can return a decryption error; allocation failure follows
  the fatal path above. Successful opens expose a frozen, read-only
  `LockedBuffer`. `WithPlaintext` destroys it on return, error and panic.
- A deliberately retained callback alias faults after destruction.
  `TestSecretPlaintextDestroyed` checks return, error and panic paths in
  isolated children. Callbacks must not retain or modify the bytes.
- Memguard itself decrypts through `secretbox.Open(nil, ...)` into a
  temporary Go-heap plaintext slice, then moves and wipes it. The wrapper
  therefore does **not** promise that plaintext never touches the Go heap.
- `Purge` destroys live buffers and session-key material, invalidating old
  secrets. Repeated purge is supported, and later new secrets can lazily
  obtain a fresh key. `TestPurge` exercises both facts. Trying to use old
  secrets after purge is not an application recovery strategy.
- Import initialization attempts to disable core dumps. First key use
  starts a background rekey goroutine with a 500 ms interval.

## Signal ownership and integration contract

Do not call `memguard.CatchInterrupt` or `memguard.CatchSignal`: their signal
reset/registration would interfere with the application's handlers, and
they exit with status 1 rather than the required 143/129/130. The exercised
seal/use/purge paths preserve SIGINT's default disposition in
`TestPackageKeepsDefaultSignalDisposition`; this behavioral test does not
by itself prove the absence of every possible signal-registration path.

The executable's owner must add both integrations, without using memguard's
signal handlers:

1. Call `EnsureLockedMemoryBudget` before any secret use; return fatal status
   1 on error, with the diagnostic intact.
2. Call `secret.Purge()` on normal and error exits, after the command and all
   plaintext users have finished, before `os.Exit`.
3. On signal exit, cancel and join plaintext users and complete child and
   emergency cleanup before purging and returning `128+signal`. Purging
   while a callback is still accessing its buffer can fault. Idempotence is
   not permission to race purge against an in-flight callback.

These changes intentionally do not edit `main.go`.

## Redaction and explicit plaintext export

`Secret` implements value-receiver `String`, `GoString`, `Format`,
`slog.LogValue`, `MarshalJSONTo`, `MarshalJSON`, `AppendText` and `MarshalText`,
all returning `[REDACTED]`. Tests cover the listed ordinary `fmt` verbs and
flags, slog text/JSON with nested groups, JSON value/pointer/interface
fields, and `errors.New(fmt.Sprint(secret))`. They do not establish that
arbitrary custom serializers are unable to override these hooks.

For json/v2's ordinary method selection, `MarshalJSONTo` precedes
`MarshalJSON`, and `AppendText` precedes `MarshalText`. Explicitly supplied
custom marshalers can override ordinary method dispatch.

`AppendPlaintextTo` deliberately copies bytes into a caller-owned slice for
credential documents; the caller must wipe that slice after use.
`WithPlaintext` also necessarily exposes its borrowed bytes to the callback.
Neither API can prevent a callback or downstream I/O library from making an
ordinary heap copy. Inputs assembled by callers, encoders, HTTP requests and
child pipes require their own lifetime and wiping discipline. The guarantee
is encrypted retained state, scoped locked callback access, and ordinary
redaction—not an absence of all transient plaintext copies.

Memory locking protects residency, not access by a debugger or privileged
process during an open window. Exit paths must not perform secret work after
purge; the dependency permits fresh key creation, and its old-secret
failure path does not immediately destroy every buffer it allocates.

## Follow-ups that could remove the startup-check condition

Neither alternative is implemented here; either requires fresh lifecycle
and exhaustion verification before changing this verdict:

- **Fix upstream memguard allocation-failure handling.** Eliminate the
  purge-under-coffer-lock deadlock and provide an allocation failure path
  that can cleanly report or terminate without hanging. This retains locked
  plaintext storage but requires an upstream change and dependency upgrade.
- **Implement an own-wiped-buffer fallback.** Use application-owned buffers
  with explicit wiping when locked allocation is unavailable, with an
  explicit policy for the weaker residency guarantee. This avoids reliance
  on memguard's failing allocation path but would need independent lifetime,
  copy, redaction and cleanup validation; it is not equivalent protection.
