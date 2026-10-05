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
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/errs"
)

const (
	testAuth        = "auth.json"
	testAuthPending = "auth.json.pending"
	testAuthMeta    = "auth.pending.meta"
)

// secretFileStore is a store with one Codex-shaped and one Claude-shaped
// namespace, each opened as a directory descriptor.
type secretFileStore struct {
	configDir  string
	codexRoot  string
	codexNS    string
	codexFD    int
	claudeRoot string
	claudeNS   string
	claudeFD   int
}

// openDirFD opens one existing directory without following links and closes
// it when the test ends.
func openDirFD(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("could not open `%s`: %v", dir, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func newSecretFileStore(t *testing.T) *secretFileStore {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "agctl")
	store := &secretFileStore{
		configDir:  configDir,
		codexRoot:  filepath.Join(configDir, "codex"),
		claudeRoot: filepath.Join(configDir, "claude"),
	}
	store.codexNS = filepath.Join(store.codexRoot, "user-abc", "acct-123")
	store.claudeNS = filepath.Join(store.claudeRoot, "acct", "org")
	for _, dir := range []string{store.codexNS, store.claudeNS} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("could not create `%s`: %v", dir, err)
		}
	}
	store.codexFD = openDirFD(t, store.codexNS)
	store.claudeFD = openDirFD(t, store.claudeNS)
	return store
}

// codexFile is the file under test bound to the Codex root.
func (s *secretFileStore) codexFile() *SecretFile {
	return NewSecretFile(s.codexRoot, s.codexFD, testAuth, filepath.Join(s.codexNS, testAuth))
}

func testSpec(prior *Digests) *PendingSpec {
	return &PendingSpec{TargetName: testAuth, PendingName: testAuthPending, MetaName: testAuthMeta, Prior: prior}
}

// strayTmps lists the `<name>.tmp.*` files inside dir, sorted.
func strayTmps(t *testing.T, dir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("could not list `%s`: %v", dir, err)
	}
	var found []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), name+".tmp.") {
			found = append(found, entry.Name())
		}
	}
	return found
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("`%s` should be readable: %v", path, err)
	}
	return string(b)
}

func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// isErrorType reports whether err matches the error type T anywhere in its
// chain.
func isErrorType[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

func TestSecretFileRefusesTargetsOutsideItsRoot(t *testing.T) {
	store := newSecretFileStore(t)
	tests := map[string]struct {
		root  string
		dir   int
		name  string
		shown string
	}{
		"error: codex root with a claude target": {
			root:  store.codexRoot,
			dir:   store.claudeFD,
			name:  testAuth,
			shown: filepath.Join(store.claudeNS, testAuth),
		},
		"error: claude root with a codex target": {
			root:  store.claudeRoot,
			dir:   store.codexFD,
			name:  ".credentials.json",
			shown: filepath.Join(store.codexNS, ".credentials.json"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			file := NewSecretFile(tt.root, tt.dir, tt.name, tt.shown)
			if _, err := file.Write(t.Context(), []byte("{}"), nil, StopComplete); !isErrorType[*OutsideRootError](err) {
				t.Errorf("Write: want an OutsideRootError, got %v", err)
			}
			if _, err := file.Read(1024); !isErrorType[*OutsideRootError](err) {
				t.Errorf("Read: want an OutsideRootError, got %v", err)
			}
			if _, err := file.Remove(); !isErrorType[*OutsideRootError](err) {
				t.Errorf("Remove: want an OutsideRootError, got %v", err)
			}
		})
	}
	for _, ns := range []string{store.claudeNS, store.codexNS} {
		entries, err := os.ReadDir(ns)
		if err != nil {
			t.Fatalf("could not list `%s`: %v", ns, err)
		}
		if len(entries) != 0 {
			t.Errorf("nothing may reach `%s`, found %d entries", ns, len(entries))
		}
	}
}

func TestSecretFileRefusesNamesThatAreNotOnePlainComponent(t *testing.T) {
	store := newSecretFileStore(t)
	target := filepath.Join(store.codexNS, testAuth)
	tests := map[string]struct {
		name  string
		shown string
	}{
		"error: a separator in the name":                   {name: "sub/auth.json", shown: filepath.Join(store.codexNS, "sub", testAuth)},
		"error: a parent component as the name":            {name: "..", shown: target},
		"error: a trailing slash on the name":              {name: "auth.json/", shown: target},
		"error: a displayed path naming another file":      {name: testAuth, shown: filepath.Join(store.codexNS, "sub", "other.json")},
		"error: the root itself":                           {name: "codex", shown: store.codexRoot},
		"error: a displayed path that climbs out the root": {name: testAuth, shown: filepath.Join(store.codexNS, "..", "..", "..", testAuth)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			file := NewSecretFile(store.codexRoot, store.codexFD, tt.name, tt.shown)
			if _, err := file.Write(t.Context(), []byte("{}"), nil, StopComplete); !isErrorType[*OutsideRootError](err) {
				t.Errorf("want an OutsideRootError, got %v", err)
			}
		})
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("nothing may reach the namespace, lstat err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(store.codexNS, "sub")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no subdirectory may be created, lstat err = %v", err)
	}
}

func TestSecretFileRefusesAPendingSpecForAnotherTarget(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	wrong := testSpec(nil)
	wrong.TargetName = "other.json"

	if _, err := file.Write(t.Context(), []byte("{}"), wrong, StopComplete); !isErrorType[*OutsideRootError](err) {
		t.Fatalf("a spec naming another file must be refused, got %v", err)
	}
	if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
		t.Errorf("nothing may be staged, found %v", strays)
	}
	if _, err := os.Lstat(filepath.Join(store.codexNS, testAuth)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the target must not exist, lstat err = %v", err)
	}
}

func TestSecretFileWriteLandsAtomicallyAt0600AndReadsBack(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	target := filepath.Join(store.codexNS, testAuth)

	first, err := file.Write(t.Context(), []byte(`{"n":1}`), testSpec(nil), StopComplete)
	if err != nil || first.SavedToPending {
		t.Fatalf("the first write should land, got %+v, %v", first, err)
	}
	second, err := file.Write(t.Context(), []byte(`{"n":2}`), testSpec(nil), StopComplete)
	if err != nil || second.SavedToPending {
		t.Fatalf("the second write should land, got %+v, %v", second, err)
	}

	if first.Snap.Ino == second.Snap.Ino {
		t.Errorf("the replacement is a rename, not an in-place write: inode %d unchanged", first.Snap.Ino)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("the target should exist: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the credential file is owner-only: mode = %04o, want 0600", mode)
	}

	outcome, err := file.Read(1024)
	if err != nil || !outcome.Present {
		t.Fatalf("the file was just written: %+v, %v", outcome, err)
	}
	if diff := gocmp.Diff([]byte(`{"n":2}`), outcome.Bytes); diff != "" {
		t.Errorf("read bytes mismatch (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(second.Snap, outcome.Snap); diff != "" {
		t.Errorf("the read sees the file the write reported (-want +got):\n%s", diff)
	}

	sealed, snap, err := file.ReadSecret(1024)
	if err != nil || sealed == nil || snap == nil {
		t.Fatalf("ReadSecret should produce a secret: %v", err)
	}
	if err := sealed.WithPlaintext(func(b []byte) error {
		if string(b) != `{"n":2}` {
			t.Errorf("the sealed document mismatches: %q", b)
		}
		return nil
	}); err != nil {
		t.Fatalf("the secret should open: %v", err)
	}

	if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
		t.Errorf("no temporary may be left behind, found %v", strays)
	}
	for _, leftover := range []string{testAuthPending, testAuthMeta} {
		if _, err := os.Lstat(filepath.Join(store.codexNS, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("`%s` must not exist, lstat err = %v", leftover, err)
		}
	}
}

func TestSecretFileRefusesASymlinkOrADirectoryAtTheTarget(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	target := filepath.Join(store.codexNS, testAuth)
	elsewhere := filepath.Join(store.configDir, "elsewhere.json")
	if err := os.WriteFile(elsewhere, []byte("not this store's"), 0o600); err != nil {
		t.Fatalf("the decoy should be writable: %v", err)
	}
	if err := os.Symlink(elsewhere, target); err != nil {
		t.Fatalf("the link should be creatable: %v", err)
	}

	if _, err := file.Write(t.Context(), []byte("{}"), nil, StopComplete); !isErrorType[*SymlinkRefusedError](err) {
		t.Errorf("a symlinked target must be refused on write, got %v", err)
	}
	if _, err := file.Read(1024); !isErrorType[*SymlinkRefusedError](err) {
		t.Errorf("and never followed on read, got %v", err)
	}
	if got := readText(t, elsewhere); got != "not this store's" {
		t.Errorf("the link's target must be untouched, got %q", got)
	}

	if err := os.Remove(target); err != nil {
		t.Fatalf("the link should be removable: %v", err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("a directory should be creatable: %v", err)
	}
	if _, err := file.Write(t.Context(), []byte("{}"), nil, StopComplete); !isErrorType[*NotRegularError](err) {
		t.Errorf("a directory target must be refused, got %v", err)
	}
	if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
		t.Errorf("nothing may be staged, found %v", strays)
	}
}

func TestSecretFileRemoveReportsWhetherAFileWasThere(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	if _, err := file.Write(t.Context(), []byte("{}"), nil, StopComplete); err != nil {
		t.Fatalf("the write should land: %v", err)
	}

	removed, err := file.Remove()
	if err != nil || !removed {
		t.Fatalf("the file was there: %v, %v", removed, err)
	}
	removed, err = file.Remove()
	if err != nil || removed {
		t.Fatalf("an absent file is not an error and was not removed: %v, %v", removed, err)
	}
	outcome, err := file.Read(1024)
	if err != nil || outcome.Present {
		t.Fatalf("the file must now be absent: %+v, %v", outcome, err)
	}
}

func TestSecretFileStopPolicies(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	target := filepath.Join(store.codexNS, testAuth)
	if _, err := file.Write(t.Context(), []byte("old"), nil, StopComplete); err != nil {
		t.Fatalf("the first write should land: %v", err)
	}
	stopped := cancelledContext(t)

	if _, err := file.Write(stopped, []byte("new"), testSpec(nil), StopDiscardStaged); !isErrorType[*WriteCancelledError](err) {
		t.Fatalf("a stopped DiscardStaged write must not replace the file, got %v", err)
	}
	if got := readText(t, target); got != "old" {
		t.Errorf("the old file must still be in place, got %q", got)
	}
	if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
		t.Errorf("the staged file must be unlinked, found %v", strays)
	}

	// StopComplete ignores the cancellation: the staged bytes may be the
	// only copy of a rotated grant.
	outcome, err := file.Write(stopped, []byte("new"), testSpec(nil), StopComplete)
	if err != nil || outcome.SavedToPending {
		t.Fatalf("a Complete write lands whatever the context says: %+v, %v", outcome, err)
	}
	if got := readText(t, target); got != "new" {
		t.Errorf("the Complete write must land, got %q", got)
	}
}

func TestSecretFileFailedRenameWithASpecParksTheBytesUnderBothPolicies(t *testing.T) {
	tests := map[string]struct {
		stop StopPolicy
	}{
		"success: complete parks the bytes":       {stop: StopComplete},
		"success: discard-staged parks the bytes": {stop: StopDiscardStaged},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			file := store.codexFile()
			target := filepath.Join(store.codexNS, testAuth)
			if _, err := file.Write(t.Context(), []byte("old"), nil, StopComplete); err != nil {
				t.Fatalf("the first write should land: %v", err)
			}

			file.faults = &writeFaults{renameErr: errors.New("rename failure injected by the test seam")}
			prior := &Digests{AccessSHA256: strings.Repeat("a", 64), RefreshSHA256: strings.Repeat("b", 64)}
			outcome, err := file.Write(t.Context(), []byte("rotated"), testSpec(prior), tt.stop)
			if err != nil {
				t.Fatalf("a failed rename with a spec is not an error: %v", err)
			}
			if !outcome.SavedToPending {
				t.Fatalf("the bytes should be parked, got %+v", outcome)
			}
			if !strings.Contains(outcome.PendingError, "injected") {
				t.Errorf("the cause is carried: %q", outcome.PendingError)
			}
			if got := readText(t, target); got != "old" {
				t.Errorf("the old file survives, got %q", got)
			}
			if got := readText(t, filepath.Join(store.codexNS, testAuthPending)); got != "rotated" {
				t.Errorf("the pending file holds the new bytes, got %q", got)
			}

			var meta map[string]any
			if err := json.Unmarshal([]byte(readText(t, filepath.Join(store.codexNS, testAuthMeta))), &meta); err != nil {
				t.Fatalf("the meta is JSON: %v", err)
			}
			if got := meta["derived_from_access_sha256"]; got != strings.Repeat("a", 64) {
				t.Errorf("derived_from_access_sha256 = %v", got)
			}
			if got := meta["derived_from_refresh_sha256"]; got != strings.Repeat("b", 64) {
				t.Errorf("derived_from_refresh_sha256 = %v", got)
			}
			if _, present := meta["new_expires_at"]; present {
				t.Errorf("no expiry is recorded when the spec has none: %v", meta)
			}
			if _, present := meta["created_at"]; !present {
				t.Errorf("the meta records when it was created: %v", meta)
			}
			if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
				t.Errorf("the temporary became the pending file, found %v", strays)
			}
		})
	}
}

func TestSecretFileFailedRenameWithoutASpecIsAnErrorAndLeavesTheOldFile(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	target := filepath.Join(store.codexNS, testAuth)
	if _, err := file.Write(t.Context(), []byte("old"), nil, StopComplete); err != nil {
		t.Fatalf("the first write should land: %v", err)
	}

	file.faults = &writeFaults{renameErr: errors.New("rename failure injected by the test seam")}
	if _, err := file.Write(t.Context(), []byte("verified"), nil, StopComplete); !isErrorType[*errs.IOError](err) {
		t.Fatalf("a failed rename without a spec is an error, got %v", err)
	}
	if got := readText(t, target); got != "old" {
		t.Errorf("the previous grant survives, got %q", got)
	}
	for _, leftover := range []string{testAuthPending, testAuthMeta} {
		if _, err := os.Lstat(filepath.Join(store.codexNS, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("`%s` must not exist, lstat err = %v", leftover, err)
		}
	}
	if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
		t.Errorf("the caller still holds the bytes, found %v", strays)
	}

	file.faults = nil
	if _, err := file.Write(t.Context(), []byte("verified"), nil, StopComplete); err != nil {
		t.Fatalf("without the seam the write lands: %v", err)
	}
	if got := readText(t, target); got != "verified" {
		t.Errorf("the retry lands, got %q", got)
	}
}

func TestSecretFileFailedPendingSaveKeepsTheStagedGrantOnlyUnderComplete(t *testing.T) {
	tests := map[string]struct {
		stop StopPolicy
		kept bool
	}{
		"success: complete keeps the staged grant":         {stop: StopComplete, kept: true},
		"success: discard-staged removes the staged bytes": {stop: StopDiscardStaged, kept: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// A directory at the pending name makes the park's rename fail
			// after the meta is written.
			store := newSecretFileStore(t)
			if err := os.Mkdir(filepath.Join(store.codexNS, testAuthPending), 0o700); err != nil {
				t.Fatalf("the occupant directory should be creatable: %v", err)
			}
			if err := os.WriteFile(filepath.Join(store.codexNS, testAuthPending, "occupant"), []byte("x"), 0o600); err != nil {
				t.Fatalf("the occupant should be writable: %v", err)
			}
			file := store.codexFile()
			file.faults = &writeFaults{renameErr: errors.New("rename failure injected by the test seam")}

			_, err := file.Write(t.Context(), []byte("rotated"), testSpec(nil), tt.stop)
			if !isErrorType[*errs.IOError](err) {
				t.Fatalf("a pending save that cannot land is an error, got %v", err)
			}

			strays := strayTmps(t, store.codexNS, testAuth)
			if tt.kept {
				if len(strays) != 1 {
					t.Fatalf("the staged grant is left for doctor to report, found %v", strays)
				}
				stray := filepath.Join(store.codexNS, strays[0])
				if got := readText(t, stray); got != "rotated" {
					t.Errorf("the kept temporary holds the grant, got %q", got)
				}
				if want := "the staged credential was kept at `" + stray + "`"; !strings.Contains(err.Error(), want) {
					t.Errorf("the error names the kept temporary: %v", err)
				}
			} else {
				if len(strays) != 0 {
					t.Errorf("the discard rule removes the staged file: %v", strays)
				}
				if strings.Contains(err.Error(), "kept") {
					t.Errorf("the discard sentence is unchanged: %v", err)
				}
			}
			if _, err := os.Lstat(filepath.Join(store.codexNS, testAuthMeta)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a meta without a pending is removed, lstat err = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(store.codexNS, testAuth)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the target must not exist, lstat err = %v", err)
			}
		})
	}
}

func TestSecretFileFailedMetaCreateKeepsTheStagedGrantOnlyUnderComplete(t *testing.T) {
	tests := map[string]struct {
		stop StopPolicy
		kept bool
	}{
		"success: complete keeps and names the staged grant": {stop: StopComplete, kept: true},
		"success: discard-staged removes the staged bytes":   {stop: StopDiscardStaged, kept: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// A directory at the meta name makes the exclusive meta create
			// fail before the pending rename is attempted.
			store := newSecretFileStore(t)
			if err := os.Mkdir(filepath.Join(store.codexNS, testAuthMeta), 0o700); err != nil {
				t.Fatalf("the occupant directory should be creatable: %v", err)
			}
			file := store.codexFile()
			file.faults = &writeFaults{renameErr: errors.New("rename failure injected by the test seam")}

			_, err := file.Write(t.Context(), []byte("rotated"), testSpec(nil), tt.stop)
			if !isErrorType[*errs.IOError](err) {
				t.Fatalf("a meta that cannot be written is an error, got %v", err)
			}
			if !strings.Contains(err.Error(), testAuthMeta) {
				t.Errorf("the meta is named: %v", err)
			}

			strays := strayTmps(t, store.codexNS, testAuth)
			if tt.kept {
				if len(strays) != 1 {
					t.Fatalf("the staged grant is kept, found %v", strays)
				}
				stray := filepath.Join(store.codexNS, strays[0])
				if got := readText(t, stray); got != "rotated" {
					t.Errorf("the kept temporary holds the grant, got %q", got)
				}
				if want := "the staged credential was kept at `" + stray + "`"; !strings.Contains(err.Error(), want) {
					t.Errorf("and named: %v", err)
				}
			} else if len(strays) != 0 {
				t.Errorf("the discard rule removes it: %v", strays)
			}
			if _, err := os.Lstat(filepath.Join(store.codexNS, testAuthPending)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("nothing may be parked, lstat err = %v", err)
			}
		})
	}
}

func TestSecretFileRefusesAnEscapingOrAliasedPendingSpec(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	target := filepath.Join(store.codexNS, testAuth)
	if _, err := file.Write(t.Context(), []byte("old"), nil, StopComplete); err != nil {
		t.Fatalf("the first write should land: %v", err)
	}
	// A Claude credential one tree over: what a `../` pending name would reach.
	claudeFile := filepath.Join(store.claudeNS, ".credentials.json")
	if err := os.WriteFile(claudeFile, []byte("claude's"), 0o600); err != nil {
		t.Fatalf("the decoy should be writable: %v", err)
	}
	escape := "../../../../claude/acct/org/.credentials.json"

	tests := map[string]struct {
		target  string
		pending string
		meta    string
	}{
		"error: an escaping pending name":        {target: testAuth, pending: escape, meta: testAuthMeta},
		"error: an escaping meta name":           {target: testAuth, pending: testAuthPending, meta: escape},
		"error: a separator in the pending name": {target: testAuth, pending: "sub/auth.json.pending", meta: testAuthMeta},
		"error: a parent component as the meta":  {target: testAuth, pending: testAuthPending, meta: ".."},
		"error: an empty pending name":           {target: testAuth, pending: "", meta: testAuthMeta},
		"error: pending equal to the target":     {target: testAuth, pending: testAuth, meta: testAuthMeta},
		"error: meta equal to the target":        {target: testAuth, pending: testAuthPending, meta: testAuth},
		"error: meta equal to the pending name":  {target: testAuth, pending: testAuthPending, meta: testAuthPending},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			spec := &PendingSpec{TargetName: tt.target, PendingName: tt.pending, MetaName: tt.meta}
			if _, err := file.Write(t.Context(), []byte("rotated"), spec, StopComplete); !isErrorType[*OutsideRootError](err) {
				t.Errorf("want an OutsideRootError, got %v", err)
			}
			if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
				t.Errorf("nothing may be staged, found %v", strays)
			}
			if got := readText(t, target); got != "old" {
				t.Errorf("the live file is untouched, got %q", got)
			}
		})
	}
	if got := readText(t, claudeFile); got != "claude's" {
		t.Errorf("nothing crossed into claude/, got %q", got)
	}
}

func TestSecretFileCrashBetweenFsyncAndRenameLeavesTheOldFileIntact(t *testing.T) {
	// The crash is injected through the unexported seam: the hook panics at
	// the point a killed process would stop, so the write never reaches the
	// rename. What must survive is the old file; what may remain is the
	// staged temporary, which doctor reports.
	store := newSecretFileStore(t)
	file := store.codexFile()
	target := filepath.Join(store.codexNS, testAuth)
	if _, err := file.Write(t.Context(), []byte("old"), nil, StopComplete); err != nil {
		t.Fatalf("the first write should land: %v", err)
	}

	type crashed struct{}
	file.faults = &writeFaults{beforeRename: func() { panic(crashed{}) }}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatalf("the injected crash should unwind the write")
			}
		}()
		_, _ = file.Write(t.Context(), []byte("rotated"), testSpec(nil), StopComplete)
	}()

	if got := readText(t, target); got != "old" {
		t.Errorf("the old file is intact after the crash, got %q", got)
	}
	strays := strayTmps(t, store.codexNS, testAuth)
	if len(strays) != 1 {
		t.Fatalf("the crash leaves exactly the staged temporary, found %v", strays)
	}
	if got := readText(t, filepath.Join(store.codexNS, strays[0])); got != "rotated" {
		t.Errorf("the staged temporary holds the unrenamed bytes, got %q", got)
	}
	for _, leftover := range []string{testAuthPending, testAuthMeta} {
		if _, err := os.Lstat(filepath.Join(store.codexNS, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("`%s` must not exist after the crash, lstat err = %v", leftover, err)
		}
	}

	file.faults = nil
	if _, err := file.Write(t.Context(), []byte("recovered"), testSpec(nil), StopComplete); err != nil {
		t.Fatalf("the next run still writes: %v", err)
	}
	if got := readText(t, target); got != "recovered" {
		t.Errorf("the next write lands, got %q", got)
	}
}

func TestSecretFileParkParksWithoutTouchingTheTarget(t *testing.T) {
	store := newSecretFileStore(t)
	file := store.codexFile()
	target := filepath.Join(store.codexNS, testAuth)
	// A torn or foreign target is exactly why a caller parks: a directory
	// at the target name must not stop the park.
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("the occupant should be creatable: %v", err)
	}

	outcome, err := file.Park(t.Context(), []byte("rotated"), *testSpec(nil))
	if err != nil {
		t.Fatalf("the park should succeed: %v", err)
	}
	if !outcome.SavedToPending {
		t.Fatalf("the bytes should be parked, got %+v", outcome)
	}
	if want := "the writes before it failed; parked without a rename"; outcome.PendingError != want {
		t.Errorf("PendingError = %q, want %q", outcome.PendingError, want)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuthPending)); got != "rotated" {
		t.Errorf("the pending file holds the grant, got %q", got)
	}
	if _, err := os.Lstat(filepath.Join(store.codexNS, testAuthMeta)); err != nil {
		t.Errorf("the meta is beside it: %v", err)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		t.Errorf("the target is not examined or replaced: %v", err)
	}
	if strays := strayTmps(t, store.codexNS, testAuth); len(strays) != 0 {
		t.Errorf("the temporary became the pending file, found %v", strays)
	}
}

func TestHex8IsEightLowercaseHexDigits(t *testing.T) {
	for range 32 {
		value := hex8()
		if len(value) != 8 {
			t.Fatalf("`%s` should be eight characters", value)
		}
		for _, b := range []byte(value) {
			lower := (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
			if !lower {
				t.Fatalf("`%s` should be lowercase hex", value)
			}
		}
	}
}
