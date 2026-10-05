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
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// LiveStoreEnv is what a live-tree hold needs to know about the process
// environment, supplied by the caller because this package sits below
// the provider that reads the environment.
type LiveStoreEnv struct {
	// SecureStorageDir is the raw value of the environment variable
	// that points Claude Code at a namespaced store. A non-empty value
	// means this environment names a namespace rather than the live
	// store, so the live tree is refused; an empty value is not set and
	// refuses nothing.
	SecureStorageDir string
	// NamedStoreDir is the live store directory the environment names.
	NamedStoreDir string
	// SecureStorageEnvName is the variable's name, for the refusal
	// message alone.
	SecureStorageEnvName string
}

// LockAnchor is the two directory descriptors every lock of one hold is
// addressed through.
//
// Opened by a single no-follow component walk from the anchor the tree
// permits — the namespace root for the manager's own tree, the resolved
// live store's parent for the live tree — and kept for the life of the
// hold. Two consequences, and both are the point: a symbolic link
// anywhere below the anchor is refused before the first mkdir, so
// nothing is created in whatever it pointed at; and the tree stops
// being a claim and becomes a derived fact, which is what the
// containment rule needs it to be.
type LockAnchor struct {
	// store is the store directory itself.
	store int
	// parent is its parent: the legacy lock is `<store>.lock`, beside
	// the store rather than inside it.
	parent int
	// storeName is the store directory's own name inside parent — the
	// spelling the walk accepted, which is what the legacy lock's name
	// is built from.
	storeName string
	// storeDir is the store directory the walk actually reached, for
	// records and messages. For the manager's own tree that is the
	// caller's own spelling, already below the namespace root with no
	// link in it; for the live tree it is the resolved store, because
	// the live store is reached through a symbolic link on a normal
	// machine and the caller's spelling would then name neither the
	// directory the artefacts are in nor the entry the legacy lock is.
	storeDir string
	// tree is which tree the walk proved it is in.
	tree Tree
	// closed remembers that the descriptors were given back.
	closed bool
}

// OpenLockAnchor opens the two descriptors a hold of subject needs,
// refusing a store that is not in the tree it claims and a way to it
// that cannot be walked without following a symbolic link.
//
// For the live tree, two refusals come before the walk: an environment
// whose secure-storage variable holds a non-empty value names a
// namespace rather than the live store, and a live store that cannot be
// resolved at all is unreachable. Only the acceptance consults the
// caller's spelling; what is walked and locked is derived from the
// environment's own name for the store, so no swap between the two
// resolutions can point the hold at a directory the environment does
// not name.
func OpenLockAnchor(subject LockSubject, paths *config.Paths, live *LiveStoreEnv) (*LockAnchor, error) {
	storeDir := filepath.Clean(subject.StoreDir)
	wrongTree := func() error {
		return &WrongTreeError{StoreDir: subject.StoreDir, Tree: subject.Tree}
	}

	// The anchor is chosen by the tree, and the walk from it is what
	// makes the choice binding: a store that is not below the anchor
	// cannot be reached from it at all, and one that is below it is
	// reached one no-follow component at a time.
	var anchor, storePath string
	switch subject.Tree {
	case TreeLive:
		if live == nil {
			return nil, &UnreachableError{Path: subject.StoreDir, Message: "this environment names no live store"}
		}
		if live.SecureStorageDir != "" {
			name := live.SecureStorageEnvName
			if name == "" {
				name = "the secure-storage directory variable"
			}
			return nil, &UnreachableError{
				Path: subject.StoreDir,
				Message: fmt.Sprintf("`%s` is set to `%s` in this shell, so this environment names a namespace rather than the live store; run without it, or lock the namespace instead",
					name, live.SecureStorageDir),
			}
		}
		// The live store is the store this environment names — and on a
		// normal machine it names it through a symbolic link. So the
		// store is resolved once, here, and everything downstream is
		// derived from the resolved path: the walk starts at the
		// resolved parent with one component left to open, and the
		// legacy lock is named after the resolved last component, which
		// is the directory entry the peer creates. Keeping the caller's
		// lexical parent instead would put this hold's legacy lock at a
		// different entry than the one a live session holds, and the
		// race that lock exists to lose would be back.
		named := filepath.Clean(live.NamedStoreDir)
		resolved, resolveErr := filepath.EvalSymlinks(named)
		// Identity, not spelling — but the tree check first: a store
		// that is not the live one is the wrong tree whether or not a
		// live store exists at all, so the resolution failure is held
		// until the caller has been shown to be asking about the live
		// store. Accepted: the characters the environment names, and
		// any spelling that resolves to the same directory.
		isLive := storeDir == named
		if !isLive && resolveErr == nil {
			if given, err := filepath.EvalSymlinks(storeDir); err == nil && given == resolved {
				isLive = true
			}
		}
		if !isLive {
			return nil, wrongTree()
		}
		if resolveErr != nil {
			return nil, &UnreachableError{
				Path:    subject.StoreDir,
				Message: fmt.Sprintf("the live store `%s` could not be resolved: %v", named, resolveErr),
			}
		}
		parent := filepath.Dir(resolved)
		if parent == resolved {
			return nil, wrongTree()
		}
		anchor, storePath = parent, resolved
	case TreeOwn:
		if !paths.IsUnderNamespaceRoot(storeDir) {
			return nil, wrongTree()
		}
		// Nothing is resolved here, and that is the difference between
		// the two trees: this store owns every component below its own
		// root, so a symbolic link at one of them is an attack rather
		// than a configuration, and the walk refuses it.
		anchor, storePath = paths.NamespaceRoot(), storeDir
	default:
		return nil, wrongTree()
	}

	storeName := filepath.Base(storePath)
	if storeName == "." || storeName == string(filepath.Separator) {
		return nil, wrongTree()
	}
	store, err := lockOpenDirUnder(anchor, storePath)
	if err != nil {
		return nil, &UnreachableError{Path: storePath, Message: err.Error()}
	}

	// The parent by descriptor rather than by a second walk: `..`
	// inside an already-opened directory is never a symbolic link, so
	// this reaches the store's real parent without any willingness to
	// follow a link planted at the store itself.
	parent, err := unix.Openat(store, "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		closeLockFD(store)
		return nil, &UnreachableError{Path: storePath, Message: fmt.Sprintf("its parent directory could not be opened: %v", err)}
	}

	return &LockAnchor{store: store, parent: parent, storeName: storeName, storeDir: storePath, tree: subject.Tree}, nil
}

// StoreDir returns the store directory this hold is about.
func (a *LockAnchor) StoreDir() string {
	return a.storeDir
}

// Tree returns which tree the walk proved the store directory is in.
func (a *LockAnchor) Tree() Tree {
	return a.tree
}

// Close gives the two descriptors back. Safe to call twice.
func (a *LockAnchor) Close() {
	if a.closed {
		return
	}
	a.closed = true
	closeLockFD(a.store)
	closeLockFD(a.parent)
}

// slot is the slot one artefact of this hold is operated on through.
func (a *LockAnchor) slot(of *lockArtefact) LockSlot {
	dir := a.store
	if of.inParent {
		dir = a.parent
	}
	return LockSlot{Dir: dir, Name: of.name, Shown: of.path}
}

// InStore returns a slot for one name inside the store directory, for a
// caller that runs the break rule against a single artefact.
func (a *LockAnchor) InStore(name, shown string) LockSlot {
	return LockSlot{Dir: a.store, Name: name, Shown: shown}
}

// lockArtefact is one lock artefact's identity: where it lives, what it
// is called there, and what it is called to a human.
type lockArtefact struct {
	// inParent says the name lives beside the store directory rather
	// than inside it.
	inParent bool
	// name is the name inside that directory.
	name string
	// path is the spelling a record and a message use.
	path string
}

// lockPlan is one of the three locks a credential-store hold is made
// of.
type lockPlan struct {
	artefact lockArtefact
	profile  LockProfile
}

// peerLockPlan is the three directories, in the peer's own nesting:
// primary first, then the legacy lock beside the store directory, then
// the storage-write mutex innermost. The order is the peer's, not a
// preference: taking the storage-write mutex first would be a genuine
// lock-order deadlock against a session whose own nesting is refresh
// body, then persist, then that mutex.
//
// The legacy lock's name comes from the opened chain — the store
// directory's own component as the no-follow walk accepted it, placed
// in the parent that walk reached. The peer names it after the resolved
// directory, and this produces exactly that directory entry, because a
// walk that refused every symbolic link below the anchor has already
// resolved everything path resolution would have.
func peerLockPlan(anchor *LockAnchor) [3]lockPlan {
	legacyName := anchor.storeName + LegacyLockSuffix
	legacyPath := filepath.Join(filepath.Dir(anchor.storeDir), legacyName)
	return [3]lockPlan{
		{
			artefact: lockArtefact{name: RefreshLockName, path: filepath.Join(anchor.storeDir, RefreshLockName)},
			profile:  RefreshProfile,
		},
		{
			artefact: lockArtefact{inParent: true, name: legacyName, path: legacyPath},
			profile:  RefreshProfile,
		},
		{
			artefact: lockArtefact{name: StorageWriteLockName, path: filepath.Join(anchor.storeDir, StorageWriteLockName)},
			profile:  StorageWriteProfile,
		},
	}
}
