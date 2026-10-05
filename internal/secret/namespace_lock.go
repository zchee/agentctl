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
	"fmt"
	"path/filepath"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/lockfile"
)

// NamespaceLockRetry is how often a blocked acquisition retries.
const NamespaceLockRetry = lockfile.RetryInterval

// NamespaceLockWait is the default wait for the interactive commands,
// which have no pass deadline of their own.
const NamespaceLockWait = 5 * time.Second

// LockCreateAttempts is how many times opening a lock file re-looks before
// giving up while another process is creating the same lock file.
const LockCreateAttempts = lockfile.CreateAttempts

// ErrLockBusy reports that somebody else holds the lock and the deadline
// passed while waiting. The caller must re-read before deciding anything:
// losing the race usually means the winner just refreshed what this
// process wanted to write.
var ErrLockBusy = lockfile.ErrLockBusy

// RefusedSymlinkError reports a symbolic link where a lock file or the
// locks directory should be.
type RefusedSymlinkError = lockfile.RefusedSymlinkError

// LockUnavailableError reports that locking failed for a reason that is
// not contention. The caller must not write.
type LockUnavailableError = lockfile.LockUnavailableError

// LockGuard is an acquired lock, released by [LockGuard.Release] and by
// the kernel if the process dies. The lock file is never unlinked.
type LockGuard = lockfile.LockGuard

// Acquire takes the exclusive lock on the file name inside locksDir,
// creating the directory at 0700 when it is missing.
//
// The waiting, creation and symlink-refusal rules are the store's shared
// flock rules; a name that is not one plain path component is refused
// before anything is created. The acquired lock's body records this
// holder — pid, start-time identity, acquisition time — so doctor can
// tell a live holder from a recycled pid; a holder that cannot record
// itself releases the lock and reports the failure, because a silent lock
// is exactly what the body exists to prevent.
func Acquire(ctx context.Context, locksDir, name string, deadline time.Time) (*LockGuard, error) {
	if !config.IsSingleComponent(name) {
		return nil, &LockUnavailableError{Reason: fmt.Sprintf("`%s` does not name a lock file", locksDir+string(filepath.Separator)+name)}
	}
	if err := namespaceLockFault(filepath.Join(locksDir, name)); err != nil {
		return nil, err
	}
	guard, err := lockfile.LockIn(ctx, locksDir, name, deadline)
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
// with [Acquire]'s waiting rules but no holder body.
//
// Unlike [Acquire], the parent directory is resolved normally: the one
// caller is the registry's configuration lock, whose parent is the
// configuration directory — a directory the user names, and one a dotfile
// manager may well have made a symbolic link. Everything else in the
// store follows that link, so refusing it only here would half-support a
// layout rather than support or reject it.
func LockFile(ctx context.Context, path string, deadline time.Time) (*LockGuard, error) {
	if !config.IsSingleComponent(filepath.Base(path)) {
		return nil, &LockUnavailableError{Reason: fmt.Sprintf("`%s` does not name a lock file", path)}
	}
	return lockfile.Lock(ctx, path, deadline)
}
