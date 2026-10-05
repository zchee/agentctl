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
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/cleanup"
)

// StagedAdoption holds a flushed temporary beside an untouched adopted copy.
// Always defer Discard after staging: Go does not automatically clean up a
// value abandoned on an early return. CommitStaged also consumes the staging,
// on either success or failure. The value must not be copied.
type StagedAdoption struct {
	mu         sync.Mutex
	root       string
	nsDir      string
	tmpName    string
	dir        int
	active     bool
	owned      bool
	unregister func() bool
}

// Discard removes an uncommitted temporary and withdraws emergency cleanup.
// It is idempotent and safe to race with CommitStaged or emergency cleanup.
func (s *StagedAdoption) Discard() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked()
}

func (s *StagedAdoption) releaseLocked() {
	if !s.active {
		return
	}
	s.active = false
	if s.unregister != nil {
		s.unregister()
	}
	if s.owned {
		_ = unlinkAt(s.dir, s.tmpName)
	}
	_ = unix.Close(s.dir)
}

// WriteAdopted atomically parks a displaced credential under AdoptedFile.
// There is no pending fallback: an adopted copy must never be replayed into
// the credential name a session reads. Errors leave the old copy untouched,
// except for a snapshot failure after a successful rename.
func WriteAdopted(ctx context.Context, paths *config.Paths, nsDir string, blobJSON *Secret) (FileSnapshot, error) {
	staged, err := StageAdopted(ctx, paths, nsDir, blobJSON)
	if err != nil {
		return FileSnapshot{}, err
	}
	defer staged.Discard()
	return CommitStaged(paths, staged)
}

// StageAdopted writes and flushes a private 0600 temporary without replacing
// AdoptedFile. It refuses paths outside the namespace, symbolic links and
// nonregular targets. Cancellation discards the staged file. Plaintext is
// opened only for the synchronous write. The caller must defer Discard.
func StageAdopted(ctx context.Context, paths *config.Paths, nsDir string, blobJSON *Secret) (*StagedAdoption, error) {
	return stageAdopted(ctx, paths, nsDir, blobJSON, nil)
}

func stageAdopted(ctx context.Context, paths *config.Paths, nsDir string, blobJSON *Secret, registry *cleanup.Registry) (*StagedAdoption, error) {
	target := filepath.Join(nsDir, AdoptedFile)
	if paths == nil || !paths.IsUnderNamespaceRoot(target) {
		return nil, &OutsideRootError{Path: target}
	}
	dir, err := OpenNamespaceDir(paths, nsDir)
	if err != nil {
		return nil, err
	}
	s := &StagedAdoption{root: paths.NamespaceRoot(), nsDir: nsDir, tmpName: AdoptedFile + ".tmp." + hex8(), dir: dir, active: true}
	s.mu.Lock()
	defer s.mu.Unlock()
	success := false
	defer func() {
		if !success {
			s.releaseLocked()
		}
	}()

	switch kind, err := entryAt(dir, AdoptedFile, target); {
	case err != nil:
		return nil, err
	case kind == entrySymlink:
		return nil, &SymlinkRefusedError{Path: target}
	case kind == entryOther:
		return nil, &NotRegularError{Path: target}
	}

	register, unregister := cleanup.Register, cleanup.Unregister
	if registry != nil {
		register, unregister = registry.Register, registry.Unregister
	}
	// Register while holding the staging mutex, before creation. Cleanup can
	// never close the descriptor or unlink while this write is in progress.
	token := register(s.Discard)
	s.unregister = func() bool { return unregister(token) }
	if err := blobJSON.WithPlaintext(func(doc []byte) error {
		s.owned = true
		err := createNewFileAt(dir, s.tmpName, doc)
		if errors.Is(err, fs.ErrExist) {
			// An exclusive-create collision belongs to somebody else.
			s.owned = false
		}
		return err
	}); err != nil {
		return nil, errs.NewIO(fmt.Sprintf("could not write `%s`", filepath.Join(nsDir, s.tmpName)), err)
	}
	if ctx.Err() != nil {
		return nil, &WriteCancelledError{Path: target}
	}
	success = true
	return s, nil
}

// CommitStaged replaces AdoptedFile with a staged credential and consumes the
// staging even on failure. There is intentionally no cancellation check: the
// caller commits only once the keychain may hold the credential being parked.
// A fresh no-follow walk must still reach the staged directory. Rename and
// cleanup remain relative to the retained descriptor, never a resolved path.
func CommitStaged(paths *config.Paths, s *StagedAdoption) (FileSnapshot, error) {
	if s == nil {
		return FileSnapshot{}, errors.New("adopted credential is not staged")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return FileSnapshot{}, errors.New("adopted credential staging was already resolved")
	}
	defer s.releaseLocked()
	target := filepath.Join(s.nsDir, AdoptedFile)
	if paths == nil || paths.NamespaceRoot() != s.root || !paths.IsUnderNamespaceRoot(target) {
		return FileSnapshot{}, &OutsideRootError{Path: target}
	}
	dir, err := OpenNamespaceDir(paths, s.nsDir)
	if err != nil {
		return FileSnapshot{}, err
	}
	defer func() { _ = unix.Close(dir) }()
	var retained, reopened unix.Stat_t
	if err := unix.Fstat(s.dir, &retained); err != nil {
		return FileSnapshot{}, errs.NewIO("could not stat the staged credential directory", err)
	}
	if err := unix.Fstat(dir, &reopened); err != nil {
		return FileSnapshot{}, errs.NewIO("could not stat the adopted credential directory", err)
	}
	if retained.Dev != reopened.Dev || retained.Ino != reopened.Ino {
		return FileSnapshot{}, errors.New("the adopted credential directory changed after staging")
	}
	if err := unix.Renameat(s.dir, s.tmpName, s.dir, AdoptedFile); err != nil {
		return FileSnapshot{}, errs.NewIO(fmt.Sprintf("could not adopt the displaced credential at `%s`", target), err)
	}
	// The temporary no longer exists. Never unlink that name after a commit,
	// even if reading back the snapshot fails or a peer creates it again.
	s.owned = false
	snap, err := snapshotAt(s.dir, AdoptedFile, target)
	if err != nil {
		return FileSnapshot{}, err
	}
	if snap == nil {
		return FileSnapshot{}, errs.NewIO(fmt.Sprintf("`%s` vanished immediately after being written", target), fs.ErrNotExist)
	}
	return *snap, nil
}
