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
	"os"
	"path/filepath"
	"time"
)

// A namespace this store owns can be pointed at by a Claude Code session
// at any time, and once it is, that session takes a refresh lock,
// rewrites the store, and eventually migrates the credentials into the
// keychain and deletes the file. Writing into a namespace while that is
// happening is how two processes end up rotating one refresh chain,
// which invalidates the other's token and costs the user a login. So
// detection looks for the traces such a session leaves, and everything
// here is read-only: nothing in this file removes a lock artefact.

// RefreshLockName is Claude Code's primary refresh lock, inside the
// store directory.
const RefreshLockName = ".oauth_refresh.lock"

// StorageWriteLockName is Claude Code's storage mutex directory: the
// caller supplies the base name and the locking library appends `.lock`.
const StorageWriteLockName = ".storage-write.lock"

// LegacyStorageWriteArtefact is an old artefact of this store's own
// earlier layout — never a peer mutex, and never a removal target.
const LegacyStorageWriteArtefact = ".storage-write"

// LiveKeychainService is the keychain service name Claude Code stores
// the live credentials under; a namespaced item appends `-<sha8>`.
const LiveKeychainService = "Claude Code-credentials"

// ForeignServiceName returns the keychain service name a namespace's
// credentials migrate into, by the eight hex digits of its suffix.
//
// One spelling for the two readers that must agree: the detection below
// decides which listed entries are this namespace's, and the unlisted
// status mode names the items to ask about — two formats of one shape
// would be a listing that silently matched nothing.
func ForeignServiceName(sha8 string) string {
	return LiveKeychainService + "-" + sha8
}

// ForeignActivityKind says what somebody else is doing with a namespace.
type ForeignActivityKind int

const (
	// ForeignNone means nothing: the caller may proceed.
	ForeignNone ForeignActivityKind = iota
	// ForeignClaudeLock means a Claude Code lock artefact is present.
	ForeignClaudeLock
	// ForeignMigratedToKeychain means a keychain item exists for this
	// namespace: a session has migrated the credentials out of the file
	// and this store must stop writing it.
	ForeignMigratedToKeychain
)

// ForeignActivity is what detection found in one namespace.
type ForeignActivity struct {
	// Kind says what was found.
	Kind ForeignActivityKind
	// LockName is the artefact's file name, for [ForeignClaudeLock].
	LockName string
	// LockAgeMS is how long ago the artefact was last touched, for
	// [ForeignClaudeLock]. Holders heartbeat every 5 s and self-lapse
	// after 60 s, so the age is what tells a live session from a crashed
	// one — though the caller refuses either way.
	LockAgeMS uint64
	// Service is the keychain service name that was found, for
	// [ForeignMigratedToKeychain].
	Service string
}

// OwnedMeta is the two spellings a namespace can be hashed under.
//
// A Claude Code session pointed at the namespace hashes the string it
// was given, so that is the primary spelling; but the user may have been
// given a path through a symbolic link, in which case the resolved
// spelling hashes differently and names a second possible item. Both
// are checked, because missing either one means writing into a migrated
// namespace.
type OwnedMeta struct {
	// ExportSHA8 is the hash of the namespace directory as it was
	// spelled at login.
	ExportSHA8 string
	// CanonicalSHA8 is the hash of the same directory after symbolic
	// link resolution, or empty when it does not differ.
	CanonicalSHA8 string
}

// DetectForeignActivity looks for signs that something other than this
// store owns a namespace.
//
// The keychain is only consulted when listing — the attribute listing
// from this pass — actually contains the service name. That keeps a
// namespace with no migration from issuing a keychain read at all, and
// it keeps the common case to zero extra subprocesses.
func DetectForeignActivity(ctx context.Context, nsDir string, owned *OwnedMeta, listing []ServiceEntry, reader Reader) ForeignActivity {
	artefacts := []string{
		filepath.Join(nsDir, RefreshLockName),
		filepath.Join(nsDir, StorageWriteLockName),
	}
	// The legacy lock sits beside the directory, named after its
	// resolved path with `.lock` appended.
	if canonical, err := filepath.EvalSymlinks(nsDir); err == nil {
		artefacts = append(artefacts, canonical+LegacyLockSuffix)
	}

	for _, artefact := range artefacts {
		ageMS, present := artefactAgeMS(artefact)
		if !present {
			continue
		}
		return ForeignActivity{Kind: ForeignClaudeLock, LockName: filepath.Base(artefact), LockAgeMS: ageMS}
	}

	for _, sha8 := range []string{owned.ExportSHA8, owned.CanonicalSHA8} {
		if sha8 == "" {
			continue
		}
		service := ForeignServiceName(sha8)
		if !listingContains(listing, service) {
			continue
		}
		switch _, err := reader.Read(ctx, service); {
		case err == nil:
			// The item is there and readable: the namespace has
			// migrated.
			return ForeignActivity{Kind: ForeignMigratedToKeychain, Service: service}
		case errors.Is(err, ErrItemNotFound):
			// Listed but gone by the time it was read. Nothing owns it
			// now.
		default:
			// Listed but unreadable — locked, timed out, refused. The
			// listing is evidence enough: an item under this
			// namespace's name exists, so writing stops. Failing closed
			// here costs a refresh; failing open costs the user's
			// session.
			return ForeignActivity{Kind: ForeignMigratedToKeychain, Service: service}
		}
	}

	return ForeignActivity{Kind: ForeignNone}
}

// listingContains reports whether the attribute listing carries the
// service name.
func listingContains(listing []ServiceEntry, service string) bool {
	for _, entry := range listing {
		if entry.Service == service {
			return true
		}
	}
	return false
}

// artefactAgeMS returns how long ago a lock artefact was modified, or
// false when it is not there. The examination never follows a symbolic
// link at the artefact's name, and a clock that has gone backwards
// saturates to zero rather than reading as an enormous age.
func artefactAgeMS(path string) (uint64, bool) {
	meta, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	age := max(time.Since(meta.ModTime()), 0)
	return uint64(age.Milliseconds()), true
}
