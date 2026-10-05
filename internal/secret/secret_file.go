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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

// SymlinkRefusedError reports a symbolic link at a credential path, or at a
// directory on the way to one. Never followed, never overwritten: a link
// there is somebody else deciding where this store's files live.
type SymlinkRefusedError struct {
	// Path is where the link was found.
	Path string
}

// Error names the refused path.
func (e *SymlinkRefusedError) Error() string {
	return fmt.Sprintf("`%s` is a symbolic link; refusing to read or write through it", e.Path)
}

// NotRegularError reports a path that exists but is not what belongs there:
// a credential or metadata file must be a regular file, and a component of
// the namespace directory must be a directory.
type NotRegularError struct {
	// Path is the offending path.
	Path string
}

// Error names the offending path.
func (e *NotRegularError) Error() string {
	return fmt.Sprintf("`%s` is not the plain file or directory that belongs at that path", e.Path)
}

// TooLargeError reports a file larger than its format allows.
type TooLargeError struct {
	// Path is the offending path.
	Path string
	// Size is its size in bytes.
	Size int64
	// Limit is the limit it broke.
	Limit int64
}

// Error names the path, its size, and the limit.
func (e *TooLargeError) Error() string {
	return fmt.Sprintf("`%s` is %d bytes, larger than the %d-byte limit", e.Path, e.Size, e.Limit)
}

// OutsideRootError reports a path that is not inside the store root it had
// to stay under.
type OutsideRootError struct {
	// Path is the refused path.
	Path string
}

// Error names the refused path.
func (e *OutsideRootError) Error() string {
	return fmt.Sprintf("`%s` is outside the namespace root; refusing to write", e.Path)
}

// NotEmptyError reports a directory that had to be empty and was not.
// Reported rather than recursed: removing a tree is never this store's job.
type NotEmptyError struct {
	// Path is the non-empty directory.
	Path string
}

// Error names the directory.
func (e *NotEmptyError) Error() string {
	return fmt.Sprintf("`%s` is not empty", e.Path)
}

// WriteCancelledError reports a write abandoned while its replacement was
// staged but not yet renamed into place. The old file is untouched.
type WriteCancelledError struct {
	// Path is the file that was not replaced.
	Path string
}

// Error names the file that kept its old contents.
func (e *WriteCancelledError) Error() string {
	return fmt.Sprintf("cancelled before `%s` could be replaced", e.Path)
}

// FileSnapshot is enough of a file's identity to notice it changed
// underneath us. Size and mtime alone would miss a same-size rewrite inside
// one mtime granularity; the device and inode catch a replacement.
type FileSnapshot struct {
	// Dev is the device number.
	Dev uint64
	// Ino is the inode number.
	Ino uint64
	// Size is the size in bytes.
	Size int64
	// MtimeNS is the modification time in nanoseconds since the epoch.
	// Nanoseconds since 1970 fit in an int64 until the year 2262.
	MtimeNS int64
}

// ReadOutcome is what one credential read found: nothing, or the bytes
// together with the identity of the file that held them.
type ReadOutcome struct {
	// Present reports whether a file was read at all.
	Present bool
	// Bytes holds the contents when Present.
	Bytes []byte
	// Snap is the file's identity at the moment it was read, when Present.
	Snap FileSnapshot
}

// WriteOutcome is what a write did: the file was replaced (Snap identifies
// it), or the rename failed and the bytes were parked as pending.
type WriteOutcome struct {
	// SavedToPending reports that the rename failed and the bytes wait
	// under the pending name instead.
	SavedToPending bool
	// PendingError carries the rename failure for the user-facing row,
	// only when SavedToPending.
	PendingError string
	// Snap identifies the file that now holds the bytes, when the write
	// landed.
	Snap FileSnapshot
}

// StopPolicy says what a write does when its context is cancelled while the
// replacement is staged but not yet renamed into place.
type StopPolicy int

const (
	// StopDiscardStaged abandons a staged replacement when the context was
	// cancelled: the temporary is unlinked, the old file stays in place,
	// and the write returns a [WriteCancelledError]. The window between
	// the fsync and the rename is the last one in which stopping costs
	// nothing, so an interrupt there must cost nothing.
	StopDiscardStaged StopPolicy = iota + 1
	// StopComplete ignores cancellation once the file is staged, and a
	// failure afterwards keeps the staged bytes on disk: they may be the
	// only copy of a server-rotated grant, and deleting them would cost
	// the user a login.
	StopComplete
)

// writeFaults is the unexported injection seam a test drives a write's
// failure windows through. Production code never sets it, so a release
// build carries no switch that could reach these paths.
type writeFaults struct {
	// beforeRename runs between the temporary's fsync and the rename —
	// the window in which a crash leaves a staged file beside the intact
	// old one. A test that panics here simulates that crash.
	beforeRename func()
	// renameErr, when non-nil, replaces the rename's result with this
	// failure.
	renameErr error
}

// SecretFile is one named file in an already-opened directory, bound to the
// root it must stay under.
//
// [NewSecretFile] is the only constructor and every field is unexported, so
// a value cannot exist without naming its root. Every operation first
// checks that the file's path spells a location strictly below that root
// and that the name is one plain path component. The check is lexical: the
// directory descriptor comes from the caller's no-follow walk, and the leaf
// operation is relative to it, so no path is resolved again here.
type SecretFile struct {
	// root is the directory this file must spell a path strictly below.
	root string
	// dir is the descriptor of the directory holding the file, from the
	// caller's no-follow walk.
	dir int
	// name is the file's name inside dir: one plain path component.
	name string
	// shown is the file's path as the user would recognise it: checked
	// against root, and used in error sentences. Nothing is resolved
	// through it.
	shown string
	// faults is the test-only injection seam; nil in production.
	faults *writeFaults
}

// NewSecretFile binds a file name in the directory dir describes to the
// root it must stay under.
//
// Infallible on purpose: the checks run at the start of every operation,
// so a value that fails them can be built but never used.
func NewSecretFile(root string, dir int, name, shown string) *SecretFile {
	return &SecretFile{root: root, dir: dir, name: name, shown: shown}
}

// check refuses a name that is not one plain component, a displayed path
// that does not end in it, and a displayed path not strictly below the
// root.
func (f *SecretFile) check() error {
	single := config.IsSingleComponent(f.name)
	named := filepath.Base(f.shown) == f.name
	if single && named && lexicallyUnder(f.root, f.shown) {
		return nil
	}
	return &OutsideRootError{Path: f.shown}
}

// Read reads the file under the regular-file and size rules of
// [readFileAt]: a file that exists and cannot be opened reads as absent.
func (f *SecretFile) Read(limit int64) (ReadOutcome, error) {
	if err := f.check(); err != nil {
		return ReadOutcome{}, err
	}
	return readFileAt(f.dir, f.name, limit, f.shown)
}

// ReadStrict is [SecretFile.Read], but a file that exists and cannot be
// opened is an error rather than absent. For a caller whose "absent"
// decides whether a credential is discarded.
func (f *SecretFile) ReadStrict(limit int64) (ReadOutcome, error) {
	if err := f.check(); err != nil {
		return ReadOutcome{}, err
	}
	return readFileAtStrict(f.dir, f.name, limit, f.shown)
}

// ReadSecret reads the file under [SecretFile.Read]'s rules and seals its
// contents into a [Secret], wiping the intermediate buffer. An absent file
// returns a nil secret and no error.
func (f *SecretFile) ReadSecret(limit int64) (*Secret, *FileSnapshot, error) {
	outcome, err := f.Read(limit)
	if err != nil || !outcome.Present {
		return nil, nil, err
	}
	sealed, err := NewSecret(outcome.Bytes)
	if err != nil {
		return nil, nil, err
	}
	snap := outcome.Snap
	return sealed, &snap, nil
}

// Remove removes the file, returning whether one was there. An absent file
// is not an error: the same operation re-run, or a peer that removed it
// first, both land here.
func (f *SecretFile) Remove() (bool, error) {
	if err := f.check(); err != nil {
		return false, err
	}
	err := unlinkAt(f.dir, f.name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, errs.NewIO(fmt.Sprintf("could not remove `%s`", f.shown), err)
	}
}

// Write replaces the file atomically with doc.
//
// The sequence is: an exclusive 0600 temporary `<name>.tmp.<8 hex>`, an
// fsync of the file, the rename, an fsync of the directory. There is no
// in-place fallback: a truncate-then-write of a credential file is a window
// in which a crash leaves no credentials at all, so the old file stays in
// place until the rename replaces it. A failed rename parks the bytes as
// pending when a spec is given; with nil it is an error and the temporary
// is removed under [StopDiscardStaged], because the caller still holds what
// it asked to write — under [StopComplete] the staged bytes are kept.
//
// ctx is consulted only under [StopDiscardStaged], at the one point where
// abandoning leaves the file exactly as it was found.
func (f *SecretFile) Write(ctx context.Context, doc []byte, pending *PendingSpec, stop StopPolicy) (WriteOutcome, error) {
	if err := f.check(); err != nil {
		return WriteOutcome{}, err
	}
	if pending != nil {
		dirShown := filepath.Dir(f.shown)
		if err := checkSpecNames(pending, dirShown); err != nil {
			return WriteOutcome{}, err
		}
		if pending.TargetName != f.name {
			return WriteOutcome{}, &OutsideRootError{Path: filepath.Join(dirShown, pending.TargetName)}
		}
	}

	// Refuse before creating anything: a symlink at the target means
	// somebody else is managing this path and the rename would land
	// somewhere unknown.
	switch kind, err := entryAt(f.dir, f.name, f.shown); {
	case err != nil:
		return WriteOutcome{}, err
	case kind == entrySymlink:
		return WriteOutcome{}, &SymlinkRefusedError{Path: f.shown}
	case kind == entryOther:
		return WriteOutcome{}, &NotRegularError{Path: f.shown}
	}

	tmpName := f.name + ".tmp." + hex8()
	tmpShown := filepath.Join(filepath.Dir(f.shown), tmpName)
	if err := createNewFileAt(f.dir, tmpName, doc); err != nil {
		// EEXIST from the exclusive create means the name belongs to a
		// file this write did not create: under StopComplete that can
		// only be an earlier write's kept temporary, which may hold a
		// rotated grant, so it is left alone.
		foreign := errors.Is(err, fs.ErrExist)
		if !(foreign && stop == StopComplete) {
			_ = unlinkAt(f.dir, tmpName)
		}
		return WriteOutcome{}, errs.NewIO(fmt.Sprintf("could not write `%s`", tmpShown), err)
	}

	if f.faults != nil && f.faults.beforeRename != nil {
		f.faults.beforeRename()
	}

	// The last moment at which a cancelled operation can leave the file
	// exactly as it found it: after the rename there is nothing left to
	// undo, and before the write there is nothing to gain. Under
	// StopComplete the staged bytes may be the only copy of a rotated
	// grant, and stopping here would destroy it.
	if stop == StopDiscardStaged && ctx.Err() != nil {
		_ = unlinkAt(f.dir, tmpName)
		return WriteOutcome{}, &WriteCancelledError{Path: f.shown}
	}

	var renameErr error
	if f.faults != nil && f.faults.renameErr != nil {
		renameErr = f.faults.renameErr
	} else {
		renameErr = unix.Renameat(f.dir, tmpName, f.dir, f.name)
	}
	if renameErr == nil {
		if err := unix.Fsync(f.dir); err != nil {
			// The rename has happened; nothing can be abandoned now.
			slog.Warn("a credential file's directory could not be flushed after the rename")
		}
		// The mode was set on the temporary file's inode, which the
		// rename carries over, so there is nothing left to chmod here.
		snap, err := snapshotAt(f.dir, f.name, f.shown)
		switch {
		case err != nil:
			return WriteOutcome{}, err
		case snap == nil:
			return WriteOutcome{}, errs.NewIO(fmt.Sprintf("`%s` vanished immediately after being written", f.shown), fs.ErrNotExist)
		default:
			return WriteOutcome{Snap: *snap}, nil
		}
	}

	if pending != nil {
		return f.saveToPending(pending, tmpName, renameErr, stop)
	}
	_ = unlinkAt(f.dir, tmpName)
	return WriteOutcome{}, errs.NewIO(fmt.Sprintf("could not rename `%s` onto `%s`", tmpShown, f.shown), renameErr)
}

// Park parks doc as pending without trying the target at all.
//
// For a writer that already knows the rename cannot happen: a rotated grant
// whose writes kept failing after the refresh was applied. The target is
// not examined — a target that is torn, a link, or unreadable is exactly
// why the caller is here — and the temporary is staged and parked under
// [StopComplete]'s rules: a failure to park keeps it, naming it in the
// error, because it may be the only copy of the grant.
func (f *SecretFile) Park(ctx context.Context, doc []byte, spec PendingSpec) (WriteOutcome, error) {
	if err := ctx.Err(); err != nil {
		return WriteOutcome{}, err
	}
	if err := f.check(); err != nil {
		return WriteOutcome{}, err
	}
	dirShown := filepath.Dir(f.shown)
	if err := checkSpecNames(&spec, dirShown); err != nil {
		return WriteOutcome{}, err
	}
	if spec.TargetName != f.name {
		return WriteOutcome{}, &OutsideRootError{Path: filepath.Join(dirShown, spec.TargetName)}
	}
	tmpName := f.name + ".tmp." + hex8()
	tmpShown := filepath.Join(dirShown, tmpName)
	if err := createNewFileAt(f.dir, tmpName, doc); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			_ = unlinkAt(f.dir, tmpName)
		}
		return WriteOutcome{}, errs.NewIO(fmt.Sprintf("could not write `%s`", tmpShown), err)
	}
	if f.faults != nil && f.faults.beforeRename != nil {
		f.faults.beforeRename()
	}
	cause := errors.New("the writes before it failed; parked without a rename")
	return f.saveToPending(&spec, tmpName, cause, StopComplete)
}

// saveToPending parks a written temporary file under the spec's pending
// name.
//
// The metadata goes first, deliberately. A crash between the two leaves a
// meta with no pending file, which the resolver reads as "nothing to
// replay"; the other order would leave credentials with no record of what
// they were derived from, which is unreplayable and indistinguishable from
// a valid pending.
//
// Under [StopComplete] a failure here does not remove the temporary: it
// may hold the only copy of a rotated grant, and a stray temporary is
// something doctor reports.
func (f *SecretFile) saveToPending(spec *PendingSpec, tmpName string, cause error, stop StopPolicy) (WriteOutcome, error) {
	keepTmp := stop == StopComplete
	metaBody, err := metaJSON(spec)
	if err != nil {
		return WriteOutcome{}, err
	}
	dirShown := filepath.Dir(f.shown)
	// Under StopComplete the error names the temporary it kept, so neither
	// the caller nor the user can mistake "the grant is on disk under this
	// name" for "nothing was staged".
	kept := func(context string) string {
		if keepTmp {
			return fmt.Sprintf("%s; the staged credential was kept at `%s`", context, filepath.Join(dirShown, tmpName))
		}
		return context
	}

	metaShown := filepath.Join(dirShown, spec.MetaName)
	_ = unlinkAt(f.dir, spec.MetaName)
	if err := createNewFileAt(f.dir, spec.MetaName, metaBody); err != nil {
		if !keepTmp {
			_ = unlinkAt(f.dir, tmpName)
		}
		return WriteOutcome{}, errs.NewIO(kept(fmt.Sprintf("could not write `%s`", metaShown)), err)
	}

	pendingShown := filepath.Join(dirShown, spec.PendingName)
	_ = unlinkAt(f.dir, spec.PendingName)
	if err := unix.Renameat(f.dir, tmpName, f.dir, spec.PendingName); err != nil {
		if !keepTmp {
			_ = unlinkAt(f.dir, tmpName)
		}
		_ = unlinkAt(f.dir, spec.MetaName)
		return WriteOutcome{}, errs.NewIO(kept(fmt.Sprintf("could not park credentials at `%s`", pendingShown)), err)
	}

	return WriteOutcome{SavedToPending: true, PendingError: cause.Error()}, nil
}

// lexicallyUnder reports whether target spells a path strictly below root
// after folding "." and ".." out of both, without resolving any symbolic
// link. It guarantees what the path says, never where it leads: the escape
// through a symlinked component is closed by the descriptor-relative,
// no-follow operations, and both checks are required.
func lexicallyUnder(root, target string) bool {
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	return cleanTarget != cleanRoot && strings.HasPrefix(cleanTarget, cleanRoot+string(filepath.Separator))
}

// entryKind is what one lstat of a name inside an opened directory found.
type entryKind int

const (
	// entryAbsent means nothing is there.
	entryAbsent entryKind = iota + 1
	// entryRegular means a regular file.
	entryRegular
	// entrySymlink means a symbolic link, whatever it points at.
	entrySymlink
	// entryOther means something else: a directory, a socket, a device.
	entryOther
)

// entryAt lstats one name inside an already-opened directory.
func entryAt(dir int, name, shown string) (entryKind, error) {
	var st unix.Stat_t
	err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			return entryRegular, nil
		case unix.S_IFLNK:
			return entrySymlink, nil
		default:
			return entryOther, nil
		}
	case errors.Is(err, unix.ENOENT):
		return entryAbsent, nil
	default:
		return 0, errs.NewIO(fmt.Sprintf("could not stat `%s`", shown), err)
	}
}

// isSymlinkErrno reports whether an open failure means O_NOFOLLOW met a
// link: ELOOP on Linux and macOS, EMLINK on some BSDs.
func isSymlinkErrno(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK)
}

// snapshotOf is the identity of an already-opened file.
func snapshotOf(info fs.FileInfo) FileSnapshot {
	st, _ := info.Sys().(*syscall.Stat_t)
	snap := FileSnapshot{Size: info.Size(), MtimeNS: info.ModTime().UnixNano()}
	if st != nil {
		snap.Dev = uint64(st.Dev)
		snap.Ino = st.Ino
	}
	return snap
}

// readFileAt opens name relative to an already-opened directory with
// O_NOFOLLOW and reads it, applying the size limit.
//
// ENOENT, EISDIR, ENOTDIR, EACCES and EPERM mean absent — there is nothing
// here to read, which for a fresh namespace is the normal state. Everything
// else means failed, and in particular a symlink or an oversized file is a
// failure, never an absence: "absent" leads to writing a new file over the
// path, and a symlink pointing somewhere else is exactly the case where
// writing would be wrong.
func readFileAt(dir int, name string, limit int64, shown string) (ReadOutcome, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return classifyOpen(shown, err)
	}
	return readOpened(fd, shown, limit)
}

// readFileAtStrict is [readFileAt], except that only ENOENT is absence.
//
// Reading a credential file that exists and cannot be opened as absent is
// right for a store whose own writer never leaves one, and wrong for a
// namespace where an unreadable file can sit beside the only copy of a
// rotated grant: there "absent" would discard it. A link is still a
// [SymlinkRefusedError]; every other open failure names the path.
func readFileAtStrict(dir int, name string, limit int64, shown string) (ReadOutcome, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		return readOpened(fd, shown, limit)
	case errors.Is(err, unix.ENOENT):
		return ReadOutcome{}, nil
	case isSymlinkErrno(err):
		return ReadOutcome{}, &SymlinkRefusedError{Path: shown}
	default:
		return ReadOutcome{}, errs.NewIO(fmt.Sprintf("could not open `%s`", shown), err)
	}
}

// classifyOpen maps an open failure onto absent, refused, or failed.
func classifyOpen(shown string, err error) (ReadOutcome, error) {
	for _, absent := range []error{unix.ENOENT, unix.EISDIR, unix.ENOTDIR, unix.EACCES, unix.EPERM} {
		if errors.Is(err, absent) {
			return ReadOutcome{}, nil
		}
	}
	if isSymlinkErrno(err) {
		return ReadOutcome{}, &SymlinkRefusedError{Path: shown}
	}
	return ReadOutcome{}, errs.NewIO(fmt.Sprintf("could not open `%s`", shown), err)
}

// readOpened applies the regular-file and size rules to an open descriptor,
// consuming it.
func readOpened(fd int, shown string, limit int64) (ReadOutcome, error) {
	file := os.NewFile(uintptr(fd), shown)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return ReadOutcome{}, errs.NewIO(fmt.Sprintf("could not stat `%s`", shown), err)
	}
	if !info.Mode().IsRegular() {
		return ReadOutcome{}, &NotRegularError{Path: shown}
	}
	if info.Size() > limit {
		return ReadOutcome{}, &TooLargeError{Path: shown, Size: info.Size(), Limit: limit}
	}

	// Bounded by one byte past the limit so a file that grew between the
	// stat and the read is caught rather than read unboundedly.
	bytes, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return ReadOutcome{}, errs.NewIO(fmt.Sprintf("could not read `%s`", shown), err)
	}
	if int64(len(bytes)) > limit {
		return ReadOutcome{}, &TooLargeError{Path: shown, Size: int64(len(bytes)), Limit: limit}
	}
	return ReadOutcome{Present: true, Bytes: bytes, Snap: snapshotOf(info)}, nil
}

// snapshotAt is the identity of one name inside an already-opened
// directory, nil when nothing is there.
func snapshotAt(dir int, name, shown string) (*FileSnapshot, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
	case errors.Is(err, unix.ENOENT):
		return nil, nil
	case isSymlinkErrno(err):
		return nil, &SymlinkRefusedError{Path: shown}
	default:
		return nil, errs.NewIO(fmt.Sprintf("could not open `%s`", shown), err)
	}
	file := os.NewFile(uintptr(fd), shown)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, errs.NewIO(fmt.Sprintf("could not stat `%s`", shown), err)
	}
	snap := snapshotOf(info)
	return &snap, nil
}

// unlinkAt removes one name from an already-opened directory.
func unlinkAt(dir int, name string) error {
	return unix.Unlinkat(dir, name, 0)
}

// chmod0600At sets one file's mode to 0600 without following a link at
// name. The link is refused by the open and the mode set on the
// descriptor, so there is no second lookup between the two.
func chmod0600At(dir int, name, shown string) error {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
	case isSymlinkErrno(err):
		return &SymlinkRefusedError{Path: shown}
	default:
		return errs.NewIO(fmt.Sprintf("could not open `%s`", shown), err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchmod(fd, uint32(config.FileMode)); err != nil {
		return errs.NewIO(fmt.Sprintf("could not set the mode of `%s`", shown), err)
	}
	return nil
}

// createNewFileAt creates a file at 0600 with O_EXCL inside an
// already-opened directory, writes it, and fsyncs it.
//
// Openat's mode argument is masked by the process umask, so the mode is
// set again on the descriptor: the file holds a refresh token and is the
// inode a rename will carry into place. O_NOFOLLOW here is about the name
// alone; dir is a descriptor a walk already produced, so there is no path
// left to re-resolve. EEXIST arrives wrapped so [fs.ErrExist] matches,
// which is how a caller tells "that name is taken" from a real failure.
func createNewFileAt(dir int, name string, b []byte) error {
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(config.FileMode))
	if err != nil {
		return err
	}
	if err := unix.Fchmod(fd, uint32(config.FileMode)); err != nil {
		_ = unix.Close(fd)
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write(b); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// hex8 returns eight random lowercase hex digits for a temporary file
// name. Collision is handled by O_EXCL, not by the randomness.
func hex8() string {
	var b [4]byte
	// crypto/rand.Read never fails on supported platforms; it aborts the
	// program instead of returning badly seeded bytes.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
