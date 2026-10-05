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
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ErrLockExists reports that somebody else holds a lock: the mkdir found
// the directory already there.
var ErrLockExists = errors.New("the lock directory already exists")

// ErrLockGone reports that a lock artefact is not there: the rmdir or
// stat found nothing, or a mkdir found its parent missing.
var ErrLockGone = errors.New("the lock directory is not there")

// lockDirMode is the mode every lock directory is created with, matching
// the directory mode the rest of the store uses.
const lockDirMode = 0o700

// LockSlot is one lock artefact, addressed the way it is operated on: a
// directory descriptor and a name, never a path.
//
// A path is re-resolved from the root on every system call, and a
// symbolic link planted anywhere along it redirects the operation into a
// directory of somebody else's choosing — the live store's lock being
// the obvious target. The descriptor is opened once per acquisition by a
// no-follow component walk and kept for the life of the hold, so the
// directory a removal lands in is the same inode the creation landed in.
type LockSlot struct {
	// Dir is the descriptor of the directory the artefact lives in.
	Dir int
	// Name is the artefact's own name inside Dir: exactly one component.
	Name string
	// Shown is what the artefact is called in a record or a message.
	// Carried rather than derived so a test's spy can record a timeline
	// a reader recognises; no operation is ever performed on it.
	Shown string
}

// LockFS is the three directory operations a peer lock is made of, all
// relative to an already-opened directory.
//
// An interface rather than three functions so a spy can record the
// sequence of operations: "the storage-write mutex is attempted once"
// and "no wait happens while anything is held" are claims about the
// sequence, and no after-the-fact inspection of the filesystem can check
// them.
type LockFS interface {
	// Mkdir makes one non-blocking mkdir at 0700. It returns
	// [ErrLockExists] when somebody already holds the lock and
	// [ErrLockGone] when the parent directory is missing.
	Mkdir(at LockSlot) error
	// Rmdir removes the directory, refusing anything that is not an
	// empty directory, so a regular file or a symbolic link at the
	// artefact's name is never removed. It returns [ErrLockGone] when
	// the directory has already gone.
	Rmdir(at LockSlot) error
	// Mtime returns the artefact's modification time at full nanosecond
	// resolution, or false when it is not there. The read never follows
	// a symbolic link at the artefact's name: a link where a lock
	// directory should be is somebody else's plant, and reading its
	// target's time would hand the sampling to the planter.
	Mtime(at LockSlot) (time.Time, bool)
}

// RealFS is the real directory operations.
type RealFS struct{}

// Mkdir implements [LockFS].
func (RealFS) Mkdir(at LockSlot) error {
	switch err := unix.Mkdirat(at.Dir, at.Name, lockDirMode); err {
	case nil:
		return nil
	case unix.EEXIST:
		return ErrLockExists
	case unix.ENOENT:
		return ErrLockGone
	default:
		return &LockIOError{Context: fmt.Sprintf("could not create `%s`", at.Shown), Message: err.Error()}
	}
}

// Rmdir implements [LockFS]. The directory-only removal is the point:
// it refuses both a non-empty directory and anything that is not a
// directory at all, so a file or a link at the artefact's name survives.
func (RealFS) Rmdir(at LockSlot) error {
	switch err := unix.Unlinkat(at.Dir, at.Name, unix.AT_REMOVEDIR); err {
	case nil:
		return nil
	case unix.ENOENT:
		return ErrLockGone
	default:
		return &LockIOError{Context: fmt.Sprintf("could not remove `%s`", at.Shown), Message: err.Error()}
	}
}

// Mtime implements [LockFS].
func (RealFS) Mtime(at LockSlot) (time.Time, bool) {
	var stat unix.Stat_t
	if err := unix.Fstatat(at.Dir, at.Name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return time.Time{}, false
	}
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec), true
}

// closeLockFD closes a descriptor, ignoring the error: every caller holds
// the descriptor read-only, so there is nothing a failed close could
// lose.
func closeLockFD(fd int) {
	_ = unix.Close(fd)
}

// lockPathComponentsUnder returns target's path components below anchor, or
// an error when target is not anchor or lexically below it.
func lockPathComponentsUnder(anchor, target string) ([]string, error) {
	cleanAnchor := filepath.Clean(anchor)
	cleanTarget := filepath.Clean(target)
	if cleanTarget == cleanAnchor {
		return nil, nil
	}
	prefix := cleanAnchor + string(filepath.Separator)
	rel, found := strings.CutPrefix(cleanTarget, prefix)
	if !found || rel == "" {
		return nil, fmt.Errorf("`%s` is not under `%s`", target, anchor)
	}
	components := strings.Split(rel, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("`%s` holds a path component that does not name a directory entry", target)
		}
	}
	return components, nil
}

// openLockWalkRoot opens the walk's starting directory. Links on the way to
// the anchor are followed: the anchor is a directory this store already
// trusts — its own namespace root, whose configuration directory a
// dotfile manager may legitimately have made a symbolic link, or a live
// store parent that was resolved deliberately — and everything below it
// is what the component walk protects.
func openLockWalkRoot(anchor string) (int, error) {
	fd, err := unix.Open(anchor, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("opening `%s`: %w", anchor, err)
	}
	return fd, nil
}

// lockOpenDirUnder walks from anchor down to target one no-follow component
// at a time and returns target's directory descriptor. A symbolic link
// at any component below the anchor is refused before anything is
// touched through it.
func lockOpenDirUnder(anchor, target string) (int, error) {
	components, err := lockPathComponentsUnder(anchor, target)
	if err != nil {
		return -1, err
	}
	fd, err := openLockWalkRoot(anchor)
	if err != nil {
		return -1, err
	}
	for _, component := range components {
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeLockFD(fd)
		if err != nil {
			return -1, fmt.Errorf("opening `%s` under `%s` without following links: %w", component, anchor, err)
		}
		fd = next
	}
	return fd, nil
}

// lockCreateDirUnder walks from anchor down to target like [lockOpenDirUnder],
// creating any missing component at 0700 on the way. A component that
// exists is opened no-follow, so a symbolic link planted at one of the
// names is refused rather than followed — including a link left at the
// final name, which would otherwise redirect every file later created
// relative to the returned descriptor.
func lockCreateDirUnder(anchor, target string) (int, error) {
	components, err := lockPathComponentsUnder(anchor, target)
	if err != nil {
		return -1, err
	}
	fd, err := openLockWalkRoot(anchor)
	if err != nil {
		return -1, err
	}
	for _, component := range components {
		if err := unix.Mkdirat(fd, component, lockDirMode); err != nil && err != unix.EEXIST {
			closeLockFD(fd)
			return -1, fmt.Errorf("creating `%s` under `%s`: %w", component, anchor, err)
		}
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeLockFD(fd)
		if err != nil {
			return -1, fmt.Errorf("opening `%s` under `%s` without following links: %w", component, anchor, err)
		}
		fd = next
	}
	return fd, nil
}

// lockCreateNewFileAt creates name inside the directory dirFD refers to —
// exclusively, without following a link at the name, at 0600 — writes
// data and flushes it to disk before returning, because the one caller
// writes a record whose whole point is to survive the crash that
// happens next. It returns [ErrLockExists] when the name is taken.
func lockCreateNewFileAt(dirFD int, name string, data []byte) error {
	fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if err == unix.EEXIST {
			return ErrLockExists
		}
		return fmt.Errorf("creating `%s`: %w", name, err)
	}
	defer closeLockFD(fd)
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			return fmt.Errorf("writing `%s`: %w", name, err)
		}
		data = data[n:]
	}
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("flushing `%s`: %w", name, err)
	}
	return nil
}

// lockReadFileAt reads name inside the directory dirFD refers to, refusing a
// symbolic link at the name, anything that is not a regular file, and a
// file larger than maxBytes — a bounded read, so one oversized file
// dropped into the directory cannot turn a report into an unbounded
// read.
func lockReadFileAt(dirFD int, name string, maxBytes int64) ([]byte, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening `%s`: %w", name, err)
	}
	defer closeLockFD(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("examining `%s`: %w", name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("`%s` is not a regular file", name)
	}
	if stat.Size > maxBytes {
		return nil, fmt.Errorf("`%s` holds %d bytes, more than the %d this reader accepts", name, stat.Size, maxBytes)
	}
	data := make([]byte, 0, stat.Size)
	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			return nil, fmt.Errorf("reading `%s`: %w", name, err)
		}
		if n == 0 {
			return data, nil
		}
		data = append(data, buf[:n]...)
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("`%s` grew past the %d bytes this reader accepts", name, maxBytes)
		}
	}
}
