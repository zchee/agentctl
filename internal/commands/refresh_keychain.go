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
	"log/slog"
	"strings"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// migratedRefreshTarget binds write authority to one owned registry record.
type migratedRefreshTarget struct {
	nsDir, service, sha8, account string
	owner                         config.AccountRecord
}

func migratedRefreshItem(paths *config.Paths, record *config.AccountRecord, service string, listing []secret.ServiceEntry) (*migratedRefreshTarget, string) {
	const mismatch = "item name does not match the recorded export spelling; refusing to refresh"
	kind, ok := claude.Classify(service)
	if record.Kind.Owned == nil || !ok || kind.Live || kind.Suffix != record.Kind.Owned.ExportSHA8 || strings.ToLower(kind.Suffix) != kind.Suffix {
		return nil, mismatch
	}
	nsDir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
	if !paths.IsUnderNamespaceRoot(nsDir) {
		return nil, mismatch
	}
	account := secret.CurrentAccount()
	count := 0
	for _, entry := range listing {
		if entry.Service != service {
			continue
		}
		count++
		if entry.Account != "" && entry.Account != account {
			return nil, "the keychain item is registered to another account; refusing to refresh"
		}
	}
	if count > 1 {
		return nil, "more than one keychain item carries this service name; refusing to refresh"
	}
	return &migratedRefreshTarget{nsDir: nsDir, service: service, sha8: kind.Suffix, account: account, owner: *record}, ""
}

// refreshMigrated never holds peer locks during network I/O. It compares the
// discovery credential twice, then admits the write under the peer's lock set.
func (s *Status) refreshMigrated(ctx context.Context, paths *config.Paths, item *migratedRefreshTarget, current *claude.Credentials) refreshOutcome {
	if !refreshIdentityMatches(current, &item.owner) {
		return refreshOccupied(item, current)
	}
	if s.Env != nil && s.Env.SecureStorageDir != nil && *s.Env.SecureStorageDir != "" {
		return refreshRefused(claude.StateOfError("refresh refused: a secure-storage override is set"), "none")
	}
	if s.Refresher == nil {
		return refreshFailure(&RefreshNotWiredError{Transport: "token refresher"})
	}
	if s.Writer == nil {
		return refreshFailure(&RefreshNotWiredError{Transport: "keychain writer"})
	}
	before, err := current.Digests()
	if err != nil {
		return refreshFailure(err)
	}
	peer, err := s.readRefreshItem(ctx, item.service)
	if err != nil {
		return refreshOutcome{state: new(claude.StateOfStale()), note: "the item could not be re-read before the refresh", lockState: "none"}
	}
	if !refreshIdentityMatches(peer, &item.owner) {
		return refreshOccupied(item, peer)
	}
	observed, err := peer.Digests()
	if err != nil {
		return refreshFailure(err)
	}
	if observed != before {
		return refreshAdopted(item, peer)
	}
	current, err = s.Refresher.RefreshAccess(ctx, current)
	if err != nil {
		if auth, ok := errors.AsType[*errs.AuthError](err); ok && auth.InvalidGrant {
			peer, readErr := s.readRefreshItem(ctx, item.service)
			if readErr != nil {
				return refreshOutcome{state: new(claude.StateOfStale()), note: "the refresh was rejected and the item could not be re-read", lockState: "none"}
			}
			if !refreshIdentityMatches(peer, &item.owner) {
				return refreshOccupied(item, peer)
			}
			if observed, digestErr := peer.Digests(); digestErr == nil && observed != before {
				return refreshAdopted(item, peer)
			}
		}
		return refreshFailure(err)
	}
	unplannedType, unplannedTier := current.SubscriptionType, current.RateLimitTier
	s.askRefreshPlan(ctx, &item.owner, current, secret.HoldBudget)
	after, err := current.Digests()
	if err != nil {
		return refreshFailure(err)
	}
	to, ok := secret.Digest8(after.AccessSHA256)
	if !ok {
		return refreshRefused(claude.StateOfError("the refreshed credential has no usable digest"), "none")
	}
	from, _ := secret.Digest8(before.AccessSHA256)
	audit := secret.WriteEvent{Target: secret.NamespaceTarget(item.sha8), FromDigest8: &from, ToDigest8: to, Outcome: secret.WriteDiscarded, Direction: secret.DirectionForward}
	defer func() { appendRefreshAudit(ctx, paths, &audit) }()
	line, err := current.ToKeychainStdinLine(item.account, item.service)
	if _, tooLong := errors.AsType[*secret.LineTooLongError](err); tooLong && (current.SubscriptionType != unplannedType || current.RateLimitTier != unplannedTier) {
		current.SubscriptionType, current.RateLimitTier = unplannedType, unplannedTier
		line, err = current.ToKeychainStdinLine(item.account, item.service)
	}
	if err != nil {
		if _, tooLong := errors.AsType[*secret.LineTooLongError](err); tooLong {
			return refreshRefused(claude.StateOfError("blob exceeds the keychain stdin limit"), "none")
		}
		return refreshFailure(err)
	}
	dir, err := secret.OpenNamespaceDir(paths, item.nsDir)
	if err != nil {
		return refreshRefused(claude.StateOfError("refresh refused: "+err.Error()), "unavailable")
	}
	_ = unix.Close(dir)
	acquisition, err := secret.AcquirePeerLocks(ctx, secret.LockSubject{StoreDir: item.nsDir, Tree: secret.TreeOwn}, paths, nil, secret.RealSeams(secret.SystemClock()))
	var draft *secret.BreakDraft
	if acquisition != nil {
		draft = acquisition.BreakRecord
	}
	if failure, ok := errors.AsType[*secret.AcquireError](err); ok {
		draft = failure.BreakRecord
	}
	if draft != nil {
		appendRefreshAudit(ctx, paths, draft.Complete(item.service, secret.NamespaceTarget(item.sha8)))
	}
	if err != nil {
		return refreshOutcome{state: new(claude.StateOfLockUnavailable()), note: err.Error(), lockState: "unavailable"}
	}
	if acquisition.Busy() {
		return refreshOutcome{state: new(claude.StateOfBusy()), note: secret.BusyRefusal(acquisition.HolderAlive, acquisition.StoppedPIDs).Error(), lockState: "busy"}
	}
	hold := acquisition.Held
	defer hold.Release()
	if err := hold.DriftCheck(); err != nil {
		return refreshRefused(claude.StateOfError("refresh refused: "+err.Error()), "unavailable")
	}
	readCtx, cancel := context.WithTimeout(ctx, secret.KeychainVerifyTimeout)
	peer, err = s.readRefreshItem(readCtx, item.service)
	cancel()
	if err != nil {
		if errors.Is(err, secret.ErrKeychainLocked) {
			return refreshRefused(claude.StateOfKeychainLocked(""), "unavailable")
		}
		return refreshDiscarded("the item could not be re-read")
	}
	observed, err = peer.Digests()
	if err != nil || observed != before {
		return refreshDiscarded("the item changed under us")
	}
	if err := hold.DriftCheck(); err != nil {
		return refreshRefused(claude.StateOfError("refresh refused: "+err.Error()), "unavailable")
	}
	if err := hold.WriteAdmission(); err != nil || hold.HoldElapsed()+secret.KeychainWriteTimeout > secret.HoldBudget {
		return refreshDiscarded("the hold ran out of budget before the write")
	}
	writeErr := s.Writer.Write(ctx, item.service, line)
	hold.Release()
	if writeErr != nil && !errors.Is(writeErr, secret.ErrKeychainTimeout) {
		audit.Outcome = secret.WriteFailed
		state := claude.StateOfError("the refresh could not be stored: " + writeErr.Error())
		if errors.Is(writeErr, secret.ErrKeychainLocked) {
			state = claude.StateOfKeychainLocked("the write was refused")
		}
		return refreshOutcome{state: &state, note: writeErr.Error(), lockState: "none"}
	}
	peer, err = s.readRefreshItem(ctx, item.service)
	if err == nil {
		if observed, digestErr := peer.Digests(); digestErr == nil && observed == after {
			audit.Outcome = secret.WriteApplied
			return refreshOutcome{credentials: current, lockState: "migrated_refreshed"}
		}
	}
	audit.Outcome = secret.WriteUnknown
	return refreshOutcome{credentials: current, state: new(claude.StateOfStale()), note: "the refreshed item could not be confirmed; re-run `status`", lockState: "migrated_refreshed"}
}

func (s *Status) readRefreshItem(ctx context.Context, service string) (*claude.Credentials, error) {
	sealed, err := s.Reader.Read(ctx, service)
	if err != nil {
		return nil, err
	}
	var credentials *claude.Credentials
	err = sealed.WithPlaintext(func(blob []byte) error {
		var parseErr error
		credentials, parseErr = claude.ParseBlob(blob)
		return parseErr
	})
	return credentials, err
}

func refreshIdentityMatches(current *claude.Credentials, record *config.AccountRecord) bool {
	identity := current.Identity()
	return identity != nil && identity.AccountUUID == record.AccountUUID && identity.OrganizationUUID != nil && *identity.OrganizationUUID == record.OrganizationUUID
}

func refreshOccupied(item *migratedRefreshTarget, occupant *claude.Credentials) refreshOutcome {
	name := "unknown identity"
	if identity := occupant.Identity(); identity != nil {
		name = identity.AccountUUID
		if identity.Email != nil {
			name = *identity.Email
		}
	}
	var adopted *claude.Credentials
	if read, err := secret.ReadAdopted(item.nsDir); err == nil && read.Present {
		adopted, _ = claude.ParseBlob(read.Bytes)
		memguard.WipeBytes(read.Bytes)
	}
	return refreshOutcome{credentials: adopted, state: new(claude.StateOfAdopted(name)), note: "its keychain item is held by another identity", lockState: "none"}
}

func refreshAdopted(item *migratedRefreshTarget, peer *claude.Credentials) refreshOutcome {
	if !refreshIdentityMatches(peer, &item.owner) {
		return refreshOccupied(item, peer)
	}
	return refreshOutcome{credentials: peer, note: "a Claude Code session refreshed this item", lockState: "adopted"}
}

func refreshDiscarded(reason string) refreshOutcome {
	return refreshOutcome{state: new(claude.StateOfStale()), note: "refresh discarded: " + reason, lockState: "none"}
}

func appendRefreshAudit(ctx context.Context, paths *config.Paths, event secret.AuditEvent) {
	// A cancelled pass still owes evidence for a grant it minted or a lock it broke.
	if _, err := secret.AuditAppend(context.WithoutCancel(ctx), paths, secret.NewAuditEntry(event)); err != nil {
		slog.ErrorContext(ctx, "an audit entry could not be appended", slog.Any("error", err))
	}
}
