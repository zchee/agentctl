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

package codex

import (
	"context"
	"crypto/rand"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
)

func (l *Lock) withLive(paths *config.Paths, user, account string, fn func() error) error {
	if l == nil || l.state == nil || paths == nil {
		return errInvalidProof
	}
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	expected, err := paths.CodexLockPath(user, account)
	if err != nil || !l.Valid() || l.state.user != user || l.state.account != account || filepath.Clean(l.Path()) != filepath.Clean(expected) {
		return errInvalidProof
	}
	return fn()
}

func (s *RefreshStateStore) loadForUpdate(ctx context.Context) (RefreshState, error) {
	read := s.Load(ctx)
	switch read.Kind {
	case RefreshStatePresent:
		return read.State, nil
	case RefreshStateAbsent:
		return NewRefreshState(), nil
	default:
		return RefreshState{}, errs.NewConfig(read.Reason)
	}
}

// storeDurable is called only inside the store's live-lock lease.
// The directory flush is mandatory: a renamed but unflushed marker cannot authorize a POST.
func (s *RefreshStateStore) storeDurable(ctx context.Context, state RefreshState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return errs.NewConfig("could not serialize Codex refresh state")
	}
	dir, err := secret.CreateDirUnder(s.root, s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	tmp := s.name + ".tmp." + rand.Text()
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("could not create refresh-state temporary: %w", err)
	}
	file := os.NewFile(uintptr(fd), tmp)
	defer func() { _ = file.Close(); _ = unix.Unlinkat(dir, tmp, 0) }()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("could not protect refresh-state temporary: %w", err)
	}
	active := fault.Active()
	if active.Is("codex_refresh_state_write") {
		return errors.New("could not write refresh-state temporary")
	}
	n, err := file.Write(body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("could not write refresh-state temporary: %w", err)
	}
	if active.Is("codex_refresh_state_file_sync") {
		return errors.New("could not flush refresh-state temporary")
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("could not flush refresh-state temporary: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("could not close refresh-state temporary: %w", err)
	}
	active.PausePoint("codex_refresh_state_before_rename")
	if err := ctx.Err(); err != nil {
		return err
	}
	if active.Is("codex_refresh_state_rename") {
		return errors.New("could not replace Codex refresh state")
	}
	if err := unix.Renameat(dir, tmp, dir, s.name); err != nil {
		return fmt.Errorf("could not replace Codex refresh state: %w", err)
	}
	if active.Is("codex_refresh_state_dir_sync") {
		return errors.New("could not flush Codex refresh-state directory")
	}
	if err := unix.Fsync(dir); err != nil {
		return fmt.Errorf("could not flush Codex refresh-state directory: %w", err)
	}
	return nil
}

func (s *RefreshStateStore) update(ctx context.Context, guard *Lock, change func(*RefreshState) (bool, error)) error {
	if guard == nil {
		return errs.NewConfig("Codex refresh state requires a live namespace lock")
	}
	return guard.withLive(s.paths, s.user, s.account, func() error {
		state, err := s.loadForUpdate(ctx)
		if err != nil {
			return err
		}
		changed, err := change(&state)
		if err != nil || !changed {
			return err
		}
		return s.storeDurable(ctx, state)
	})
}

func (s *RefreshStateStore) writeInflight(ctx context.Context, credentials *LockedCredentials) (*InflightToken, error) {
	return s.beginSend(ctx, credentials, false)
}

func (s *RefreshStateStore) writeResend(ctx context.Context, credentials *LockedCredentials) (*InflightToken, error) {
	return s.beginSend(ctx, credentials, true)
}

func (s *RefreshStateStore) beginSend(ctx context.Context, credentials *LockedCredentials, resend bool) (*InflightToken, error) {
	if credentials == nil || !credentials.Valid() {
		return nil, errs.NewConfig("Codex refresh state requires locked credentials")
	}
	user, account := credentials.IDs()
	if user != s.user || account != s.account {
		return nil, errs.NewConfig("credentials from another namespace cannot authorize this refresh marker")
	}
	digest, ok := credentials.RefreshDigest8()
	if !ok {
		return nil, errs.NewConfig("no refresh token to record in Codex refresh state")
	}
	guard := credentials.Lock()
	var token *InflightToken
	err := guard.withLive(s.paths, s.user, s.account, func() error {
		state, err := s.loadForUpdate(ctx)
		if err != nil {
			return err
		}
		if resend {
			if state.Resent {
				return errs.NewConfig("Codex refresh state has already been re-sent once; run `agentctl codex login`")
			}
			if state.Inflight == nil || state.Inflight.SentDigest8 != digest || state.Class == nil {
				return errs.NewConfig("Codex refresh state records no unknown refresh for this grant")
			}
		} else if state.Inflight != nil {
			return errs.NewConfig("Codex refresh state already records a refresh in flight; refusing to send another")
		}
		now := time.Now().UTC()
		state.Inflight = &Inflight{SentDigest8: digest, SentAt: now}
		state.LastSentAt = new(now)
		if resend {
			state.Resent = true
		} else {
			state.AmbiguousSince, state.Class, state.RetryAfter = nil, nil, nil
			state.Resent = false
		}
		if err := s.storeDurable(ctx, state); err != nil {
			return err
		}
		token = &InflightToken{authorization: &inflightAuthorization{digest8: digest, lock: guard}}
		return nil
	})
	return token, err
}

func (s *RefreshStateStore) markInterrupted(ctx context.Context, guard *Lock) error {
	return s.update(ctx, guard, func(state *RefreshState) (bool, error) {
		if state.Inflight == nil || state.Class != nil {
			return false, nil
		}
		state.AmbiguousSince = new(state.Inflight.SentAt)
		state.Class = new(RefreshUnknownInterrupted)
		return true, nil
	})
}

func (s *RefreshStateStore) markUnknown(ctx context.Context, guard *Lock, class RefreshUnknownClass, now time.Time, retryAfter *time.Duration) error {
	if class.Label() == "" {
		return errs.NewConfig("unknown Codex refresh classification")
	}
	return s.update(ctx, guard, func(state *RefreshState) (bool, error) {
		if state.Inflight == nil {
			return false, errs.NewConfig("Codex refresh state records no refresh in flight to mark unknown")
		}
		if state.AmbiguousSince == nil {
			state.AmbiguousSince = new(now.UTC())
		}
		state.Class = new(class)
		state.RetryAfter = nil
		if retryAfter != nil && *retryAfter >= 0 {
			state.RetryAfter = new(uint64(*retryAfter / time.Second))
		}
		return true, nil
	})
}

func (s *RefreshStateStore) clearInflight(ctx context.Context, guard *Lock) error {
	return s.update(ctx, guard, func(state *RefreshState) (bool, error) {
		state.Inflight, state.AmbiguousSince, state.Class, state.RetryAfter = nil, nil, nil, nil
		state.Resent = false
		return true, nil
	})
}

func (s *RefreshStateStore) settleInflight(ctx context.Context, guard *Lock, earliest *EarliestRefresh, dead *string) error {
	return s.update(ctx, guard, func(state *RefreshState) (bool, error) {
		state.Inflight, state.AmbiguousSince, state.Class, state.RetryAfter = nil, nil, nil, nil
		state.Resent = false
		state.EarliestRefresh, state.DeadDigest8 = earliest, dead
		return true, nil
	})
}

func (s *RefreshStateStore) restoreUnknown(ctx context.Context, guard *Lock, resent bool) error {
	return s.update(ctx, guard, func(state *RefreshState) (bool, error) {
		if state.Inflight == nil || state.Class == nil {
			return false, errs.NewConfig("Codex refresh state records no unknown refresh to restore")
		}
		state.Resent = resent
		return true, nil
	})
}

func (s *RefreshStateStore) recordDidNotHelp(ctx context.Context, guard *Lock) error {
	return s.update(ctx, guard, func(state *RefreshState) (bool, error) {
		if state.DidNotHelp < 255 {
			state.DidNotHelp++
		}
		switch state.DidNotHelp {
		case 0:
			state.FloorMin = DefaultRefreshFloorMin
		case 1:
			state.FloorMin = 2 * DefaultRefreshFloorMin
		default:
			state.FloorMin = 4 * DefaultRefreshFloorMin
		}
		return true, nil
	})
}

func (s *RefreshStateStore) resetFloor(ctx context.Context, guard *Lock) error {
	return s.update(ctx, guard, func(state *RefreshState) (bool, error) {
		state.DidNotHelp, state.FloorMin = 0, DefaultRefreshFloorMin
		return true, nil
	})
}

// ResetRefreshStateForLogin restores all policy facts after a verified login install.
// It requires the same still-live namespace lock held by the auth writer.
func ResetRefreshStateForLogin(ctx context.Context, paths *config.Paths, guard *Lock) error {
	if guard == nil {
		return errs.NewConfig("Codex login reset requires a live namespace lock")
	}
	user, account := guard.IDs()
	store, err := NewRefreshStateStore(paths, user, account)
	if err != nil {
		return err
	}
	return guard.withLive(paths, user, account, func() error { return store.storeDurable(ctx, NewRefreshState()) })
}

// RemoveRefreshState unlinks a namespace's marker without following its final component.
// It returns false for absence and requires the same live lock as account removal.
func RemoveRefreshState(ctx context.Context, paths *config.Paths, guard *Lock) (bool, error) {
	if guard == nil {
		return false, errs.NewConfig("Codex marker removal requires a live namespace lock")
	}
	user, account := guard.IDs()
	store, err := NewRefreshStateStore(paths, user, account)
	if err != nil {
		return false, err
	}
	var removed bool
	err = guard.withLive(paths, user, account, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := secret.OpenDirUnder(store.root, store.dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(dir) }()
		err = unix.Unlinkat(dir, store.name, 0)
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("could not remove Codex refresh marker: %w", err)
		}
		removed = true
		return nil
	})
	return removed, err
}
