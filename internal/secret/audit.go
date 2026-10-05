// Copyright 2026 The agentctl Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package secret

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

// AuditLogFile is the append-only audit log's file name, under the
// namespace root.
//
// A line in this file is the only evidence a crashed swap leaves behind: a
// keychain write has no rename to undo, and a lock artefact is a directory
// the kernel releases nothing on death. It is not a place secrets live: an
// entry carries digest prefixes — eight hex digits of a SHA-256 — and the
// append refuses anything else, so a caller that passed a whole digest or
// a token is a failed write rather than a leak.
const AuditLogFile = "keychain-writes.jsonl"

// DigestPrefixLen is how many hex digits of a digest an entry may carry.
const DigestPrefixLen = 8

// AuditLogPath is where the log lives for one store.
func AuditLogPath(paths *config.Paths) string {
	return filepath.Join(paths.NamespaceRoot(), AuditLogFile)
}

// auditNow is the clock entry timestamps come from, replaceable only by a
// test in this package.
var auditNow = time.Now

// auditProcessStart anchors every entry's monotonic reading. A monotonic
// clock has no epoch, so the number is meaningless on its own and
// deliberately so: it exists to compare two entries' spacing against their
// wall-clock spacing, which is how a clock step shows up in the record of
// a break that spanned one.
var auditProcessStart = time.Now()

// AuditEvent is one kind of event the log records. The set is closed:
// outside packages construct the exported event types but cannot add one,
// so every reader's dispatch stays complete.
type AuditEvent interface {
	// auditEventName is the event member's value; empty for the kind this
	// build only reads.
	auditEventName() string
}

// KeychainWriteOutcome is how a keychain write ended.
type KeychainWriteOutcome string

const (
	// WriteApplied means the item was re-read after the locks were
	// released and holds the incoming credential.
	WriteApplied KeychainWriteOutcome = "applied"
	// WriteUnknown means the write returned but the verifying re-read did
	// not agree — which includes a legitimate peer write landing in the
	// gap. It means "re-run status", not "failed".
	WriteUnknown KeychainWriteOutcome = "unknown"
	// WriteFailed means the write itself failed and the item was left as
	// it was.
	WriteFailed KeychainWriteOutcome = "failed"
	// WriteDiscarded means the refresh was performed and never written: a
	// refusal after the POST threw away a credential the server had
	// already minted. Recorded rather than silent, because the item still
	// holds the old refresh token, which the server has usually rotated
	// away, so the next pass may report needs-login for reasons this pass
	// created.
	WriteDiscarded KeychainWriteOutcome = "discarded"
)

// WriteDirection is which way round a swap ran.
type WriteDirection string

const (
	// DirectionForward is a forward swap — and any write that is not a
	// reversal, such as a refresh saved in place. An entry written before
	// the member existed reads as forward, the conservative reading for
	// the guard that consults it: it arms rather than disarms.
	DirectionForward WriteDirection = "forward"
	// DirectionUndo is a reversal.
	DirectionUndo WriteDirection = "undo"
)

// IncomingIdentity is the account a live write installed in the item, by
// id alone — never a token and never an email address.
type IncomingIdentity struct {
	// AccountUUID is the account id.
	AccountUUID string `json:"account_uuid"`
	// OrganizationUUID is the organization id, when the registry knows
	// one; nil for a record still carrying the unknown-organization
	// placeholder.
	OrganizationUUID *string `json:"organization_uuid"`
}

// WriteEvent is one keychain write.
type WriteEvent struct {
	// Target says which item was written.
	Target Target
	// FromDigest8 is the first eight hex digits of the outgoing
	// credential's access-token digest, or nil when the item did not exist
	// — which is what records a first write as a first write.
	FromDigest8 *string
	// ToDigest8 is the first eight hex digits of the incoming credential's
	// access-token digest.
	ToDigest8 string
	// Outcome says how the write ended.
	Outcome KeychainWriteOutcome
	// Direction says which way round the swap ran.
	Direction WriteDirection
	// IncomingIdentity names the account a live write installed, by id
	// alone, forward and undo alike; nil on namespace entries and on live
	// entries written before the member existed — which is why an entry
	// without it refuses rather than being guessed at.
	IncomingIdentity *IncomingIdentity
}

func (*WriteEvent) auditEventName() string { return "write" }

// auditEventName makes a lock-break record appendable as an audit event.
func (*LockBreakRecord) auditEventName() string { return "lock_break" }

// ConfigOutcome is how one config step of a live pass ended.
type ConfigOutcome string

const (
	// ConfigApplied means the file was rewritten.
	ConfigApplied ConfigOutcome = "applied"
	// ConfigSkipped means nothing was there to rewrite, or the lock could
	// not be taken.
	ConfigSkipped ConfigOutcome = "skipped"
	// ConfigRefused means the file could not be rewritten safely, decided
	// before anything was written.
	ConfigRefused ConfigOutcome = "refused"
	// ConfigAborted means a check under the lock failed and nothing was
	// renamed.
	ConfigAborted ConfigOutcome = "aborted"
	// ConfigFailed means a write or a rename failed.
	ConfigFailed ConfigOutcome = "failed"
	// ConfigNotAttempted means the step deliberately did not run.
	ConfigNotAttempted ConfigOutcome = "not_attempted"
	// ConfigUnrecognized is a word a later build writes; read-only.
	ConfigUnrecognized ConfigOutcome = "unrecognized"
)

// ConfigReason is why a config step did not apply.
type ConfigReason string

const (
	// ConfigReasonAbsent: there is no configuration file.
	ConfigReasonAbsent ConfigReason = "absent"
	// ConfigReasonUnreadable: not a regular file, too large, or unreadable.
	ConfigReasonUnreadable ConfigReason = "unreadable"
	// ConfigReasonUnparseable: not JSON.
	ConfigReasonUnparseable ConfigReason = "unparseable"
	// ConfigReasonNotAnObject: the top level is not an object.
	ConfigReasonNotAnObject ConfigReason = "not_an_object"
	// ConfigReasonNotReproducible: re-serialising did not reproduce its bytes.
	ConfigReasonNotReproducible ConfigReason = "not_reproducible"
	// ConfigReasonBackupUnwritable: its backup could not be written first.
	ConfigReasonBackupUnwritable ConfigReason = "backup_unwritable"
	// ConfigReasonLockBusy: a session held the configuration lock throughout.
	ConfigReasonLockBusy ConfigReason = "lock_busy"
	// ConfigReasonLockStale: the configuration lock was stale; it is never broken.
	ConfigReasonLockStale ConfigReason = "lock_stale"
	// ConfigReasonCancelled: the run was cancelled while waiting for the lock.
	ConfigReasonCancelled ConfigReason = "cancelled"
	// ConfigReasonChangedUnderLock: the file changed while the lock was held.
	ConfigReasonChangedUnderLock ConfigReason = "changed_under_lock"
	// ConfigReasonCompromised: the held lock was broken underneath us.
	ConfigReasonCompromised ConfigReason = "compromised"
	// ConfigReasonBudget: a term could not finish inside the lock's budget.
	ConfigReasonBudget ConfigReason = "budget"
	// ConfigReasonIO: a write or a rename failed.
	ConfigReasonIO ConfigReason = "io"
	// ConfigReasonProfileUnavailable: the installed account's profile could not be read.
	ConfigReasonProfileUnavailable ConfigReason = "profile_unavailable"
	// ConfigReasonSwapUnknown: the swap's own outcome is unknown.
	ConfigReasonSwapUnknown ConfigReason = "swap_unknown"
	// ConfigReasonAlreadyCurrent: a catch-up found the file already naming
	// the live item's account.
	ConfigReasonAlreadyCurrent ConfigReason = "already_current"
	// ConfigReasonDeclined: a catch-up's rewrite was not confirmed. Never
	// written: a declined catch-up appends no line.
	ConfigReasonDeclined ConfigReason = "declined"
	// ConfigReasonAuditRefused: a catch-up met an audit log that is
	// refused. Never written: there is no log to write it to.
	ConfigReasonAuditRefused ConfigReason = "audit_refused"
	// ConfigReasonUnrecognized is a word a later build writes; read-only.
	ConfigReasonUnrecognized ConfigReason = "unrecognized"
)

// ConfigWriteRecord is what one config step of a live pass did: the
// rewrite that follows an applied live write, or the record that it was
// not attempted. One line per step even when nothing was written, so the
// log says why a file was left as it was. Ids and digest prefixes only:
// the account is a pair of ids, never an email, and the backup is a file
// name, never a path — the append refuses an entry that carries anything
// else.
type ConfigWriteRecord struct {
	// After is the identity of the write entry this pass appended, as
	// [AuditID.String] renders it; nil on a catch-up, which follows no
	// write of its own.
	After *string `json:"after"`
	// Outcome says how the step ended.
	Outcome ConfigOutcome `json:"outcome"`
	// Reason says why, when it did not apply.
	Reason *ConfigReason `json:"reason"`
	// Account is the account written into the configuration, by id alone.
	Account *IncomingIdentity `json:"account"`
	// FromSHA8 is the first eight hex digits of the file's digest as it
	// was read.
	FromSHA8 *string `json:"from_sha8"`
	// ToSHA8 is the same for the file as it was written; only on applied.
	ToSHA8 *string `json:"to_sha8"`
	// Backup is the backup's file name, under the peer's backups
	// directory.
	Backup *string `json:"backup"`
	// HoldMS is how long the configuration lock was held, in whole
	// milliseconds, when one was taken.
	HoldMS *uint64 `json:"hold_ms"`
}

func (*ConfigWriteRecord) auditEventName() string { return "config_write" }

// UnrecognizedEvent is an event kind this build does not know: a later
// build's additive kind, read rather than refused. Read-only: the append
// refuses to write it, so a line can only ever arrive here from a newer
// build.
type UnrecognizedEvent struct{}

func (*UnrecognizedEvent) auditEventName() string { return "" }

// AuditEntry is one line of the log. The provenance members — the
// timestamp, the monotonic reading, the writing process — come first and
// belong to this process, so the pid can never be read as the pid of
// somebody else's lock holder.
type AuditEntry struct {
	// TS is when this process wrote the entry, RFC 3339 in UTC. Kept as
	// the string the line carries, so a round trip is byte-stable.
	TS string
	// MonotonicMS is milliseconds on this process's monotonic clock, for
	// the reason [auditProcessStart] gives.
	MonotonicMS uint64
	// PID is this process's own id — never a holder's.
	PID uint32
	// Event is what happened.
	Event AuditEvent
}

// NewAuditEntry stamps an event with now, this process's monotonic
// reading, and this process's id.
func NewAuditEntry(event AuditEvent) AuditEntry {
	return AuditEntry{
		TS:          auditNow().UTC().Format(time.RFC3339Nano),
		MonotonicMS: uint64(max(time.Since(auditProcessStart).Milliseconds(), 0)),
		PID:         uint32(os.Getpid()),
		Event:       event,
	}
}

// ID is this entry's identity: its timestamp and the process that wrote
// it.
func (e AuditEntry) ID() AuditID {
	return AuditID{TS: e.TS, PID: e.PID}
}

// AuditID names one entry, so a command can print it and a later doctor
// or undo can find the same line.
type AuditID struct {
	// TS is the entry's timestamp.
	TS string
	// PID is the process that wrote it.
	PID uint32
}

// String renders the id the way the log's readers match it.
func (id AuditID) String() string {
	return fmt.Sprintf("%s#%d", id.TS, id.PID)
}

// auditProvenance is the head of every line, in the fixed order.
type auditProvenance struct {
	TS          string `json:"ts"`
	MonotonicMS uint64 `json:"monotonic_ms"`
	PID         uint32 `json:"agctl_pid"`
	Event       string `json:"event"`
}

// auditWriteLine is a write entry as the line spells it.
type auditWriteLine struct {
	auditProvenance
	Target           Target               `json:"target"`
	FromDigest8      *string              `json:"from_digest8"`
	ToDigest8        string               `json:"to_digest8"`
	Outcome          KeychainWriteOutcome `json:"outcome"`
	Direction        WriteDirection       `json:"direction"`
	IncomingIdentity *IncomingIdentity    `json:"incoming_identity,omitzero"`
}

// auditLockBreakLine is a lock-break entry as the line spells it.
type auditLockBreakLine struct {
	auditProvenance
	LockBreakRecord
}

// auditConfigWriteLine is a config-step entry as the line spells it.
type auditConfigWriteLine struct {
	auditProvenance
	ConfigWriteRecord
}

// auditEntryLine is one entry as the line the log holds, with the
// digest-prefix guard applied. Both entry points go through this, which is
// what keeps one serialisation and one digest check: a second spelling of
// either would let an entry one of them refuses reach the log through the
// other.
func auditEntryLine(entry AuditEntry) (string, error) {
	provenance := auditProvenance{TS: entry.TS, MonotonicMS: entry.MonotonicMS, PID: entry.PID}
	var line any
	switch event := entry.Event.(type) {
	case *WriteEvent:
		if event.FromDigest8 != nil {
			if err := checkDigest8("from_digest8", *event.FromDigest8); err != nil {
				return "", err
			}
		}
		if err := checkDigest8("to_digest8", event.ToDigest8); err != nil {
			return "", err
		}
		direction := event.Direction
		if direction == "" {
			direction = DirectionForward
		}
		provenance.Event = event.auditEventName()
		line = &auditWriteLine{auditProvenance: provenance, Target: event.Target, FromDigest8: event.FromDigest8, ToDigest8: event.ToDigest8, Outcome: event.Outcome, Direction: direction, IncomingIdentity: event.IncomingIdentity}
	case *LockBreakRecord:
		provenance.Event = event.auditEventName()
		line = &auditLockBreakLine{auditProvenance: provenance, LockBreakRecord: *event}
	case *ConfigWriteRecord:
		// The same prefix rule for both digests, and a backup that is a
		// bare file name in the peer's shape — a caller that passed a
		// whole digest or a path fails the append rather than leaking it.
		if event.FromSHA8 != nil {
			if err := checkDigest8("from_sha8", *event.FromSHA8); err != nil {
				return "", err
			}
		}
		if event.ToSHA8 != nil {
			if err := checkDigest8("to_sha8", *event.ToSHA8); err != nil {
				return "", err
			}
		}
		if event.Backup != nil && !isBackupName(*event.Backup) {
			return "", errs.NewConfig(fmt.Sprintf("an audit entry's `backup` must be a `.claude.json.backup.<ms>` file name, not %d characters; the audit log holds no paths", len(*event.Backup)))
		}
		if event.Outcome == ConfigUnrecognized || (event.Reason != nil && *event.Reason == ConfigReasonUnrecognized) {
			return "", errs.NewConfig("an audit entry's config outcome or reason is a word this build only reads; it is never written")
		}
		provenance.Event = event.auditEventName()
		line = &auditConfigWriteLine{auditProvenance: provenance, ConfigWriteRecord: *event}
	case *UnrecognizedEvent:
		// Writing it would record an event nobody performed under a name
		// that means "a later build wrote this".
		return "", errs.NewConfig("an unrecognised audit event is read from a later build's log, never written")
	default:
		return "", errs.NewConfig("an audit entry carries an event kind the log does not write")
	}

	body, err := json.Marshal(line)
	if err != nil {
		return "", errs.NewConfig(fmt.Sprintf("an audit entry could not be serialized: %v", err))
	}
	return string(body) + "\n", nil
}

// AuditAppend appends one entry and returns its identity.
//
// The file is created 0600 if it is absent, opened O_APPEND through the
// same no-follow walk every store mutation uses, written with a single
// write and fsynced before this returns — because the whole point of the
// record is to survive the crash that happens next. A refused entry — a
// digest field that is not exactly eight lowercase hex digits, a backup
// that is not a bare file name — never creates the log on its way to being
// refused; so does a refused log: a link at its name or on the way to it,
// something that is not a regular file, or a mode that is not 0600, which
// is refused rather than repaired because a chmod would erase the evidence
// that somebody else can read this machine's swap history.
func AuditAppend(ctx context.Context, paths *config.Paths, entry AuditEntry) (AuditID, error) {
	// Serialized and checked before any I/O, so a malformed entry cannot
	// create the log on its way to being refused.
	line, err := auditEntryLine(entry)
	if err != nil {
		return AuditID{}, err
	}

	if err := paths.EnsureDirs(ctx); err != nil {
		return AuditID{}, err
	}
	shown := AuditLogPath(paths)
	file, err := OpenAuditLog(paths, shown)
	if err != nil {
		return AuditID{}, err
	}
	defer func() { _ = file.Close() }()
	if err := WriteAuditLine(file, shown, line); err != nil {
		return AuditID{}, err
	}
	return entry.ID(), nil
}

// AuditAppendThrough appends one entry through a descriptor the caller is
// already holding.
//
// A live swap gates on [OpenAuditLog] before it writes anything — a held
// descriptor, not a report — and keeps it across the write, so the entry
// that records the write is appended through the same file the gate proved
// appendable. Nothing between the two can redirect the log: a planted link
// or a chmod after the open changes the name, and this writes to the
// descriptor. Serialization and the digest checks are [AuditAppend]'s own,
// shared rather than repeated. shown is for the error sentences only;
// nothing is resolved through it.
func AuditAppendThrough(file *os.File, shown string, entry AuditEntry) (AuditID, error) {
	line, err := auditEntryLine(entry)
	if err != nil {
		return AuditID{}, err
	}
	if err := WriteAuditLine(file, shown, line); err != nil {
		return AuditID{}, err
	}
	return entry.ID(), nil
}

// WriteAuditLine lands one line with one write and one fsync, because the
// point of the record is to survive the crash that happens next. Exported
// so another provider's log appends its own entry type through the same
// one-write-one-flush rule rather than a copy of it.
func WriteAuditLine(file *os.File, shown, line string) error {
	if _, err := file.WriteString(line); err != nil {
		return errs.NewIO(fmt.Sprintf("could not append to the audit log `%s`", shown), err)
	}
	if err := file.Sync(); err != nil {
		return errs.NewIO(fmt.Sprintf("could not flush the audit log `%s`", shown), err)
	}
	return nil
}

// AuditLogStateKind is what is at the log's name.
type AuditLogStateKind int

const (
	// AuditLogAbsent means nothing is there: a store that has never
	// appended.
	AuditLogAbsent AuditLogStateKind = iota + 1
	// AuditLogPresent means a regular file at mode 0600 — the one shape
	// the append writes to.
	AuditLogPresent
	// AuditLogWrongMode means a regular file whose permission bits are
	// something else.
	AuditLogWrongMode
	// AuditLogRefused means a symbolic link at the log's name, a link on
	// the way to it, or something that is not a regular file at all.
	AuditLogRefused
)

// AuditLogState is the log's state seen through the same walk the append
// writes through. The vocabulary exists so the refusal and the report
// cannot drift: the append hands [AuditLogState.Note]'s sentence to its
// caller, and doctor prints the same sentence. A wrong mode is a state,
// never a repair: the file belongs to whoever set it that way, and saying
// so is worth more than quietly making it look right.
type AuditLogState struct {
	// Kind classifies what is there.
	Kind AuditLogStateKind
	// Mode carries the permission bits for [AuditLogWrongMode].
	Mode fs.FileMode
	// Why carries the refusal for [AuditLogRefused].
	Why string
}

// Note is the sentence doctor prints, and the reason a refused append
// carries.
func (s AuditLogState) Note() string {
	switch s.Kind {
	case AuditLogAbsent:
		return "absent"
	case AuditLogPresent:
		return "present"
	case AuditLogWrongMode:
		return fmt.Sprintf("present, but its mode is %04o and not %04o: agentctl refuses the log and will not change it", s.Mode, config.FileMode)
	default:
		return s.Why
	}
}

// IsAppendable reports whether the append will write to what is at that
// name.
func (s AuditLogState) IsAppendable() bool {
	return s.Kind == AuditLogAbsent || s.Kind == AuditLogPresent
}

// AuditLogStateOf is what is at the audit log's name, for doctor's report.
// It reads through the same walk the append uses, so what it reports is
// what the append would meet rather than what a second path resolution
// would find. A store with no namespace root at all has never appended.
func AuditLogStateOf(paths *config.Paths) AuditLogState {
	dir, err := openAuditLogDir(paths)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return AuditLogState{Kind: AuditLogAbsent}
	default:
		return AuditLogState{Kind: AuditLogRefused, Why: fmt.Sprintf("unreachable: %v", err)}
	}
	defer func() { _ = unix.Close(dir) }()
	return auditStateAt(dir, AuditLogFile)
}

// OpenAuditLog opens the log for one append, refusing a link at its name
// and a mode that is not 0600.
//
// The log lives in the directory the held-lock records are kept in, so
// whoever could plant that name could plant this one too — and this file
// is the only durable evidence a broken lock leaves. The directory is
// resolved once, one no-follow component at a time from the configuration
// directory down, and the log opened relative to the descriptor that walk
// produced. The mode is checked on the open descriptor, not on the name,
// so there is no window between the check and the write; it is refused,
// never repaired. A live-store swap gates on this descriptor rather than
// on [AuditLogStateOf]: a report leaves the whole width between the look
// and the write to whoever can plant a name here, a held descriptor leaves
// none.
func OpenAuditLog(paths *config.Paths, shown string) (*os.File, error) {
	dir, err := openAuditLogDir(paths)
	if err != nil {
		return nil, auditRefused(shown, fmt.Sprintf("its directory is unreachable: %v", err))
	}
	defer func() { _ = unix.Close(dir) }()
	return OpenAuditLogAt(dir, AuditLogFile, shown)
}

// OpenAuditLogAt is [OpenAuditLog]'s open, relative to a directory the
// caller's own no-follow walk produced, for a log with any name. Every
// rule is the same, because it is that function's body; another provider's
// log reaches its own directory and opens its own name through here. name
// must be one plain path component; shown is the log as the user would
// recognise it, used only in the error sentences.
func OpenAuditLogAt(dir int, name, shown string) (*os.File, error) {
	if !config.IsSingleComponent(name) {
		return nil, auditRefused(shown, "its name is not a single path component")
	}

	// O_NONBLOCK is not decoration: without it a FIFO planted at this name
	// — one mkfifo, the same precondition as the symbolic link — blocks
	// the open until a reader arrives, and the append runs with the
	// namespace lock held, so the whole store would wedge instead of one
	// entry failing. With it the open returns at once and the FIFO is
	// refused as what it is; it changes nothing for a regular file.
	flags := unix.O_WRONLY | unix.O_APPEND | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Openat(dir, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		// Separate creation from opening an existing log: concurrent O_CREAT
		// opens can return ENOENT while another creator installs the file.
		// An exclusive creator wins; the others reopen without creation.
		fd, err = unix.Openat(dir, name, flags|unix.O_CREAT|unix.O_EXCL, uint32(config.FileMode))
		if errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(dir, name, flags, 0)
		}
	}
	if err != nil {
		// The open has already refused: O_NOFOLLOW followed nothing and
		// no open uses O_TRUNC. This second look only decides which
		// sentence the caller is handed.
		return nil, auditRefused(shown, whyAuditOpenFailed(dir, name, err))
	}

	file := os.NewFile(uintptr(fd), shown)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, errs.NewIO(fmt.Sprintf("could not stat the audit log `%s`", shown), err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, auditRefused(shown, "it is not a regular file")
	}
	if mode := info.Mode().Perm(); mode != config.FileMode {
		_ = file.Close()
		return nil, auditRefused(shown, AuditLogState{Kind: AuditLogWrongMode, Mode: mode}.Note())
	}
	return file, nil
}

// ReadAuditLogAt reads a log's whole text relative to a directory the
// caller's own no-follow walk produced. The reader refuses what the writer
// refuses — an undo acts on what a tail returns, so a log somebody else
// could redirect would be an undo somebody else could direct — but it does
// not apply the mode rule: a log at the wrong mode is one doctor must
// still be able to report, and reading it discloses nothing its mode has
// not already disclosed. present is false when nothing is there.
func ReadAuditLogAt(dir int, name, shown string) (string, bool, error) {
	if !config.IsSingleComponent(name) {
		return "", false, auditRefused(shown, "its name is not a single path component")
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
	case errors.Is(err, unix.ENOENT):
		return "", false, nil
	default:
		return "", false, auditRefused(shown, whyAuditOpenFailed(dir, name, err))
	}
	file := os.NewFile(uintptr(fd), shown)
	defer func() { _ = file.Close() }()
	text, err := io.ReadAll(file)
	if err != nil {
		return "", false, errs.NewIO(fmt.Sprintf("could not read the audit log `%s`", shown), err)
	}
	return string(text), true, nil
}

// ForEachAuditLineAt hands the caller one line at a time instead of the
// whole file, reporting whether a log was there at all.
//
// A caller that must consult every line — which provider state a refused
// login left behind, a thousand writes back — would otherwise turn an
// append-only log into a whole-file allocation that grows with the user's
// history; this reads through a buffer, so the memory it needs is the
// bound plus the buffer, whatever the log's size. A line longer than
// maxLineBytes, and a line that is not UTF-8, is skipped rather than
// refused: neither is a line this store wrote, and a reader asked to find
// this store's own entries must not be stopped by somebody else's. The
// bound is the caller's, because only the caller knows how long its own
// entries are.
func ForEachAuditLineAt(dir int, name, shown string, maxLineBytes int, visit func(line string)) (bool, error) {
	if !config.IsSingleComponent(name) {
		return false, auditRefused(shown, "its name is not a single path component")
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
	case errors.Is(err, unix.ENOENT):
		return false, nil
	default:
		return false, auditRefused(shown, whyAuditOpenFailed(dir, name, err))
	}
	file := os.NewFile(uintptr(fd), shown)
	defer func() { _ = file.Close() }()

	reader := bufio.NewReader(file)
	var line []byte
	// Set when the current line has already passed the bound: its
	// remaining bytes are consumed and dropped rather than collected, so
	// one enormous line costs time and not memory.
	overBound := false
	flush := func() {
		if !overBound && utf8.Valid(line) {
			visit(string(line))
		}
		line = line[:0]
		overBound = false
	}
	for {
		chunk, err := reader.ReadSlice('\n')
		complete := err == nil
		if len(chunk) > 0 {
			body := chunk
			if complete {
				body = chunk[:len(chunk)-1]
			}
			if len(line)+len(body) > maxLineBytes {
				overBound = true
			}
			if !overBound {
				line = append(line, body...)
			}
		}
		switch {
		case complete:
			flush()
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			// A final line with no terminating newline is still a line: a
			// writer interrupted between the bytes and the newline leaves
			// one.
			if len(line) > 0 && !overBound && utf8.Valid(line) {
				visit(string(line))
			}
			return true, nil
		default:
			return true, errs.NewIO(fmt.Sprintf("could not read the audit log `%s`", shown), err)
		}
	}
}

// openAuditLogDir opens the directory the log lives in without following a
// link, anchored at the configuration directory and walked down to the
// namespace root, so the component a planter would swap is opened
// no-follow like every other.
func openAuditLogDir(paths *config.Paths) (int, error) {
	return OpenDirUnder(paths.ConfigDir(), paths.NamespaceRoot())
}

// auditStateAt is what is at name inside an already-opened directory.
func auditStateAt(dir int, name string) AuditLogState {
	var st unix.Stat_t
	err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			return AuditLogState{Kind: AuditLogRefused, Why: "a symbolic link, which agentctl will not append through"}
		case unix.S_IFREG:
			mode := fs.FileMode(st.Mode) & fs.ModePerm
			if mode == config.FileMode {
				return AuditLogState{Kind: AuditLogPresent}
			}
			return AuditLogState{Kind: AuditLogWrongMode, Mode: mode}
		default:
			return AuditLogState{Kind: AuditLogRefused, Why: "not a regular file"}
		}
	case errors.Is(err, unix.ENOENT):
		return AuditLogState{Kind: AuditLogAbsent}
	default:
		return AuditLogState{Kind: AuditLogRefused, Why: fmt.Sprintf("could not be examined: %v", err)}
	}
}

// whyAuditOpenFailed says why an open of the log failed, in the user's
// terms: the state's sentence when the shape of what is there explains it,
// the errno when it does not.
func whyAuditOpenFailed(dir int, name string, openErr error) string {
	state := auditStateAt(dir, name)
	if state.IsAppendable() {
		return fmt.Sprintf("it could not be opened: %v", openErr)
	}
	return state.Note()
}

// auditRefused is the refusal both halves of the log hand back.
func auditRefused(shown, why string) error {
	return errs.NewConfig(fmt.Sprintf("the audit log `%s` is refused: %s", shown, why))
}

// UnreadableAuditLine names one line a tail could not read, so doctor can
// report it instead of hiding it.
type UnreadableAuditLine struct {
	// Line is the 1-based line number.
	Line int
	// Reason says why it did not parse.
	Reason string
}

// AuditTail is what one tail read found: the entries it could parse, and
// the lines it could not. Two lists rather than one failure, because the
// caller's job is to report this file and one damaged line must not take
// the report down with it: the append is one write plus an fsync, so a
// process killed between them leaves a truncated final line, which is by
// construction inside the last n — failing the whole read for it would
// break doctor and undo exactly after the crash they are the recovery for.
type AuditTail struct {
	// Entries are the entries that parsed, oldest first.
	Entries []AuditEntry
	// Unreadable names the lines in the window that did not.
	Unreadable []UnreadableAuditLine
}

// TailAuditLog returns the last n entries, oldest first, plus the lines in
// that window that could not be read.
//
// An absent log is no entries rather than an error: a store that has never
// written a keychain item has nothing to explain. A line carrying unknown
// members is not unreadable — a log written by a later build still reads —
// and neither is a line whose event is a kind this build does not know: it
// reads as [UnrecognizedEvent] rather than blocking an undo that refuses
// on unreadable lines. The read goes through the same no-follow walk the
// append writes through.
func TailAuditLog(paths *config.Paths, n int) (AuditTail, error) {
	shown := AuditLogPath(paths)
	dir, err := openAuditLogDir(paths)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		// A store with no namespace root has never appended.
		return AuditTail{}, nil
	default:
		return AuditTail{}, auditRefused(shown, fmt.Sprintf("its directory is unreachable: %v", err))
	}
	defer func() { _ = unix.Close(dir) }()
	text, present, err := ReadAuditLogAt(dir, AuditLogFile, shown)
	if err != nil || !present {
		return AuditTail{}, err
	}

	type numbered struct {
		number int
		line   string
	}
	var lines []numbered
	for index, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			lines = append(lines, numbered{number: index + 1, line: trimmed})
		}
	}
	start := max(len(lines)-n, 0)
	var tail AuditTail
	for _, item := range lines[start:] {
		entry, err := decodeAuditLine([]byte(item.line))
		if err != nil {
			tail.Unreadable = append(tail.Unreadable, UnreadableAuditLine{Line: item.number, Reason: err.Error()})
			continue
		}
		tail.Entries = append(tail.Entries, entry)
	}
	return tail, nil
}

// decodeAuditLine reads one line back into an entry, enforcing what the
// writer's vocabulary promises: valid provenance, a valid target, and
// outcome words the write kinds define. An unknown event kind reads as
// [UnrecognizedEvent]; an unknown config outcome or reason word reads as
// unrecognized rather than failing, so a later build's additive word does
// not block a reader.
func decodeAuditLine(line []byte) (AuditEntry, error) {
	var head struct {
		TS          *string `json:"ts"`
		MonotonicMS *uint64 `json:"monotonic_ms"`
		PID         *uint32 `json:"agctl_pid"`
		Event       *string `json:"event"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return AuditEntry{}, err
	}
	if head.TS == nil || head.MonotonicMS == nil || head.PID == nil || head.Event == nil {
		return AuditEntry{}, errors.New("missing ts, monotonic_ms, agctl_pid or event")
	}
	if _, err := time.Parse(time.RFC3339, *head.TS); err != nil {
		return AuditEntry{}, fmt.Errorf("invalid ts: %w", err)
	}
	entry := AuditEntry{TS: *head.TS, MonotonicMS: *head.MonotonicMS, PID: *head.PID}

	switch *head.Event {
	case "write":
		var body struct {
			Target           *string           `json:"target"`
			FromDigest8      *string           `json:"from_digest8"`
			ToDigest8        *string           `json:"to_digest8"`
			Outcome          *string           `json:"outcome"`
			Direction        *string           `json:"direction"`
			IncomingIdentity *IncomingIdentity `json:"incoming_identity"`
		}
		if err := json.Unmarshal(line, &body); err != nil {
			return AuditEntry{}, err
		}
		if body.Target == nil || body.ToDigest8 == nil || body.Outcome == nil {
			return AuditEntry{}, errors.New("a write entry is missing target, to_digest8 or outcome")
		}
		target, err := parseTarget(*body.Target)
		if err != nil {
			return AuditEntry{}, err
		}
		outcome := KeychainWriteOutcome(*body.Outcome)
		switch outcome {
		case WriteApplied, WriteUnknown, WriteFailed, WriteDiscarded:
		default:
			return AuditEntry{}, fmt.Errorf("unknown write outcome `%s`", outcome)
		}
		direction := DirectionForward
		if body.Direction != nil {
			direction = WriteDirection(*body.Direction)
			if direction != DirectionForward && direction != DirectionUndo {
				return AuditEntry{}, fmt.Errorf("unknown write direction `%s`", direction)
			}
		}
		entry.Event = &WriteEvent{Target: target, FromDigest8: body.FromDigest8, ToDigest8: *body.ToDigest8, Outcome: outcome, Direction: direction, IncomingIdentity: body.IncomingIdentity}
	case "lock_break":
		var record LockBreakRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return AuditEntry{}, err
		}
		// Value fields cannot distinguish an omitted member from a valid
		// zero. Check presence separately, including each supplied sample.
		var members map[string]jsontext.Value
		if err := json.Unmarshal(line, &members); err != nil {
			return AuditEntry{}, err
		}
		require := func(members map[string]jsontext.Value, names ...string) error {
			for _, name := range names {
				value := strings.TrimSpace(string(members[name]))
				if value == "" || value == "null" {
					return fmt.Errorf("a lock_break entry is missing required member `%s`", name)
				}
			}
			return nil
		}
		if err := require(members, "path", "store_dir", "tree", "service", "target", "sample_a", "interval_wall_ms", "interval_monotonic_ms", "holder_evidence", "outcome"); err != nil {
			return AuditEntry{}, err
		}
		for _, name := range []string{"sample_a", "sample_b", "sample_c"} {
			raw := members[name]
			if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
				continue
			}
			var sample map[string]jsontext.Value
			if err := json.Unmarshal(raw, &sample); err != nil {
				return AuditEntry{}, err
			}
			if err := require(sample, "at", "mtime_ns", "age_ms"); err != nil {
				return AuditEntry{}, fmt.Errorf("%s: %w", name, err)
			}
		}
		if _, err := parseTarget(string(record.Target)); err != nil {
			return AuditEntry{}, err
		}
		if record.Tree != TreeOwn && record.Tree != TreeLive {
			return AuditEntry{}, fmt.Errorf("unknown tree `%s`", record.Tree)
		}
		switch record.Evidence {
		case EvidenceStoppedClaudePresent, EvidenceNoStoppedClaude, EvidenceUnreadable, EvidenceNone:
		default:
			return AuditEntry{}, fmt.Errorf("unknown holder evidence `%s`", record.Evidence)
		}
		if record.Outcome != OutcomeBroken && record.Outcome != OutcomeAbandoned {
			return AuditEntry{}, fmt.Errorf("unknown break outcome `%s`", record.Outcome)
		}
		switch record.Reason {
		case "", ReasonHeartbeatObserved, ReasonTooYoung, ReasonVanished, ReasonClockJump, ReasonRetaken, ReasonHolderStopped, ReasonHolderUnreadable:
		default:
			return AuditEntry{}, fmt.Errorf("unknown break reason `%s`", record.Reason)
		}
		entry.Event = &record
	case "config_write":
		var body struct {
			Outcome *string `json:"outcome"`
		}
		if err := json.Unmarshal(line, &body); err != nil {
			return AuditEntry{}, err
		}
		if body.Outcome == nil {
			return AuditEntry{}, errors.New("a config_write entry is missing its outcome")
		}
		var record ConfigWriteRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return AuditEntry{}, err
		}
		switch record.Outcome {
		case ConfigApplied, ConfigSkipped, ConfigRefused, ConfigAborted, ConfigFailed, ConfigNotAttempted:
		default:
			record.Outcome = ConfigUnrecognized
		}
		if record.Reason != nil {
			switch *record.Reason {
			case ConfigReasonAbsent, ConfigReasonUnreadable, ConfigReasonUnparseable, ConfigReasonNotAnObject, ConfigReasonNotReproducible, ConfigReasonBackupUnwritable, ConfigReasonLockBusy, ConfigReasonLockStale, ConfigReasonCancelled, ConfigReasonChangedUnderLock, ConfigReasonCompromised, ConfigReasonBudget, ConfigReasonIO, ConfigReasonProfileUnavailable, ConfigReasonSwapUnknown, ConfigReasonAlreadyCurrent, ConfigReasonDeclined, ConfigReasonAuditRefused:
			default:
				unrecognized := ConfigReasonUnrecognized
				record.Reason = &unrecognized
			}
		}
		entry.Event = &record
	default:
		entry.Event = &UnrecognizedEvent{}
	}
	return entry, nil
}

// parseTarget reads a target token back, refusing one the vocabulary does
// not contain.
func parseTarget(s string) (Target, error) {
	if s == string(TargetLive) {
		return TargetLive, nil
	}
	if sha8, ok := strings.CutPrefix(s, "namespace:"); ok {
		return NamespaceTarget(sha8), nil
	}
	return "", fmt.Errorf("unknown audit target `%s`", s)
}

// Digest8 takes the first [DigestPrefixLen] hex digits of a digest for an
// entry. ok is false when the digest is shorter than that, or when its
// prefix is not lowercase hex — which means the caller has something other
// than a digest in its hand and should not be writing it to a file either
// way.
func Digest8(digest string) (string, bool) {
	if len(digest) < DigestPrefixLen {
		return "", false
	}
	prefix := digest[:DigestPrefixLen]
	if !isDigest8(prefix) {
		return "", false
	}
	return prefix, true
}

// checkDigest8 refuses a digest field that is anything but eight lowercase
// hex digits.
func checkDigest8(field, value string) error {
	if isDigest8(value) {
		return nil
	}
	return errs.NewConfig(fmt.Sprintf("an audit entry's `%s` must be %d lowercase hex digits, not %d characters; the audit log holds digest prefixes only", field, DigestPrefixLen, len(value)))
}

// isDigest8 reports whether value is exactly eight lowercase hex digits.
func isDigest8(value string) bool {
	if len(value) != DigestPrefixLen {
		return false
	}
	for _, b := range []byte(value) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// isBackupName reports whether value is a backup file name in the peer's
// shape — `.claude.json.backup.<digits>` or `.config.json.backup.<digits>`
// — and so carries no path separator.
func isBackupName(value string) bool {
	for _, prefix := range []string{".claude.json.backup.", ".config.json.backup."} {
		stamp, ok := strings.CutPrefix(value, prefix)
		if !ok || stamp == "" {
			continue
		}
		digits := true
		for _, b := range []byte(stamp) {
			if b < '0' || b > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	return false
}
