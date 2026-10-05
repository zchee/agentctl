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
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// ConfigLockErrorKind classifies a configuration lock refusal.
type ConfigLockErrorKind uint8

const (
	// ConfigLockBusy means the lock remained held through every attempt.
	ConfigLockBusy ConfigLockErrorKind = iota + 1
	// ConfigLockStale means the lock is stale and was left untouched.
	ConfigLockStale
	// ConfigLockCancelled means cancellation interrupted a wait.
	ConfigLockCancelled
	// ConfigLockCompromised means a held lock's modification time changed.
	ConfigLockCompromised
	// ConfigLockUnreachable means the literal parent could not be opened.
	ConfigLockUnreachable
	// ConfigLockIO means a directory operation failed without contention.
	ConfigLockIO
)

// ConfigLockError is why a configuration lock could not be taken or trusted.
type ConfigLockError struct {
	Kind    ConfigLockErrorKind
	Age     time.Duration
	Path    string
	Message string
	Err     error
}

// Error implements the error interface.
func (e *ConfigLockError) Error() string {
	switch e.Kind {
	case ConfigLockBusy:
		return "a Claude Code session held the configuration lock through every retry"
	case ConfigLockStale:
		return fmt.Sprintf("the configuration lock is %d ms old, past the peer's staleness window; agentctl never breaks it", e.Age.Milliseconds())
	case ConfigLockCancelled:
		return "cancelled while waiting for the configuration lock"
	case ConfigLockCompromised:
		return fmt.Sprintf("`%s` was modified while agentctl held it; the lock is compromised", e.Path)
	case ConfigLockUnreachable:
		return fmt.Sprintf("`%s` cannot be locked: %s", e.Path, e.Message)
	default:
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
}

// Unwrap exposes the cancellation or filesystem error.
func (e *ConfigLockError) Unwrap() error { return e.Err }

// ConfigHold is a configuration lock beside the literal configuration path.
// Release must be called when the caller finishes. The caller checks Elapsed
// against ConfigHoldBudget across its whole read/write sequence; DriftCheck
// checks only the lock's modification time.
type ConfigHold struct {
	at         LockSlot
	mtime      time.Time
	readable   bool
	acquiredAt time.Duration
	clock      Clock
	fs         LockFS
	unregister func() bool
	mu         sync.Mutex
	temp       *LockSlot
	released   bool
}

// Elapsed returns the monotonic time since the successful mkdir.
func (h *ConfigHold) Elapsed() time.Duration {
	return max(h.clock.Monotonic()-h.acquiredAt, 0)
}

// Shown returns the lock's literal path, for messages only.
func (h *ConfigHold) Shown() string { return h.at.Shown }

// DriftCheck refuses an unreadable or changed modification time.
func (h *ConfigHold) DriftCheck() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.released {
		mtime, readable := h.fs.Mtime(h.at)
		if h.readable && readable && mtime.Equal(h.mtime) {
			return nil
		}
	}
	return &ConfigLockError{Kind: ConfigLockCompromised, Path: h.at.Shown}
}

// TrackTemp registers a temporary file before its creation. It duplicates dir
// so cleanup remains valid if the caller closes its descriptor. name must be
// one plain component. The caller retains ownership of dir even on failure.
func (h *ConfigHold) TrackTemp(dir int, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || !config.IsSingleComponent(name) {
		return &ConfigLockError{Kind: ConfigLockIO, Message: "could not track configuration temporary file", Err: unix.EINVAL}
	}
	fd, err := unix.FcntlInt(uintptr(dir), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return &ConfigLockError{Kind: ConfigLockIO, Message: "could not retain configuration temporary directory", Err: err}
	}
	h.clearTemp(false)
	h.temp = &LockSlot{Dir: fd, Name: name}
	return nil
}

// UntrackTemp withdraws a temporary file once it was renamed or removed.
func (h *ConfigHold) UntrackTemp() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearTemp(false)
}

// Release unlinks any tracked temporary file and gives back the lock. It is
// idempotent and never removes a lock emergency cleanup already released.
func (h *ConfigHold) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	h.clearTemp(true)
	// A consumed registration owns the descriptor until its callback finishes.
	// Removing again could delete the lock a peer took after emergency cleanup.
	if h.unregister != nil && !h.unregister() {
		return
	}
	h.releaseLocked()
}

func (h *ConfigHold) releaseLocked() {
	h.released = true
	h.clearTemp(true)
	_ = h.fs.Rmdir(h.at)
	_ = unix.Close(h.at.Dir)
}

func (h *ConfigHold) clearTemp(unlink bool) {
	if h.temp != nil {
		if unlink {
			_ = unix.Unlinkat(h.temp.Dir, h.temp.Name, 0)
		}
		_ = unix.Close(h.temp.Dir)
		h.temp = nil
	}
}

// AcquireConfigLock takes one lock beside configPath, not its symlink target.
// A held lock gets three waits of 200, 400 and 800 milliseconds, each with
// jitter below that rung. Stale locks are reported and never removed.
func AcquireConfigLock(ctx context.Context, configPath string, seams *Seams) (*ConfigHold, error) {
	return acquireConfigLock(ctx, configPath, seams, true)
}

// TryConfigLock makes exactly one nonblocking mkdir attempt. It never sleeps
// or retries a vanished lock; callers can read without the lock on refusal.
func TryConfigLock(ctx context.Context, configPath string, seams *Seams) (*ConfigHold, error) {
	return acquireConfigLock(ctx, configPath, seams, false)
}

func acquireConfigLock(ctx context.Context, configPath string, seams *Seams, wait bool) (*ConfigHold, error) {
	at, err := openConfigLockSlot(configPath)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			_ = unix.Close(at.Dir)
		}
	}()
	ladder := [...]time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	rung, retriedVanished := 0, false
	for {
		err := seams.FS.Mkdir(at)
		switch {
		case err == nil:
			hold := &ConfigHold{at: at, acquiredAt: seams.Clock.Monotonic(), clock: seams.Clock, fs: seams.FS}
			hold.mtime, hold.readable = seams.FS.Mtime(at)
			if seams.Cleanup != nil {
				hold.unregister = seams.Cleanup.Register(func() {
					hold.mu.Lock()
					defer hold.mu.Unlock()
					hold.releaseLocked()
				})
			}
			owned = true
			return hold, nil
		case errors.Is(err, ErrLockExists):
			mtime, present := seams.FS.Mtime(at)
			if present {
				age := elapsedSince(seams.Clock.Wall(), mtime)
				if age >= ConfigProfile().Stale {
					return nil, &ConfigLockError{Kind: ConfigLockStale, Age: age}
				}
			} else if wait && !retriedVanished {
				retriedVanished = true
				continue
			}
		case errors.Is(err, ErrLockGone):
			return nil, &ConfigLockError{Kind: ConfigLockUnreachable, Path: filepath.Dir(configPath), Message: "the directory went away", Err: err}
		default:
			return nil, &ConfigLockError{Kind: ConfigLockIO, Path: at.Shown, Message: fmt.Sprintf("could not create `%s`", at.Shown), Err: err}
		}
		if !wait || rung == len(ladder) {
			return nil, &ConfigLockError{Kind: ConfigLockBusy}
		}
		retriedVanished = false
		nap := ladder[rung] + seams.Clock.Jitter(ladder[rung])
		rung++
		if err := seams.Clock.Sleep(ctx, nap); err != nil {
			return nil, &ConfigLockError{Kind: ConfigLockCancelled, Err: err}
		}
		if err := ctx.Err(); err != nil {
			return nil, &ConfigLockError{Kind: ConfigLockCancelled, Err: err}
		}
	}
}

func openConfigLockSlot(configPath string) (LockSlot, error) {
	name := filepath.Base(configPath)
	if !config.IsSingleComponent(name) {
		return LockSlot{}, &ConfigLockError{Kind: ConfigLockUnreachable, Path: configPath, Message: "the configuration path has no directory and file name"}
	}
	parent := filepath.Dir(configPath)
	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return LockSlot{}, &ConfigLockError{Kind: ConfigLockUnreachable, Path: parent, Message: fmt.Sprintf("its directory could not be opened: %v", err), Err: err}
	}
	name += ".lock"
	return LockSlot{Dir: fd, Name: name, Shown: filepath.Join(parent, name)}, nil
}
