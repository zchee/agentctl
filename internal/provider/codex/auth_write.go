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
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
)

// LoginRefusal reports the first acceptance rule a login child broke.
type LoginRefusal struct{ Kind, Reason string }

// Error reports the fixed refusal sentence without credential contents.
func (e *LoginRefusal) Error() string {
	switch e.Kind {
	case "child":
		return "the Codex login was refused: " + e.Reason
	case "absent":
		return "the Codex login wrote no credential file into its scratch home"
	case "mode":
		return "the Codex login is in `" + e.Reason + "` mode; only a ChatGPT login has a usage source"
	case "identity":
		return "the Codex login's id token names no ChatGPT user or account; choose one workspace and log in again"
	case "ids":
		return "the Codex login's ids cannot name a namespace: " + e.Reason
	default:
		return "the Codex login's credential could not be used: " + e.Reason
	}
}

// VerifyLogin checks the child report before reading and parsing its credential once.
func VerifyLogin(ctx context.Context, scratch string, report *PostExitReport) (*VerifiedLogin, error) {
	if !report.clean() {
		return nil, &LoginRefusal{Kind: "child", Reason: strings.Join(report.anomalies(), "; ")}
	}
	raw, absent, err := readRegularAuth(ctx, filepath.Join(scratch, authFile))
	if absent {
		return nil, &LoginRefusal{Kind: "absent"}
	}
	if err != nil {
		return nil, &LoginRefusal{Reason: err.Error()}
	}
	defer memguard.WipeBytes(raw)
	doc, err := ParseCredentials(raw)
	if err != nil {
		return nil, &LoginRefusal{Reason: err.Error()}
	}
	if doc.AuthMode() != AuthChatGPT {
		return nil, &LoginRefusal{Kind: "mode", Reason: doc.AuthMode().Label()}
	}
	if doc.Digests() == nil {
		return nil, &LoginRefusal{Reason: "the credential has no access token"}
	}
	identity := doc.Identity()
	if identity == nil {
		return nil, &LoginRefusal{Kind: "identity"}
	}
	for _, id := range []string{identity.UserID, identity.AccountID} {
		if err := config.ValidateCodexSegment(id); err != nil {
			return nil, &LoginRefusal{Kind: "ids", Reason: err.Error()}
		}
	}
	return &VerifiedLogin{state: &verifiedLoginState{doc: doc, user: identity.UserID, account: identity.AccountID}}, nil
}

type namespaceDir struct {
	root, shown, user, account string
	fd                         int
}

func openNamespaceDir(paths *config.Paths, user, account string, guard *Lock) (*namespaceDir, error) {
	expected, err := paths.CodexLockPath(user, account)
	if err != nil {
		return nil, err
	}
	if !guard.Valid() || guard.Path() != expected {
		return nil, errInvalidProof
	}
	shown, err := paths.CodexNamespaceDir(user, account)
	if err != nil {
		return nil, err
	}
	fd, err := secret.CreateDirUnder(paths.CodexRoot(), shown)
	if err != nil {
		return nil, err
	}
	return &namespaceDir{root: paths.CodexRoot(), shown: shown, user: user, account: account, fd: fd}, nil
}

func (n *namespaceDir) file(name string) *secret.SecretFile {
	return secret.NewSecretFile(n.root, n.fd, name, filepath.Join(n.shown, name))
}

func (n *namespaceDir) current() (ResolvedKind, *Credentials, *secret.FileSnapshot, error) {
	read, err := n.file(authFile).ReadStrict(secret.MaxCredentialsBytes)
	if err != nil {
		return "", nil, nil, err
	}
	if !read.Present {
		return ResolvedAbsent, nil, nil, nil
	}
	defer memguard.WipeBytes(read.Bytes)
	credentials, err := ParseCredentials(read.Bytes)
	if err != nil {
		if e, ok := errors.AsType[*CredentialsError](err); ok && e.Kind == "truncated" {
			return ResolvedTorn, nil, nil, nil
		}
		return "", nil, nil, fmt.Errorf("`%s`: %w", filepath.Join(n.shown, authFile), err)
	}
	return ResolvedCredentials, credentials, &read.Snap, nil
}

// NamespaceRead contains a document bound to the original lock or an absent/torn result.
type NamespaceRead struct {
	Kind        ResolvedKind
	Credentials *LockedCredentials
}

// OwnedNamespace binds an owned proof and live lock to one directory descriptor.
type OwnedNamespace struct{ *ownedNamespaceState }

type ownedNamespaceState struct {
	owned  OwnedRecord
	guard  *Lock
	ns     *namespaceDir
	mu     sync.Mutex
	closed bool
}

// OpenOwnedNamespace opens or creates only the namespace validated by its lock.
func OpenOwnedNamespace(paths *config.Paths, owned *OwnedRecord, guard *Lock) (*OwnedNamespace, error) {
	if owned == nil || !guard.Valid() {
		return nil, errInvalidProof
	}
	guard.state.mu.Lock()
	defer guard.state.mu.Unlock()
	ns, err := openNamespaceDir(paths, owned.user, owned.account, guard)
	if err != nil {
		return nil, err
	}
	return &OwnedNamespace{ownedNamespaceState: &ownedNamespaceState{owned: *owned, guard: guard, ns: ns}}, nil
}

// Close closes this namespace descriptor; it does not release the caller's lock.
func (n *OwnedNamespace) Close() error {
	if n == nil || n.ownedNamespaceState == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	return unix.Close(n.ns.fd)
}

// Owned returns an isolated copy of the namespace's owned proof.
func (n *OwnedNamespace) Owned() *OwnedRecord {
	if n == nil || n.ownedNamespaceState == nil {
		return nil
	}
	owned := n.owned
	return &owned
}

// Lock returns the original lock identity for refresh-state mutations.
func (n *OwnedNamespace) Lock() *Lock {
	if n == nil || n.ownedNamespaceState == nil {
		return nil
	}
	return n.guard
}

// IDs returns the derived namespace identity.
func (n *OwnedNamespace) IDs() (string, string) {
	if n == nil || n.ownedNamespaceState == nil {
		return "", ""
	}
	return n.owned.user, n.owned.account
}

func (n *OwnedNamespace) begin() (func(), error) {
	if n == nil || n.ownedNamespaceState == nil || !n.guard.Valid() {
		return nil, errInvalidProof
	}
	n.guard.state.mu.Lock()
	n.mu.Lock()
	done := func() { n.mu.Unlock(); n.guard.state.mu.Unlock() }
	if n.closed || !n.guard.Valid() {
		done()
		return nil, errInvalidProof
	}
	return done, nil
}

// Read is the only producer of credentials bound to an owned namespace lock.
func (n *OwnedNamespace) Read() (NamespaceRead, error) {
	done, err := n.begin()
	if err != nil {
		return NamespaceRead{}, err
	}
	defer done()
	kind, credentials, _, err := n.ns.current()
	if err != nil {
		return NamespaceRead{}, err
	}
	read := NamespaceRead{Kind: kind}
	if credentials != nil {
		read.Credentials = &LockedCredentials{state: &lockedCredentialState{inner: credentials, guard: n.guard, user: n.ns.user, account: n.ns.account, base: cloneDigests(credentials.Digests())}}
	}
	return read, nil
}

// PostSnapshot records file identity and grant prefixes without retaining token bytes.
type PostSnapshot struct {
	snap    secret.FileSnapshot
	digests *secret.Digests
}

// RefreshDigest8 returns the grant prefix seen immediately before a POST.
func (s *PostSnapshot) RefreshDigest8() (string, bool) {
	if s == nil {
		return "", false
	}
	value := digest8Of(s.digests)
	if value == nil {
		return "", false
	}
	return *value, true
}

// SizeAndMtimeNS returns the snapshot's file size and modification timestamp.
func (s *PostSnapshot) SizeAndMtimeNS() (int64, int64) {
	if s == nil {
		return 0, 0
	}
	return s.snap.Size, s.snap.MtimeNS
}

// SnapshotForPost reads the current file identity; absent and torn files return nil.
func (n *OwnedNamespace) SnapshotForPost() (*PostSnapshot, error) {
	done, err := n.begin()
	if err != nil {
		return nil, err
	}
	defer done()
	_, credentials, snap, err := n.ns.current()
	if err != nil || credentials == nil {
		return nil, err
	}
	return &PostSnapshot{snap: *snap, digests: credentials.Digests()}, nil
}

type codexPendingCredential struct{}

func (codexPendingCredential) MetaRequiresExpiry() bool { return false }
func (codexPendingCredential) UnusableIsAbsent() bool   { return false }
func (codexPendingCredential) Validate(raw []byte) bool {
	_, err := ParseCredentials(raw)
	return err == nil
}

func (codexPendingCredential) Digests(raw []byte) (secret.Digests, bool) {
	credentials, err := ParseCredentials(raw)
	if err != nil {
		return secret.Digests{}, false
	}
	digests := credentials.Digests()
	if digests == nil {
		return secret.Digests{}, false
	}
	return *digests, true
}

// ResolvePending replays or discards first, keeping every unusable pair for retry.
func (n *OwnedNamespace) ResolvePending(ctx context.Context) (secret.PendingDecision, *WriteReceipt, Daemon, error) {
	done, err := n.begin()
	if err != nil {
		return secret.PendingDecision{}, nil, Daemon{}, err
	}
	defer done()
	evidence := DaemonEvidence(ctx, n.ns.shown)
	spec := &secret.PendingSpec{TargetName: authFile, PendingName: pendingFile, MetaName: pendingMeta}
	decision, write, err := secret.ResolvePendingWith(n.ns.fd, n.ns.shown, spec, false, codexPendingCredential{})
	if err != nil || write == nil {
		return decision, nil, evidence, err
	}
	kind := WritePendingDiscarded
	if decision.Kind == secret.PendingReplayed {
		kind = WritePendingReplayed
	}
	return decision, newWriteReceipt(kind, n.ns.user, n.ns.account, write.Before, write.Pending), evidence, nil
}

func (n *OwnedNamespace) checkCredentials(credentials *LockedCredentials) error {
	if !credentials.Valid() || credentials.state.guard.state != n.guard.state || credentials.state.user != n.ns.user || credentials.state.account != n.ns.account {
		return errInvalidProof
	}
	return nil
}

func refreshPendingSpec(credentials *LockedCredentials) *secret.PendingSpec {
	spec := &secret.PendingSpec{TargetName: authFile, PendingName: pendingFile, MetaName: pendingMeta, Prior: credentials.BaseDigests()}
	if expiry := credentials.state.inner.AccessExpiresAt(); expiry != nil && *expiry >= math.MinInt64/1000 && *expiry <= math.MaxInt64/1000 {
		spec.ExpiresAtMS = new(*expiry * 1000)
	}
	return spec
}

// WriteAuth consumes a verified login and installs it with no pending fallback.
// The caller resets the refresh marker under the same lock after success.
func WriteAuth(ctx context.Context, paths *config.Paths, login *VerifiedLogin, guard *Lock) (*WriteReceipt, Identity, error) {
	if login == nil || login.state == nil || !guard.Valid() {
		return nil, Identity{}, errInvalidProof
	}
	guard.state.mu.Lock()
	defer guard.state.mu.Unlock()
	ns, err := openNamespaceDir(paths, login.state.user, login.state.account, guard)
	if err != nil {
		return nil, Identity{}, err
	}
	defer func() { _ = unix.Close(ns.fd) }()
	if login.state.consumed.Swap(true) {
		return nil, Identity{}, errInvalidProof
	}
	kind, before, _, currentErr := ns.current()
	overwrote := currentErr != nil || kind != ResolvedAbsent
	var beforeDigests *secret.Digests
	if before != nil {
		beforeDigests = before.Digests()
	}
	var buffer bytes.Buffer
	defer func() { memguard.WipeBytes(buffer.Bytes()) }()
	if err := login.state.doc.WriteJSONTo(&buffer); err != nil {
		return nil, Identity{}, err
	}
	outcome, err := ns.file(authFile).WriteWithFaults(ctx, buffer.Bytes(), nil, secret.StopComplete, secret.WriteFaultNames{BeforeRename: fault.CodexBeforeRename, RenameFail: fault.CodexInstallRenameFail})
	if err != nil {
		return nil, Identity{}, err
	}
	if outcome.SavedToPending {
		return nil, Identity{}, errors.New("the verified Codex login was parked instead of installed")
	}
	receipt := newWriteReceipt(WriteLoginInstall, ns.user, ns.account, beforeDigests, login.state.doc.Digests())
	receipt.state.overwrote = overwrote
	return receipt, login.Identity(), nil
}

// Write installs a merge only if the current grant still matches its original read.
func (n *OwnedNamespace) Write(ctx context.Context, credentials *LockedCredentials) (CodexWrite, error) {
	done, err := n.begin()
	if err != nil {
		return CodexWrite{}, err
	}
	defer done()
	if err := n.checkCredentials(credentials); err != nil {
		return CodexWrite{}, err
	}
	kind, before, _, err := n.ns.current()
	if err != nil {
		return CodexWrite{}, err
	}
	if kind == ResolvedTorn {
		return CodexWrite{Kind: CodexWriteTorn}, nil
	}
	var beforeDigests *secret.Digests
	if before != nil {
		beforeDigests = before.Digests()
	}
	after := credentials.state.inner.Digests()
	base := credentials.state.base
	baseRefresh := ""
	if base != nil {
		baseRefresh = base.RefreshSHA256
	}
	if beforeDigests != nil && beforeDigests.RefreshSHA256 != baseRefresh {
		return CodexWrite{Kind: CodexWriteChangedSinceRead, Receipt: newWriteReceipt(WriteDiscardedExternal, n.ns.user, n.ns.account, beforeDigests, after)}, nil
	}
	var buffer bytes.Buffer
	defer func() { memguard.WipeBytes(buffer.Bytes()) }()
	if err := credentials.state.inner.WriteJSONTo(&buffer); err != nil {
		return CodexWrite{}, err
	}
	outcome, err := n.ns.file(authFile).WriteWithFaults(ctx, buffer.Bytes(), refreshPendingSpec(credentials), secret.StopComplete, secret.WriteFaultNames{BeforeRename: fault.CodexBeforeRename, RenameFail: fault.CodexRenameFail})
	if err != nil {
		return CodexWrite{}, err
	}
	if !outcome.SavedToPending && fault.Active().Is(fault.CodexErrorAfterRename) {
		return CodexWrite{}, errors.New("the Codex credential could not be examined after the rename")
	}
	writeKind := WriteRefreshApplied
	if outcome.SavedToPending {
		writeKind = WriteRefreshSavedToPending
	}
	return CodexWrite{Kind: CodexWriteLanded, Outcome: outcome, Receipt: newWriteReceipt(writeKind, n.ns.user, n.ns.account, beforeDigests, after)}, nil
}

// Park preserves a rotated grant as pending without attempting the auth target.
func (n *OwnedNamespace) Park(ctx context.Context, credentials *LockedCredentials) (CodexWrite, error) {
	done, err := n.begin()
	if err != nil {
		return CodexWrite{}, err
	}
	defer done()
	if err := n.checkCredentials(credentials); err != nil {
		return CodexWrite{}, err
	}
	_, before, _, _ := n.ns.current()
	var beforeDigests *secret.Digests
	if before != nil {
		beforeDigests = before.Digests()
	}
	var buffer bytes.Buffer
	defer func() { memguard.WipeBytes(buffer.Bytes()) }()
	if err := credentials.state.inner.WriteJSONTo(&buffer); err != nil {
		return CodexWrite{}, err
	}
	outcome, err := n.ns.file(authFile).Park(context.WithoutCancel(ctx), buffer.Bytes(), *refreshPendingSpec(credentials))
	if err != nil {
		return CodexWrite{}, err
	}
	return CodexWrite{Kind: CodexWriteLanded, Outcome: outcome, Receipt: newWriteReceipt(WriteRefreshSavedToPending, n.ns.user, n.ns.account, beforeDigests, credentials.state.inner.Digests())}, nil
}

func isNamedAuthFile(name string) bool {
	if name == authFile || name == pendingFile || name == pendingMeta {
		return true
	}
	suffix, ok := strings.CutPrefix(name, authTempPrefix)
	if !ok || len(suffix) != 8 {
		return false
	}
	for _, b := range []byte(suffix) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
			return false
		}
	}
	return true
}

func (n *OwnedNamespace) entries() ([]os.DirEntry, error) {
	fd, err := unix.Openat(n.ns.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), n.ns.shown)
	defer func() { _ = file.Close() }()
	return file.ReadDir(-1)
}

// HasStrayTmp reports interrupted staged auth writes that block resending.
func (n *OwnedNamespace) HasStrayTmp() (bool, error) {
	done, err := n.begin()
	if err != nil {
		return false, err
	}
	defer done()
	entries, err := n.entries()
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), authTempPrefix) && isNamedAuthFile(entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}

// DaemonEvidence reads session evidence without touching the daemon's locks.
func (n *OwnedNamespace) DaemonEvidence(ctx context.Context) Daemon {
	if n == nil || n.ownedNamespaceState == nil {
		return Daemon{Kind: DaemonNone}
	}
	return DaemonEvidence(ctx, n.ns.shown)
}

// RemoveNamedFiles refuses all foreign entries before removing any credential file.
// The caller removes the external refresh marker under the same lock.
func (n *OwnedNamespace) RemoveNamedFiles() (*WriteReceipt, error) {
	done, err := n.begin()
	if err != nil {
		return nil, err
	}
	defer done()
	entries, err := n.entries()
	if err != nil {
		return nil, err
	}
	var named, foreign []string
	for _, entry := range entries {
		if isNamedAuthFile(entry.Name()) {
			named = append(named, entry.Name())
		} else {
			foreign = append(foreign, entry.Name())
		}
	}
	if len(foreign) > 0 {
		return nil, fmt.Errorf("`%s` holds entries agentctl did not create (%q); nothing was removed", n.ns.shown, foreign)
	}
	_, before, _, _ := n.ns.current()
	var beforeDigests *secret.Digests
	if before != nil {
		beforeDigests = before.Digests()
	}
	for _, name := range named {
		if _, err := n.ns.file(name).Remove(); err != nil {
			return nil, err
		}
	}
	if err := secret.RemoveDirUnder(n.ns.root, n.ns.shown); err != nil {
		return nil, err
	}
	_ = secret.RemoveDirUnder(n.ns.root, filepath.Dir(n.ns.shown))
	return newWriteReceipt(WriteDelete, n.ns.user, n.ns.account, beforeDigests, nil), nil
}
