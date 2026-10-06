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

package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
)

// TokenRefresher exchanges and merges one refresh grant without writing a store.
type TokenRefresher = claude.TokenRefresher

// KeychainWriter updates an already validated item using a bounded stdin line.
type KeychainWriter interface {
	Write(ctx context.Context, service string, line *secret.KeychainStdinLine) error
}

// RefreshNotWiredError refuses a refresh when its required transport is absent.
type RefreshNotWiredError struct {
	// Transport names the missing dependency, never credential material.
	Transport string
}

// Error explains why the grant was left unspent.
func (e *RefreshNotWiredError) Error() string { return e.Transport + " is not wired" }

// refreshOutcome is the result of one refresh or pending-resolution barrier.
type refreshOutcome struct {
	credentials *claude.Credentials
	state       *claude.AccountState
	note        string
	lockState   string
}

// refreshExpired serializes file refreshes with pending replay and peer detection.
// The credential is reread under the lock: the discovery copy can already be old.
func (s *Status) refreshExpired(ctx context.Context, paths *config.Paths, record *config.AccountRecord, listing []secret.ServiceEntry) refreshOutcome {
	return s.underNamespaceLock(ctx, paths, record, listing, true)
}

func (s *Status) underNamespaceLock(ctx context.Context, paths *config.Paths, record *config.AccountRecord, listing []secret.ServiceEntry, mayRefresh bool) refreshOutcome {
	nsDir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
	target := filepath.Join(nsDir, secret.CredentialsFile)
	if record.Kind.Owned == nil || !paths.IsUnderNamespaceRoot(target) {
		return refreshRefused(claude.StateOfError("refresh refused: the credential path is outside the store"), "unavailable")
	}
	if _, err := secret.Snapshot(target); err != nil {
		return refreshRefused(claude.StateOfError("refresh refused: "+err.Error()), "unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(secret.NamespaceLockWait)
	}
	guard, err := secret.Acquire(ctx, paths.LocksDir(), filepath.Base(paths.LockPath(record.AccountUUID, record.OrganizationUUID)), deadline)
	if err != nil {
		if errors.Is(err, secret.ErrLockBusy) || errors.Is(err, context.DeadlineExceeded) {
			if fresh := rereadCredential(nsDir); fresh != nil && !fresh.AccessExpired(s.now().UnixMilli(), claude.RefreshMarginMillis) {
				return refreshOutcome{credentials: fresh, note: "another process refreshed this account", lockState: "adopted"}
			}
			return refreshRefused(claude.StateOfBusy(), "busy")
		}
		return refreshOutcome{state: new(claude.StateOfLockUnavailable()), note: err.Error(), lockState: "unavailable"}
	}
	defer func() { _ = guard.Release() }()

	activity := s.detectRefreshActivity(ctx, nsDir, record, listing)
	dir, err := secret.OpenNamespaceDir(paths, nsDir)
	if err != nil {
		return refreshRefused(claude.StateOfError("refresh refused: "+err.Error()), "unavailable")
	}
	defer func() { _ = unix.Close(dir) }()
	spec := secret.PendingSpec{TargetName: secret.CredentialsFile, PendingName: secret.PendingFile, MetaName: secret.PendingMetaFile}
	decision, _, err := secret.ResolvePendingWith(dir, nsDir, &spec, activity.Kind != secret.ForeignNone, refreshPendingCredential{})
	if err != nil {
		return refreshRefused(claude.StateOfError("refresh refused: the pending write could not be resolved: "+err.Error()), "unavailable")
	}
	pendingState := refreshPendingState(decision)
	switch activity.Kind {
	case secret.ForeignClaudeLock:
		if pendingState == nil {
			pendingState = new(claude.StateOfClaudeSessionDetected(activity.LockName, activity.LockAgeMS))
		}
		return refreshOutcome{state: pendingState, lockState: "claude_detected"}
	case secret.ForeignMigratedToKeychain:
		if pendingState == nil {
			pendingState = new(claude.StateOfMigratedToKeychain(activity.Service))
		}
		return refreshOutcome{state: pendingState, lockState: "migrated"}
	}

	file := secret.NewSecretFile(paths.NamespaceRoot(), dir, secret.CredentialsFile, target)
	current, snapshot := readRefreshFile(file)
	if current == nil {
		return refreshOutcome{state: new(claude.StateOfNeedsLogin()), note: "the stored credential is gone", lockState: "none"}
	}
	if !current.AccessExpired(s.now().UnixMilli(), claude.RefreshMarginMillis) {
		return refreshOutcome{credentials: current, state: pendingState, lockState: "adopted"}
	}
	if !mayRefresh {
		return refreshOutcome{credentials: current, state: pendingState, lockState: "none"}
	}
	if s.Refresher == nil {
		return refreshFailure(&RefreshNotWiredError{Transport: "token refresher"})
	}
	prior, err := current.Digests()
	if err != nil {
		return refreshFailure(err)
	}
	current, err = s.Refresher.RefreshAccess(ctx, current)
	if err != nil {
		return refreshFailure(err)
	}
	s.askRefreshPlan(ctx, record, current, secret.ReadTimeout)
	fault.Active().PausePoint(fault.BeforeRefreshRecheck)
	observed, err := secret.Snapshot(target)
	if err != nil || snapshot == nil || observed == nil || *snapshot != *observed || s.detectRefreshActivity(ctx, nsDir, record, listing).Kind != secret.ForeignNone {
		return refreshRefused(claude.StateOfRefreshDiscarded(), "claude_detected")
	}
	blob, err := current.BlobJSON()
	if err != nil {
		return refreshFailure(err)
	}
	defer memguard.WipeBytes(blob)
	write, err := secret.WriteCredentials(ctx, &secret.WriteRequest{
		Paths: paths, NSDir: nsDir, BlobJSON: blob,
		Prior: &secret.Digests{AccessSHA256: prior.AccessSHA256, RefreshSHA256: prior.RefreshSHA256}, NewExpiresAtMS: current.ExpiresAtMillis,
	})
	if err != nil {
		if _, cancelled := errors.AsType[*secret.WriteCancelledError](err); cancelled {
			return refreshRefused(claude.StateOfRefreshDiscarded(), "unavailable")
		}
		return refreshOutcome{state: new(claude.StateOfError("the refresh could not be stored: " + err.Error())), note: err.Error(), lockState: "unavailable"}
	}
	if write.SavedToPending {
		pendingState = new(claude.StateOfError(fmt.Sprintf("refresh saved to pending; write failed (%v)", write.PendingError)))
	}
	return refreshOutcome{credentials: current, state: pendingState, lockState: "none"}
}

func (s *Status) detectRefreshActivity(ctx context.Context, nsDir string, record *config.AccountRecord, listing []secret.ServiceEntry) secret.ForeignActivity {
	owned := &secret.OwnedMeta{ExportSHA8: record.Kind.Owned.ExportSHA8}
	if canonical, err := claude.Canonical(nsDir); err == nil {
		sha := claude.SHA8(claude.ExportSpelling(canonical))
		if sha != owned.ExportSHA8 {
			owned.CanonicalSHA8 = sha
		}
	}
	return secret.DetectForeignActivity(ctx, nsDir, owned, listing, s.Reader)
}

func readRefreshFile(file *secret.SecretFile) (*claude.Credentials, *secret.FileSnapshot) {
	read, err := file.Read(secret.MaxCredentialsBytes)
	if err != nil || !read.Present {
		return nil, nil
	}
	defer memguard.WipeBytes(read.Bytes)
	credentials, err := claude.ParseBlob(read.Bytes)
	if err != nil {
		return nil, nil
	}
	return credentials, &read.Snap
}

func rereadCredential(nsDir string) *claude.Credentials {
	read, err := secret.ReadCredentials(nsDir)
	if err != nil || !read.Present {
		return nil
	}
	defer memguard.WipeBytes(read.Bytes)
	credentials, _ := claude.ParseBlob(read.Bytes)
	return credentials
}

func (s *Status) askRefreshPlan(ctx context.Context, record *config.AccountRecord, current *claude.Credentials, save time.Duration) {
	if s.Profiles == nil || !claude.NeedsPlan(current) {
		return
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= claude.ProfileTimeout+save {
		return
	}
	profile, err := s.Profiles.ProfileOf(ctx, current)
	if err != nil {
		slog.DebugContext(ctx, "the plan could not be read; it is asked again at the next refresh", slog.Any("error", err))
		return
	}
	if profile.AccountUUID == record.AccountUUID && profile.OrganizationUUID == record.OrganizationUUID {
		claude.FillPlan(current, profile)
	}
}

func refreshRefused(state claude.AccountState, lockState string) refreshOutcome {
	return refreshOutcome{state: &state, lockState: lockState}
}

func refreshFailure(err error) refreshOutcome {
	state := claude.StateOfError(err.Error())
	if auth, ok := errors.AsType[*errs.AuthError](err); ok && auth.InvalidGrant {
		state = claude.StateOfNeedsLogin()
	}
	if status, ok := errors.AsType[*errs.HTTPError](err); ok && status.Status == 429 {
		state = claude.StateOfRateLimited()
		if status.HasRetryAfter {
			state = claude.StateOfRateLimitedAfter(uint64(max(0, status.RetryAfter/time.Second)))
		}
	}
	return refreshOutcome{state: &state, note: err.Error(), lockState: "none"}
}

func refreshPendingState(decision secret.PendingDecision) *claude.AccountState {
	switch decision.Kind {
	case secret.PendingReplayed:
		return new(claude.StateOfPendingReplayed())
	case secret.PendingDiscarded:
		if decision.Reason == secret.PendingFileRemoved {
			return new(claude.StateOfNeedsLogin())
		}
		return new(claude.StateOfPendingDiscarded(decision.Reason.Label()))
	default:
		return nil
	}
}

type refreshPendingCredential struct{}

func (refreshPendingCredential) MetaRequiresExpiry() bool { return true }
func (refreshPendingCredential) UnusableIsAbsent() bool   { return true }
func (refreshPendingCredential) Validate(blob []byte) bool {
	_, err := claude.ParseBlob(blob)
	return err == nil
}

func (refreshPendingCredential) Digests(blob []byte) (secret.Digests, bool) {
	credentials, err := claude.ParseBlob(blob)
	if err != nil {
		return secret.Digests{}, false
	}
	digests, err := credentials.Digests()
	return secret.Digests{AccessSHA256: digests.AccessSHA256, RefreshSHA256: digests.RefreshSHA256}, err == nil
}
