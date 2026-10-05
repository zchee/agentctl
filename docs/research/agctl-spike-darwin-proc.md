# Darwin process observation over sysctl: field map and verdict

Written 2026-10-05 17:49:00 JST (from `date`). Measured on Darwin 27.2.0
(macOS arm64), go1.27.1, `golang.org/x/sys@v0.48.0`, against the Rust
reference frozen at `dbf6aeab84dfbe2465de496316d7bf6780940a60`.

## Verdict: GO

Every field the Rust libproc path (`src/runtime/proc/macos.rs`) reads for
same-UID `claude` observation and the own-writer-gone proof has a working
`unix.KinfoProc` equivalent via `unix.SysctlKinfoProcSlice("kern.proc.all")`
and `kern.proc.pid.<pid>`, with one pre-approved narrowing (`P_comm` 16
bytes versus `proc_name`'s 32-byte `pbi_name`; harmless for the 6-byte
exact target, proven by test). No field requires the `purego` + libproc
fallback. Implemented in `internal/runtime/proc` (commits `2645bec`
implementation, `92fc1bb` tests); `go test -race -count=1
./internal/runtime/...` passes under both build tags, `GOOS=linux go vet
./internal/runtime/...` passes, `golangci-lint run ./internal/runtime/...`
reports 0 issues.

## Field map

| Rust read (`proc_bsdinfo` via `proc_pidinfo`, `src/runtime/proc/macos.rs`) | Go read (`unix.KinfoProc` via sysctl `kern.proc.*`) | Tested by |
|---|---|---|
| `pbi_status` → holder state (`:91-119`) | `Proc.P_stat` (`int8`; SIDL=1, SRUN=2, SSLEEP=3, SSTOP=4, SZOMB=5 from `sys/proc.h`; x/sys does not export them, so the package defines them) | `TestHolderFromStatus`, `TestChildLifecycleIsObserved` (live SIGSTOP/zombie transitions) |
| `pbi_ppid` (`:120-140`) | `Eproc.Ppid` (`int32`) | `TestLookupOwnProcess`, `TestChildLifecycleIsObserved` |
| `pbi_pgid` (`:120-140`) | `Eproc.Pgid` (`int32`) | `TestLookupOwnProcess` (against `unix.Getpgrp()`) |
| `pbi_start_tvsec` / `pbi_start_tvusec` (`:161-187`) | `Proc.P_starttime` (`unix.Timeval`: `Sec int64`, `Usec int32`; microsecond resolution confirmed live) | `TestStartFrom`, `TestLookupOwnProcess`, `TestChildLifecycleIsObserved` (child start within 1 s of spawn) |
| `pbi_uid` (effective UID) | `Eproc.Ucred.Uid` (`uint32`) | `TestLookupOwnProcess`, `TestLookupAnotherUsersProcess` |
| ownership comparison: `pbi_uid` vs caller `getuid()` (`:242-250`) | `Eproc.Pcred.P_ruid` vs `os.Getuid()` — real vs real, the plan's directed correction of Rust's effective-vs-real comparison | `TestSameUserNamedMatchesExactly`, `TestSameUserNamedExcludesAnotherUsersProcess`, `TestLookupAnotherUsersProcess` |
| `proc_name`: `pbi_name` (32 bytes) with `pbi_comm` (16) fallback (`:454-463`) | `Proc.P_comm` only (`[17]byte`, 16 bytes + NUL) — see the limit below | `TestCommandName`, `TestLongNameTruncatesToSixteenBytes`, `TestSameUserNamedMatchesExactly` |
| `proc_listpids(PROC_ALL_PIDS)` (`:336-375`) | `unix.SysctlKinfoProcSlice("kern.proc.all")`; records with `P_pid <= 0` are padding and dropped | `TestListContainsSelfAndInit` |
| `kill(pid, 0)` existence probe (`:69-76`) | not needed as a separate call: `kern.proc.pid.<pid>` answers a missing process with an empty record (see below) | `TestLookupGoneOrImpossiblePIDs`, `TestChildLifecycleIsObserved` (reaped child) |
| `e_tpgid`, `e_tdev` (`tty_foreground`, `:116-124`; a later consumer, not part of this step) | `Eproc.Tpgid`, `Eproc.Tdev` (both `int32`, present in x/sys) | not yet consumed; mapped, no gap |

The own-writer-gone proof (`src/runtime/proc.rs:140-153`) is reproduced as
`WriterGone`: dead (gone or zombie) proves gone; a live process with a
start identity differing from the recorded one proves the pid recycled;
an empty recorded identity, an unreadable record, or an unrenderable
current start all fail closed to false.

## The `P_comm` limit and `claude` matching

The kernel's accounting record carries at most 16 bytes of command name
(`MAXCOMLEN`); `proc_name`'s 32-byte `pbi_name` has **no sysctl
equivalent**. The exact, case-sensitive match target is `claude` (6
bytes), which fits with 10 bytes to spare, so the narrowing cannot cause
a false negative for the name that matters. The consequence for longer
names is a documented non-match, not a crash: an 18-byte executable name
is observed as its first 16 bytes, which equals neither the full name nor
`claude` (`TestLongNameTruncatesToSixteenBytes`). Names that merely
contain the target (`Claude Helper`, `claude-code`) never match
(`TestSameUserNamedMatchesExactly`), preserving the Rust matcher's two
bans on substring and command-line matching.

## Behavioural differences between sysctl and libproc (all measured live)

- **Other users' records are readable.** `kern.proc.pid.1` returns
  launchd's full record (ruid 0, euid 0, start time included), where
  `proc_pidinfo` answers `EPERM`. The Rust sweep's
  `Refused`/`signalable`/`Unclassified` machinery exists to tell "another
  user's" from "sandbox-blinded" per process; on the sysctl path that
  distinction has no per-process form. A sandbox that denies the read
  denies the **whole** `kern.proc.all` call, so the failure surfaces as a
  listing error ("I do not know"), never as a silently shorter list — the
  false negative that machinery guards against cannot occur silently
  here.
- **Zombies stay visible.** A killed, uncollected child keeps a record
  with `P_stat = 5` (SZOMB → dead), measured directly. No separate
  `kill(pid, 0)` probe is needed to see it.
- **A missing process is an empty answer, not an error.**
  `unix.SysctlKinfoProc` surfaces it as `EIO` (its size check);
  `unix.SysctlRaw("kern.proc.pid", pid)` disambiguates: 0 bytes means no
  such process, a non-empty wrong-sized answer means the running kernel's
  record layout is not the one the build expects (648 bytes,
  `unix.SizeofKinfoProc`), which is reported as an error rather than
  trusted, because a mismatched layout would misread every process alike.
- **UID comparison basis.** Rust compares the target's *effective* UID
  (`pbi_uid`) against the caller's *real* UID (`getuid()`); the plan
  directs real-vs-real (`Eproc.Pcred.P_ruid` vs `os.Getuid()`), so a
  setuid peer is attributed to whoever started it. Both fields are
  carried on the record, so consumers can revisit the choice without a
  new kernel read.

## Start identity rendering

Rendered as RFC 3339 UTC with microseconds, trailing zeros trimmed, and
no fraction for a whole second — byte-compatible with the Rust rendering
(the Rust unit expectation `2025-09-09T06:40:00.123456Z` for
`(1_757_400_000, 123_456)` is asserted verbatim in `TestStartFrom`). The
string is compared, never parsed; one microsecond of difference is a
different identity. Where Rust relies on checked arithmetic to refuse
implausible kernel values, the Go rendering refuses a negative second, a
second past 9999-12-31T23:59:59Z, or microseconds outside [0, 999999],
yielding an empty identity that no caller may treat as a match — both
sides refuse the same garbage instead of wrapping it into a plausible
timestamp.

## Open items

None blocking. `Eproc.Tpgid`/`Eproc.Tdev` are mapped but unconsumed until
the terminal-foreground work needs them.
