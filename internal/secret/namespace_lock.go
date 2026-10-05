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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// NamespaceLockRetry is how often a blocked acquisition retries.
const NamespaceLockRetry = 250 * time.Millisecond

// NamespaceLockWait is the default wait for the interactive commands,
// which have no pass deadline of their own.
const NamespaceLockWait = 5 * time.Second

// LockCreateAttempts is how many times opening a lock file re-looks before
// giving up. Each round costs two openat calls and only happens while
// another process is creating the same lock file, so a handful is
// generous; the bound is there so churn cannot spin forever.
const LockCreateAttempts = 8

// ErrLockBusy reports that somebody else holds the lock and the deadline
// passed while waiting. The caller must re-read before deciding anything:
// losing the race usually means the winner just refreshed what this
// process wanted to write.
var ErrLockBusy = errors.New("another process holds the namespace lock")

// RefusedSymlinkError reports a symbolic link where a lock file or the
// locks directory should be. A link there is somebody else deciding where
// this store's locks live, which is a decision to refuse rather than to
// follow.
type RefusedSymlinkError struct {
	// Path is where the link was found.
	Path string
}

// Error names the refused path.
func (e *RefusedSymlinkError) Error() string {
	return fmt.Sprintf("`%s` is a symbolic link; refusing to lock through it", e.Path)
}

// LockUnavailableError reports that locking failed for a reason that is
// not contention. The caller must not write: a filesystem that cannot
// flock is one agentctl declines to refresh on, rather than one it
// silently refreshes on without mutual exclusion.
type LockUnavailableError struct {
	// Reason says what failed, in the user's terms.
	Reason string
	// Err is the underlying cause, when one exists.
	Err error
}

// Error states why the lock is unavailable.
func (e *LockUnavailableError) Error() string {
	return "the namespace lock is unavailable: " + e.Reason
}

// Unwrap returns the underlying cause.
func (e *LockUnavailableError) Unwrap() error {
	return e.Err
}

// LockGuard is an acquired lock.
//
// The exclusive flock is released by [LockGuard.Release], and by the
// kernel if the process dies — which is what makes a crash during a
// refresh safe: the next process can take the lock immediately, and what
// it finds on disk is either the old state or a pending file, never a
// half-written one. The lock file itself is never unlinked: flock locks an
// inode, and a recreated file is a second inode two processes could hold
// at once.
type LockGuard struct {
	file *os.File
	path string
}

// Path returns the lock file this guard holds.
func (g *LockGuard) Path() string {
	return g.path
}

// Release unlocks and closes the lock file, never unlinking it. Releasing
// an already-released guard is a no-op.
func (g *LockGuard) Release() error {
	if g.file == nil {
		return nil
	}
	file := g.file
	g.file = nil
	// Best effort: closing the descriptor releases the lock anyway, and
	// there is no holder left to tell about a failure.
	_ = flock(int(file.Fd()), unix.LOCK_UN)
	return file.Close()
}

// flock is replaceable so a test can prove that an errno other than
// EWOULDBLOCK fails closed instead of retrying; production code never
// changes it.
var flock = unix.Flock

// Acquire takes the exclusive lock on the file name inside locksDir,
// creating the directory at 0700 when it is missing.
//
// The directory is opened without following symbolic links and the lock
// file is taken relative to that descriptor, so there is no second path
// resolution between checking the directory and using it; a link at
// either level is refused as a [RefusedSymlinkError]. A blocked
// acquisition retries every [NamespaceLockRetry] until deadline, then
// reports [ErrLockBusy]; ctx cancellation is noticed immediately rather
// than at the end of the next interval. A name that is not one plain path
// component is refused before anything is created. The acquired lock's
// body records this holder — pid, start-time identity, acquisition time —
// so doctor can tell a live holder from a recycled pid; a holder that
// cannot record itself releases the lock and reports the failure, because
// a silent lock is exactly what the body exists to prevent.
func Acquire(ctx context.Context, locksDir, name string, deadline time.Time) (*LockGuard, error) {
	if !config.IsSingleComponent(name) {
		return nil, &LockUnavailableError{Reason: fmt.Sprintf("`%s` does not name a lock file", locksDir+string(filepath.Separator)+name)}
	}
	dirFD, err := openLocksDir(locksDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirFD) }()
	guard, err := lockAt(ctx, dirFD, name, filepath.Join(locksDir, name), deadline)
	if err != nil {
		return nil, err
	}
	if err := writeBody(ctx, guard); err != nil {
		_ = guard.Release()
		return nil, err
	}
	return guard, nil
}

// LockFile takes an exclusive flock on path, creating the file if needed,
// with [Acquire]'s waiting rules.
//
// Unlike [Acquire], the parent directory is resolved normally: the one
// caller is the registry's configuration lock, whose parent is the
// configuration directory — a directory the user names, and one a dotfile
// manager may well have made a symbolic link. Everything else in the
// store follows that link, so refusing it only here would half-support a
// layout rather than support or reject it.
func LockFile(ctx context.Context, path string, deadline time.Time) (*LockGuard, error) {
	parent := filepath.Dir(path)
	name := filepath.Base(path)
	if !config.IsSingleComponent(name) {
		return nil, &LockUnavailableError{Reason: fmt.Sprintf("`%s` does not name a lock file", path)}
	}
	dirFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &LockUnavailableError{Reason: fmt.Sprintf("could not open `%s`: %v", parent, err), Err: err}
	}
	defer func() { _ = unix.Close(dirFD) }()
	return lockAt(ctx, dirFD, name, path, deadline)
}

// lockAt takes the exclusive flock on name inside an already-opened
// directory, retrying contention until deadline and failing closed on
// every other error.
func lockAt(ctx context.Context, dirFD int, name, path string, deadline time.Time) (*LockGuard, error) {
	file, err := openLockFile(dirFD, name, path)
	if err != nil {
		return nil, err
	}

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		err := flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return &LockGuard{file: file, path: path}, nil
		case errors.Is(err, unix.EWOULDBLOCK):
		default:
			// Fail closed: an errno that is not contention — ENOTSUP from a
			// filesystem without flock included — never authorises a write.
			closeDiscard(file)
			return nil, &LockUnavailableError{Reason: fmt.Sprintf("could not lock `%s`: %v", path, err), Err: err}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			closeDiscard(file)
			return nil, fmt.Errorf("cancelled while waiting for the namespace lock: %w", ctxErr)
		}
		if !time.Now().Before(deadline) {
			closeDiscard(file)
			return nil, ErrLockBusy
		}
		if timer == nil {
			timer = time.NewTimer(NamespaceLockRetry)
		} else {
			timer.Reset(NamespaceLockRetry)
		}
		select {
		case <-ctx.Done():
			closeDiscard(file)
			return nil, fmt.Errorf("cancelled while waiting for the namespace lock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// closeDiscard closes a file whose close error has nowhere useful to go:
// the caller is already returning a more precise failure.
func closeDiscard(file *os.File) {
	_ = file.Close()
}

// openLockFile opens the lock file inside the directory dirFD names,
// creating it at 0600 if it is not there yet.
//
// Deliberately not one openat with O_CREAT. Darwin does not retry
// openat's lookup when another thread wins the create race: two processes
// reaching a fresh lock file at the same moment make it return ENOENT —
// not EEXIST — a large fraction of the time. So the create is split: open
// what is there, and only when there is nothing there create it with
// O_EXCL, reading EEXIST as "somebody just made it" and looking again. A
// planted symbolic link is refused either way round: O_NOFOLLOW refuses
// it on the open of an existing file, and the exclusive create reports it
// as EEXIST, which sends us back to the open that reports it properly.
func openLockFile(dirFD int, name, path string) (*os.File, error) {
	for range LockCreateAttempts {
		fd, err := unix.Openat(dirFD, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		switch {
		case err == nil:
			return os.NewFile(uintptr(fd), path), nil
		case errors.Is(err, unix.ENOENT):
		default:
			return nil, openLockError(err, path)
		}
		fd, err = unix.Openat(dirFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, uint32(config.FileMode))
		switch {
		case err == nil:
			return os.NewFile(uintptr(fd), path), nil
		case errors.Is(err, unix.EEXIST):
		default:
			return nil, openLockError(err, path)
		}
	}
	return nil, &LockUnavailableError{Reason: fmt.Sprintf("could not open `%s`: it kept being created and removed underneath us", path)}
}

// openLockError maps an openat failure on the lock file onto the
// package's lock errors.
func openLockError(err error, path string) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
		return &RefusedSymlinkError{Path: path}
	}
	return &LockUnavailableError{Reason: fmt.Sprintf("could not open `%s`: %v", path, err), Err: err}
}

// openLocksDir creates the locks directory at 0700 when it is missing,
// refusing a symbolic link, and returns a descriptor opened without
// following links.
//
// os.Stat follows links, so it would happily accept a locks directory
// pointing anywhere and then create every lock file there. The check is
// therefore an lstat; the O_NOFOLLOW open closes the window the lstat
// leaves, because a link swapped in between the two would otherwise be
// followed by a second resolution, and there is no second resolution once
// the descriptor is held.
func openLocksDir(dir string) (int, error) {
	info, err := os.Lstat(dir)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		return -1, &RefusedSymlinkError{Path: dir}
	case err == nil && info.IsDir():
	case err == nil:
		return -1, &LockUnavailableError{Reason: fmt.Sprintf("`%s` is not a directory", dir)}
	case errors.Is(err, fs.ErrNotExist):
		if mkdirErr := os.MkdirAll(dir, config.DirMode); mkdirErr != nil {
			return -1, &LockUnavailableError{Reason: fmt.Sprintf("could not create `%s`: %v", dir, mkdirErr), Err: mkdirErr}
		}
	default:
		return -1, &LockUnavailableError{Reason: fmt.Sprintf("could not stat `%s`: %v", dir, err), Err: err}
	}

	// Darwin reports O_DIRECTORY|O_NOFOLLOW on a link as ENOTDIR, so that
	// errno joins ELOOP here — the lstat above already established that a
	// directory was there a moment ago.
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) || errors.Is(err, unix.ENOTDIR) {
			return -1, &RefusedSymlinkError{Path: dir}
		}
		return -1, &LockUnavailableError{Reason: fmt.Sprintf("could not open `%s`: %v", dir, err), Err: err}
	}
	return fd, nil
}
