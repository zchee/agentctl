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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

// fileStore is a store in a temporary directory, plus one namespace inside
// it.
type fileStore struct {
	tempDir string
	paths   *config.Paths
	nsDir   string
}

func newFileStore(t *testing.T) *fileStore {
	t.Helper()
	tempDir := t.TempDir()
	paths := config.NewPaths(filepath.Join(tempDir, "agctl"))
	return &fileStore{tempDir: tempDir, paths: paths, nsDir: paths.NamespaceDir("acct", "org")}
}

// blobJSON is a minimal credential document with the given tokens.
func blobJSON(access, refresh string) string {
	middle := ""
	if refresh != "" {
		middle = fmt.Sprintf(`"refreshToken":"%s",`, refresh)
	}
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"%s",%s"expiresAt":9999999999999}}`, access, middle)
}

func (s *fileStore) write(t *testing.T, json string, prior *Digests) WriteOutcome {
	t.Helper()
	outcome, err := WriteCredentials(t.Context(), &WriteRequest{Paths: s.paths, NSDir: s.nsDir, BlobJSON: []byte(json), Prior: prior, NewExpiresAtMS: 9_999_999_999_999})
	if err != nil {
		t.Fatalf("the write should succeed: %v", err)
	}
	return outcome
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("`%s` should exist: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestFileStoreReadReportsTheFilesIdentityAlongsideItsBytes(t *testing.T) {
	store := newFileStore(t)
	json := blobJSON("access-1", "refresh-1")
	store.write(t, json, nil)

	outcome, err := ReadCredentials(store.nsDir)
	if err != nil || !outcome.Present {
		t.Fatalf("the file was just written: %+v, %v", outcome, err)
	}
	if string(outcome.Bytes) != json {
		t.Errorf("bytes mismatch: %q", outcome.Bytes)
	}

	snap, err := Snapshot(filepath.Join(store.nsDir, CredentialsFile))
	if err != nil || snap == nil {
		t.Fatalf("the file exists: %v", err)
	}
	if diff := gocmp.Diff(*snap, outcome.Snap); diff != "" {
		t.Errorf("the read and a bare lstat agree about identity (-lstat +read):\n%s", diff)
	}
	if outcome.Snap.Size != int64(len(json)) || outcome.Snap.Ino == 0 || outcome.Snap.Dev == 0 || outcome.Snap.MtimeNS <= 0 {
		t.Errorf("the snapshot carries the identity fields: %+v", outcome.Snap)
	}
}

func TestFileStoreReadClassifiesAbsenceAndFailure(t *testing.T) {
	tests := map[string]struct {
		plant       func(t *testing.T, store *fileStore)
		wantPresent bool
		wantErr     func(error) bool
	}{
		"success: an absent namespace is absent, not an error": {
			plant: func(*testing.T, *fileStore) {},
		},
		"success: a component that is a file reads as absent": {
			plant: func(t *testing.T, store *fileStore) {
				if err := os.MkdirAll(filepath.Dir(store.nsDir), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(store.nsDir, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
		"error: a directory named like the credential file is a failure": {
			// The regular-file check catches it, and a failure is the
			// safer answer: "absent" would lead to a write the store
			// would refuse anyway.
			plant: func(t *testing.T, store *fileStore) {
				if err := os.MkdirAll(filepath.Join(store.nsDir, CredentialsFile), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantErr: isErrorType[*NotRegularError],
		},
		"error: a symlinked credential file is a failure, not an absence": {
			plant: func(t *testing.T, store *fileStore) {
				if err := os.MkdirAll(store.nsDir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				target := filepath.Join(store.nsDir, "elsewhere.json")
				if err := os.WriteFile(target, []byte(blobJSON("a", "")), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				if err := os.Symlink(target, filepath.Join(store.nsDir, CredentialsFile)); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantErr: isErrorType[*SymlinkRefusedError],
		},
		"error: an oversized credential file is a failure, not an absence": {
			plant: func(t *testing.T, store *fileStore) {
				if err := os.MkdirAll(store.nsDir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				big := make([]byte, MaxCredentialsBytes+1)
				if err := os.WriteFile(filepath.Join(store.nsDir, CredentialsFile), big, 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			wantErr: isErrorType[*TooLargeError],
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newFileStore(t)
			tt.plant(t, store)
			outcome, err := ReadCredentials(store.nsDir)
			if tt.wantErr != nil {
				if !tt.wantErr(err) {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the read should classify, not fail: %v", err)
			}
			if outcome.Present != tt.wantPresent {
				t.Errorf("Present = %v, want %v", outcome.Present, tt.wantPresent)
			}
		})
	}
}

func TestFileStoreSnapshotReportsAbsenceRefusesLinksAndChangesWithTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	snap, err := Snapshot(path)
	if err != nil || snap != nil {
		t.Fatalf("an absent path is not an error: %v, %v", snap, err)
	}

	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	first, err := Snapshot(path)
	if err != nil || first == nil {
		t.Fatalf("the file exists: %v", err)
	}
	if first.Size != 3 || first.Ino == 0 || first.Dev == 0 {
		t.Errorf("identity fields: %+v", first)
	}

	// A replacement changes the inode even when the size matches, which is
	// the case a size-and-mtime check would miss.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	second, err := Snapshot(path)
	if err != nil || second == nil {
		t.Fatalf("the file exists: %v", err)
	}
	if diff := gocmp.Diff(first, second); diff == "" {
		t.Errorf("the replacement must change the identity")
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := Snapshot(link); !isErrorType[*SymlinkRefusedError](err) {
		t.Errorf("a symlink must be refused, not followed: %v", err)
	}
	followed, err := SnapshotFollowing(link)
	if err != nil || followed == nil {
		t.Errorf("the following variant reads through the link: %v", err)
	}
}

func TestFileStoreWriteLandsAt0600In0700DirectoriesAndLeavesNoTemporary(t *testing.T) {
	store := newFileStore(t)
	json := blobJSON("access-1", "refresh-1")
	outcome := store.write(t, json, nil)
	if outcome.SavedToPending {
		t.Fatalf("the rename should succeed: %+v", outcome)
	}

	target := filepath.Join(store.nsDir, CredentialsFile)
	if got := readText(t, target); got != json {
		t.Errorf("contents mismatch: %q", got)
	}
	if mode := modeOf(t, target); mode != config.FileMode {
		t.Errorf("file mode = %04o, want %04o", mode, config.FileMode)
	}
	if outcome.Snap.Size != int64(len(json)) {
		t.Errorf("snapshot size = %d", outcome.Snap.Size)
	}
	for _, dir := range []string{store.paths.NamespaceRoot(), store.nsDir} {
		if mode := modeOf(t, dir); mode != config.DirMode {
			t.Errorf("`%s` mode = %04o, want %04o", dir, mode, config.DirMode)
		}
	}
	strays, err := ListStrayTmp(store.nsDir)
	if err != nil || len(strays) != 0 {
		t.Errorf("no temporary may remain: %v, %v", strays, err)
	}
}

func TestFileStoreASecondWriteReplacesTheFileWithANewInode(t *testing.T) {
	store := newFileStore(t)
	first := blobJSON("access-1", "refresh-1")
	before := store.write(t, first, nil)
	second := blobJSON("access-2", "refresh-2")
	after := store.write(t, second, &Digests{AccessSHA256: strings.Repeat("a", 64)})

	if before.Snap.Ino == after.Snap.Ino {
		t.Errorf("the replacement is atomic, not in-place: inode %d unchanged", before.Snap.Ino)
	}
	if got := readText(t, filepath.Join(store.nsDir, CredentialsFile)); got != second {
		t.Errorf("contents mismatch: %q", got)
	}
}

func TestFileStoreWriteRefusesATargetOutsideTheNamespaceRoot(t *testing.T) {
	store := newFileStore(t)
	tests := map[string]string{
		"error: the configuration directory": store.paths.ConfigDir(),
		"error: a lexical escape":            filepath.Join(store.paths.NamespaceRoot(), "..", "..", "elsewhere"),
		"error: an unrelated absolute path":  filepath.Join(store.tempDir, "never-written"),
	}
	for name, nsDir := range tests {
		t.Run(name, func(t *testing.T) {
			req := &WriteRequest{Paths: store.paths, NSDir: nsDir, BlobJSON: []byte(blobJSON("a", ""))}
			if _, err := WriteCredentials(t.Context(), req); !isErrorType[*OutsideRootError](err) {
				t.Errorf("`%s` must be refused, got %v", nsDir, err)
			}
			if _, err := os.Lstat(filepath.Join(nsDir, CredentialsFile)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("nothing may be written at `%s`", nsDir)
			}
		})
	}
}

func TestFileStoreWriteRefusesASymlinkedOrDirectoryTarget(t *testing.T) {
	store := newFileStore(t)
	if err := os.MkdirAll(store.nsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	elsewhere := filepath.Join(store.nsDir, "elsewhere.json")
	if err := os.WriteFile(elsewhere, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(store.nsDir, CredentialsFile)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	req := &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(blobJSON("a", ""))}
	if _, err := WriteCredentials(t.Context(), req); !isErrorType[*SymlinkRefusedError](err) {
		t.Fatalf("a symlinked target must be refused, got %v", err)
	}
	if got := readText(t, elsewhere); got != "{}" {
		t.Errorf("the link's target is untouched, got %q", got)
	}

	other := newFileStore(t)
	if err := os.MkdirAll(filepath.Join(other.nsDir, CredentialsFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	req = &WriteRequest{Paths: other.paths, NSDir: other.nsDir, BlobJSON: []byte(blobJSON("a", ""))}
	if _, err := WriteCredentials(t.Context(), req); !isErrorType[*NotRegularError](err) {
		t.Errorf("a directory target must be refused, got %v", err)
	}
}

func TestFileStoreListStrayTmpFindsOnlyTheEightHexShape(t *testing.T) {
	store := newFileStore(t)
	if err := os.MkdirAll(store.nsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	good := filepath.Join(store.nsDir, CredentialsFile+".tmp.0123abcd")
	bad := []string{
		filepath.Join(store.nsDir, CredentialsFile+".tmp.short"),
		filepath.Join(store.nsDir, CredentialsFile+".tmp.0123abcde"),
		filepath.Join(store.nsDir, CredentialsFile),
		filepath.Join(store.nsDir, "unrelated"),
	}
	for _, path := range append([]string{good}, bad...) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write `%s`: %v", path, err)
		}
	}

	found, err := ListStrayTmp(store.nsDir)
	if err != nil {
		t.Fatalf("the namespace should be listable: %v", err)
	}
	if diff := gocmp.Diff([]string{good}, found); diff != "" {
		t.Errorf("stray listing mismatch (-want +got):\n%s", diff)
	}
	missing, err := ListStrayTmp(filepath.Join(store.tempDir, "no-such-dir"))
	if err != nil || missing != nil {
		t.Errorf("a missing directory is an empty list: %v, %v", missing, err)
	}
}

func TestFileStoreRemoveNamespaceClearsTheFilesAndTheDirectories(t *testing.T) {
	store := newFileStore(t)
	json := blobJSON("a", "r")
	store.write(t, json, nil)
	for name, body := range map[string]string{
		PendingFile:                       json,
		PendingMetaFile:                   "{}",
		AdoptedFile:                       json,
		CredentialsFile + ".tmp.deadbeef": "x",
		AdoptedFile + ".tmp.0123abcd":     "x",
	} {
		if err := os.WriteFile(filepath.Join(store.nsDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write `%s`: %v", name, err)
		}
	}

	// A lock file for the namespace, which must survive.
	locksDir := store.paths.LocksDir()
	if err := os.MkdirAll(locksDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lock := store.paths.LockPath("acct", "org")
	if err := os.WriteFile(lock, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := RemoveNamespace(store.paths, store.nsDir); err != nil {
		t.Fatalf("removal should succeed: %v", err)
	}
	if _, err := os.Lstat(store.nsDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the namespace directory is gone, lstat err = %v", err)
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Errorf("the lock file is never unlinked: %v", err)
	}
	if _, err := os.Lstat(store.paths.NamespaceRoot()); err != nil {
		t.Errorf("the root survives: %v", err)
	}
}

func TestFileStoreRemoveNamespaceRefusesAPathOutsideTheRoot(t *testing.T) {
	store := newFileStore(t)
	if err := RemoveNamespace(store.paths, store.paths.ConfigDir()); !isErrorType[*OutsideRootError](err) {
		t.Errorf("the config directory is not a namespace, got %v", err)
	}
}

func TestFileStoreAFailedRenameParksTheCredentialsAndWritesTheMetadataFirst(t *testing.T) {
	store := newFileStore(t)
	first := blobJSON("old-access", "old-refresh")
	store.write(t, first, nil)

	prior := &Digests{AccessSHA256: strings.Repeat("c", 64), RefreshSHA256: strings.Repeat("d", 64)}
	second := blobJSON("new-access", "old-refresh")
	req := &WriteRequest{
		Paths:          store.paths,
		NSDir:          store.nsDir,
		BlobJSON:       []byte(second),
		Prior:          prior,
		NewExpiresAtMS: 9_999_999_999_999,
		faults:         &writeFaults{renameErr: errors.New("rename failure injected by the test seam")},
	}
	outcome, err := WriteCredentials(t.Context(), req)
	if err != nil {
		t.Fatalf("the injected rename failure should park the credentials: %v", err)
	}
	if !outcome.SavedToPending || outcome.PendingError == "" {
		t.Fatalf("the row explains why the write failed: %+v", outcome)
	}

	if got := readText(t, filepath.Join(store.nsDir, CredentialsFile)); got != first {
		t.Errorf("the old credentials are still in place, got %q", got)
	}
	if got := readText(t, filepath.Join(store.nsDir, PendingFile)); got != second {
		t.Errorf("the pending file holds the new bytes, got %q", got)
	}
	strays, err := ListStrayTmp(store.nsDir)
	if err != nil || len(strays) != 0 {
		t.Errorf("the temporary became the pending file: %v, %v", strays, err)
	}
	meta := readText(t, filepath.Join(store.nsDir, PendingMetaFile))
	for _, want := range []string{
		`"derived_from_access_sha256":"` + strings.Repeat("c", 64) + `"`,
		`"derived_from_refresh_sha256":"` + strings.Repeat("d", 64) + `"`,
		`"new_expires_at":9999999999999`,
		`"created_at":"`,
	} {
		if !strings.Contains(meta, want) {
			t.Errorf("the meta must carry %s, got %s", want, meta)
		}
	}
}

func TestFileStoreAFailedFirstRenameRecordsNullDigests(t *testing.T) {
	store := newFileStore(t)
	req := &WriteRequest{
		Paths:          store.paths,
		NSDir:          store.nsDir,
		BlobJSON:       []byte(blobJSON("first-access", "first-refresh")),
		NewExpiresAtMS: 9_999_999_999_999,
		faults:         &writeFaults{renameErr: errors.New("rename failure injected by the test seam")},
	}
	outcome, err := WriteCredentials(t.Context(), req)
	if err != nil || !outcome.SavedToPending {
		t.Fatalf("the first write should park: %+v, %v", outcome, err)
	}
	meta := readText(t, filepath.Join(store.nsDir, PendingMetaFile))
	if !strings.HasPrefix(meta, `{"derived_from_access_sha256":null,"derived_from_refresh_sha256":null,"created_at":"`) {
		t.Errorf("a first write records nulls, as the format fixes: %s", meta)
	}
}

func TestFileStoreACancelledWriteUnlinksTheTemporaryAndLeavesTheOldFile(t *testing.T) {
	store := newFileStore(t)
	first := blobJSON("old-access", "old-refresh")
	store.write(t, first, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(blobJSON("new-access", "old-refresh")), NewExpiresAtMS: 1}
	if _, err := WriteCredentials(ctx, req); !isErrorType[*WriteCancelledError](err) {
		t.Fatalf("a cancelled write must not replace the credentials, got %v", err)
	}

	if got := readText(t, filepath.Join(store.nsDir, CredentialsFile)); got != first {
		t.Errorf("the old credentials are still in place, got %q", got)
	}
	strays, err := ListStrayTmp(store.nsDir)
	if err != nil || len(strays) != 0 {
		t.Errorf("the staged replacement was unlinked: %v, %v", strays, err)
	}
	if _, err := os.Lstat(filepath.Join(store.nsDir, PendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("and it was not parked as pending either")
	}
}

// plantDirectoryLink plants link as a symbolic link to a directory outside
// the store, with one file inside whose survival the caller asserts. This
// is the escape the no-follow walk exists to close: the path still spells
// something under the namespace root, so the lexical check passes, and it
// resolves to a directory another program owns.
func plantDirectoryLink(t *testing.T, tempDir, link string) string {
	t.Helper()
	elsewhere := filepath.Join(tempDir, "someone-elses-store")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, CredentialsFile), []byte("not this store's"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return elsewhere
}

func assertUntouched(t *testing.T, elsewhere string) {
	t.Helper()
	if got := readText(t, filepath.Join(elsewhere, CredentialsFile)); got != "not this store's" {
		t.Errorf("the link's target was written through: %q", got)
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != CredentialsFile {
			t.Errorf("the link's target gained a file: %s", entry.Name())
		}
	}
}

func TestFileStoreRefusesASymlinkedComponent(t *testing.T) {
	tests := map[string]struct {
		link func(store *fileStore) string
	}{
		"error: a symlinked account component": {
			link: func(store *fileStore) string { return filepath.Join(store.paths.NamespaceRoot(), "acct") },
		},
		"error: a symlinked organization component": {
			link: func(store *fileStore) string { return store.nsDir },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newFileStore(t)
			elsewhere := plantDirectoryLink(t, store.tempDir, tt.link(store))

			req := &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(blobJSON("a", ""))}
			if _, err := WriteCredentials(t.Context(), req); !isErrorType[*SymlinkRefusedError](err) {
				t.Errorf("a symlinked component must be refused on write, got %v", err)
			}
			assertUntouched(t, elsewhere)

			// Deleting through a link is the same escape as writing
			// through one, and a worse one to discover after the fact.
			if err := RemoveNamespace(store.paths, store.nsDir); !isErrorType[*SymlinkRefusedError](err) {
				t.Errorf("a symlinked component must be refused on removal, got %v", err)
			}
			assertUntouched(t, elsewhere)
		})
	}
}

func TestFileStoreRefusesAComponentThatIsAFile(t *testing.T) {
	store := newFileStore(t)
	if err := os.MkdirAll(store.paths.NamespaceRoot(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.paths.NamespaceRoot(), "acct"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	req := &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(blobJSON("a", ""))}
	if _, err := WriteCredentials(t.Context(), req); !isErrorType[*NotRegularError](err) {
		t.Errorf("a component that is a file must be refused, got %v", err)
	}
}

// plantLockDir creates one lock directory in the namespace, the way the
// vendor's session does: acquire is mkdir and release is rmdir, so an
// empty directory is the whole shape of a lapsed lock.
func plantLockDir(t *testing.T, store *fileStore, name string) string {
	t.Helper()
	if err := os.MkdirAll(store.nsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(store.nsDir, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir lock: %v", err)
	}
	return path
}

func TestFileStoreRemoveDirUnderRootRemovesAnEmptyLockDirectory(t *testing.T) {
	store := newFileStore(t)
	lock := plantLockDir(t, store, ".oauth_refresh.lock")

	if err := RemoveDirUnderRoot(store.paths, lock); err != nil {
		t.Fatalf("an empty lock directory is removable: %v", err)
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock directory is gone, lstat err = %v", err)
	}
	if _, err := os.Lstat(store.nsDir); err != nil {
		t.Errorf("and the namespace around it survives: %v", err)
	}
}

func TestFileStoreRemoveDirUnderRootRefusals(t *testing.T) {
	tests := map[string]struct {
		plant   func(t *testing.T, store *fileStore) string
		wantErr func(error) bool
		survive bool
	}{
		"error: a non-empty directory is reported, never recursed": {
			plant: func(t *testing.T, store *fileStore) string {
				lock := plantLockDir(t, store, ".storage-write.lock")
				if err := os.WriteFile(filepath.Join(lock, "holder.json"), []byte("{}"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				return lock
			},
			wantErr: isErrorType[*NotEmptyError],
			survive: true,
		},
		"error: a regular file is refused": {
			plant: func(t *testing.T, store *fileStore) string {
				if err := os.MkdirAll(store.nsDir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				file := filepath.Join(store.nsDir, ".oauth_refresh.lock")
				if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				return file
			},
			wantErr: isErrorType[*NotRegularError],
			survive: true,
		},
		"error: a symlink at the artefact is refused": {
			plant: func(t *testing.T, store *fileStore) string {
				target := plantLockDir(t, store, "target.lock")
				link := filepath.Join(store.nsDir, ".oauth_refresh.lock")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return link
			},
			wantErr: isErrorType[*NotRegularError],
			survive: true,
		},
		"error: a path outside the root is refused": {
			plant: func(t *testing.T, store *fileStore) string {
				outside := filepath.Join(store.tempDir, "elsewhere", ".oauth_refresh.lock")
				if err := os.MkdirAll(outside, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return outside
			},
			wantErr: isErrorType[*OutsideRootError],
			survive: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newFileStore(t)
			path := tt.plant(t, store)
			if err := RemoveDirUnderRoot(store.paths, path); !tt.wantErr(err) {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := os.Lstat(path); tt.survive && err != nil {
				t.Errorf("the refused artefact must survive: %v", err)
			}
		})
	}
}

func TestFileStoreRemoveDirUnderRootRefusesASymlinkedComponent(t *testing.T) {
	store := newFileStore(t)
	elsewhere := plantDirectoryLink(t, store.tempDir, store.nsDir)
	if err := os.Mkdir(filepath.Join(elsewhere, ".oauth_refresh.lock"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := RemoveDirUnderRoot(store.paths, filepath.Join(store.nsDir, ".oauth_refresh.lock"))
	if !isErrorType[*SymlinkRefusedError](err) {
		t.Fatalf("a symlinked component must be refused, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, ".oauth_refresh.lock")); err != nil {
		t.Errorf("the other store's lock directory is untouched: %v", err)
	}
}

func TestFileStoreRemoveDirUnderRemovesBelowTheAnchorItIsGiven(t *testing.T) {
	store := newFileStore(t)
	home := filepath.Join(store.tempDir, "home")
	live := filepath.Join(home, ".claude")
	lock := filepath.Join(live, ".oauth_refresh.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := RemoveDirUnder(home, lock); err != nil {
		t.Fatalf("a lock directory below the anchor is removable: %v", err)
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leaked lock directory is gone, lstat err = %v", err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Errorf("and the store around it survives: %v", err)
	}

	// The legacy lock sits beside the store directory, which is why the
	// anchor is the parent rather than the store itself.
	legacy := filepath.Join(home, ".claude.lock")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := RemoveDirUnder(home, legacy); err != nil {
		t.Fatalf("the legacy lock is below the same anchor: %v", err)
	}
}

func TestFileStoreRemoveDirUnderRefusesASymlinkedStoreDirectory(t *testing.T) {
	store := newFileStore(t)
	home := filepath.Join(store.tempDir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	elsewhere := filepath.Join(store.tempDir, "someone-elses-claude")
	if err := os.MkdirAll(filepath.Join(elsewhere, ".oauth_refresh.lock"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".claude")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := RemoveDirUnder(home, filepath.Join(home, ".claude", ".oauth_refresh.lock"))
	if !isErrorType[*SymlinkRefusedError](err) {
		t.Fatalf("a symlinked store directory must be refused, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, ".oauth_refresh.lock")); err != nil {
		t.Errorf("the target is untouched: %v", err)
	}
}

func TestFileStoreCreateDirUnderMakesAMissingDirectoryAt0700AndReopensAnExistingOne(t *testing.T) {
	store := newFileStore(t)
	anchor := store.paths.NamespaceRoot()
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := filepath.Join(anchor, "held-locks")

	fd, err := CreateDirUnder(anchor, dir)
	if err != nil {
		t.Fatalf("a missing directory is created: %v", err)
	}
	if mode := modeOf(t, dir); mode != 0o700 {
		t.Errorf("mode = %04o: 0700, set on the descriptor rather than left to the umask", mode)
	}
	// The descriptor addresses that directory, which is the whole point of
	// returning one: a file made through it lands there.
	if err := createNewFileAt(fd, "probe", []byte("x")); err != nil {
		t.Fatalf("the descriptor is writable: %v", err)
	}
	_ = unix.Close(fd)
	if _, err := os.Lstat(filepath.Join(dir, "probe")); err != nil {
		t.Errorf("the probe landed in the directory: %v", err)
	}

	again, err := CreateDirUnder(anchor, dir)
	if err != nil {
		t.Fatalf("an existing directory is reopened: %v", err)
	}
	if err := createNewFileAt(again, "probe-2", []byte("x")); err != nil {
		t.Fatalf("still the same directory: %v", err)
	}
	_ = unix.Close(again)
	if _, err := os.Lstat(filepath.Join(dir, "probe-2")); err != nil {
		t.Errorf("the second probe landed: %v", err)
	}
}

func TestFileStoreCreateDirUnderRefusals(t *testing.T) {
	store := newFileStore(t)
	anchor := store.paths.NamespaceRoot()
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	elsewhere := filepath.Join(store.tempDir, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A symlink at the directory it would create: a path-based is-dir
	// check follows links and says yes, which is the bug the walk closes.
	leafLink := filepath.Join(anchor, "held-locks")
	if err := os.Symlink(elsewhere, leafLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := CreateDirUnder(anchor, leafLink); !isErrorType[*SymlinkRefusedError](err) {
		t.Errorf("a link at the leaf is refused, got %v", err)
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil || len(entries) != 0 {
		t.Errorf("nothing was created through the link: %v, %v", entries, err)
	}
	if info, err := os.Lstat(leafLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link itself was neither followed nor replaced: %v", err)
	}

	// A link one level up is refused too: the walk checks every component.
	middle := filepath.Join(anchor, "middle")
	if err := os.Symlink(elsewhere, middle); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := CreateDirUnder(anchor, filepath.Join(middle, "held-locks")); !isErrorType[*SymlinkRefusedError](err) {
		t.Errorf("a link above the leaf is refused, got %v", err)
	}

	// A regular file where a directory belongs is refused as what it is.
	occupied := filepath.Join(anchor, "occupied")
	if err := os.WriteFile(occupied, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := CreateDirUnder(anchor, occupied); !isErrorType[*NotRegularError](err) {
		t.Errorf("a file is not a directory, got %v", err)
	}

	// The anchor is the caller's own root and is never created by the walk.
	absent := filepath.Join(store.tempDir, "no-such-root")
	if _, err := CreateDirUnder(absent, filepath.Join(absent, "held-locks")); !isErrorType[*errs.IOError](err) {
		t.Errorf("a missing anchor is a failure, not something to create, got %v", err)
	}
	if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("and nothing was made on the way to finding out")
	}
}

func TestFileStoreConcurrentWritersThroughTheLockfileNeverTearTheStore(t *testing.T) {
	store := newFileStore(t)
	locksDir := store.paths.LocksDir()
	documents := make([]string, 8)
	for i := range documents {
		documents[i] = blobJSON(fmt.Sprintf("access-%d", i), fmt.Sprintf("refresh-%d", i))
	}

	var group sync.WaitGroup
	errCh := make(chan error, len(documents))
	for i := range documents {
		group.Go(func() {
			guard, err := Acquire(t.Context(), locksDir, "acct.org.lock", time.Now().Add(30*time.Second))
			if err != nil {
				errCh <- fmt.Errorf("writer %d could not lock: %w", i, err)
				return
			}
			defer func() { _ = guard.Release() }()
			req := &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(documents[i]), NewExpiresAtMS: 1}
			if _, err := WriteCredentials(t.Context(), req); err != nil {
				errCh <- fmt.Errorf("writer %d failed: %w", i, err)
			}
		})
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	final := readText(t, filepath.Join(store.nsDir, CredentialsFile))
	if !slices.Contains(documents, final) {
		t.Errorf("the final file is one writer's whole document, got %q", final)
	}
	if mode := modeOf(t, filepath.Join(store.nsDir, CredentialsFile)); mode != config.FileMode {
		t.Errorf("mode = %04o", mode)
	}
	strays, err := ListStrayTmp(store.nsDir)
	if err != nil || len(strays) != 0 {
		t.Errorf("no writer left a temporary: %v, %v", strays, err)
	}
}
