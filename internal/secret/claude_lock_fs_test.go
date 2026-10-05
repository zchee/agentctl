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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// openTestDir opens a directory for slot-based operations and closes it
// with the test.
func openTestDir(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func TestRealFSMakesAndRemovesALockDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	at := LockSlot{Dir: openTestDir(t, dir), Name: ".oauth_refresh.lock", Shown: filepath.Join(dir, ".oauth_refresh.lock")}
	fs := RealFS{}

	if err := fs.Mkdir(at); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	info, err := os.Stat(at.Shown)
	if err != nil {
		t.Fatalf("Stat(%q) = %v", at.Shown, err)
	}
	if !info.IsDir() {
		t.Fatalf("the lock artefact must be a directory")
	}
	if mode := info.Mode().Perm(); mode != config.DirMode {
		t.Errorf("lock directory mode = %o, want %o", mode, config.DirMode)
	}

	if _, present := fs.Mtime(at); !present {
		t.Errorf("Mtime() must see the directory that was just made")
	}
	if err := fs.Mkdir(at); !errors.Is(err, ErrLockExists) {
		t.Errorf("a second Mkdir() = %v, want %v", err, ErrLockExists)
	}
	if err := fs.Rmdir(at); err != nil {
		t.Fatalf("Rmdir() = %v", err)
	}
	if err := fs.Rmdir(at); !errors.Is(err, ErrLockGone) {
		t.Errorf("a second Rmdir() = %v, want %v", err, ErrLockGone)
	}
	if _, present := fs.Mtime(at); present {
		t.Errorf("Mtime() must report a removed directory as gone")
	}
}

func TestRealFSMkdirReportsAMissingParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	parent := filepath.Join(dir, "store")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir(%q) = %v", parent, err)
	}
	fd := openTestDir(t, parent)
	if err := os.Remove(parent); err != nil {
		t.Fatalf("Remove(%q) = %v", parent, err)
	}

	at := LockSlot{Dir: fd, Name: ".oauth_refresh.lock", Shown: filepath.Join(parent, ".oauth_refresh.lock")}
	if err := (RealFS{}).Mkdir(at); !errors.Is(err, ErrLockGone) {
		t.Errorf("Mkdir() into a removed parent = %v, want %v", err, ErrLockGone)
	}
}

func TestRealFSNeverRemovesThroughASymbolicLink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir(%q) = %v", target, err)
	}
	linkName := ".oauth_refresh.lock"
	if err := os.Symlink(target, filepath.Join(dir, linkName)); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}
	at := LockSlot{Dir: openTestDir(t, dir), Name: linkName, Shown: filepath.Join(dir, linkName)}
	fs := RealFS{}

	// The directory-only removal refuses the link, so the target — which
	// could be the live store's lock — survives.
	if err := fs.Rmdir(at); err == nil || errors.Is(err, ErrLockGone) {
		t.Fatalf("Rmdir() through a link = %v, want a refusal", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the link's target must survive: %v", err)
	}

	// And the sampling reads the link's own time, not the target's: a
	// planted link must not let its planter feed the break rule the
	// target's heartbeat.
	old := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatalf("Chtimes(%q) = %v", target, err)
	}
	got, present := fs.Mtime(at)
	if !present {
		t.Fatalf("Mtime() must see the link entry")
	}
	if got.Equal(old) {
		t.Errorf("Mtime() = %v, the target's time; it must read the link itself", got)
	}
}

func TestRealFSMtimeKeepsNanosecondResolution(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	name := "lockdir"
	if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	want := time.Date(2026, time.March, 5, 6, 7, 8, 123456789, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, name), want, want); err != nil {
		t.Fatalf("Chtimes() = %v", err)
	}

	got, present := (RealFS{}).Mtime(LockSlot{Dir: openTestDir(t, dir), Name: name, Shown: name})
	if !present {
		t.Fatalf("Mtime() must see the directory")
	}
	// The comparison between two samples is exact, so the reading has to
	// carry every digit the filesystem stores.
	if !got.Equal(want) {
		t.Errorf("Mtime() = %v, want %v to the nanosecond", got.UTC(), want)
	}
}

func TestLockPathComponentsUnder(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		anchor  string
		target  string
		want    []string
		wantErr bool
	}{
		"success: the anchor itself has no components":   {anchor: "/a/b", target: "/a/b", want: nil},
		"success: one component":                         {anchor: "/a/b", target: "/a/b/c", want: []string{"c"}},
		"success: a deeper path walks every component":   {anchor: "/a", target: "/a/b/c/d", want: []string{"b", "c", "d"}},
		"error: a sibling is outside the anchor":         {anchor: "/a/b", target: "/a/bc", wantErr: true},
		"error: a parent is outside the anchor":          {anchor: "/a/b", target: "/a", wantErr: true},
		"error: an unrelated root is outside the anchor": {anchor: "/a/b", target: "/x/y", wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := lockPathComponentsUnder(tt.anchor, tt.target)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("lockPathComponentsUnder(%q, %q) = %v, want an error", tt.anchor, tt.target, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("lockPathComponentsUnder(%q, %q) = %v", tt.anchor, tt.target, err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("components mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLockOpenDirUnderRefusesASymlinkedComponent(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	anchor := filepath.Join(tmp, "root")
	elsewhere := filepath.Join(tmp, "elsewhere", "org")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	if err := os.Mkdir(anchor, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	// `acct` is a link: the redirection that turns a contained path into
	// an operation somewhere else entirely.
	if err := os.Symlink(filepath.Join(tmp, "elsewhere"), filepath.Join(anchor, "acct")); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}

	if fd, err := lockOpenDirUnder(anchor, filepath.Join(anchor, "acct", "org")); err == nil {
		closeLockFD(fd)
		t.Fatalf("lockOpenDirUnder() through a link must refuse")
	}
}

func TestLockOpenDirUnderWalksARealChain(t *testing.T) {
	t.Parallel()

	anchor := t.TempDir()
	store := filepath.Join(anchor, "acct", "org")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	fd, err := lockOpenDirUnder(anchor, store)
	if err != nil {
		t.Fatalf("lockOpenDirUnder() = %v", err)
	}
	defer closeLockFD(fd)

	// The descriptor is the store directory itself: a name created
	// through it lands there.
	if err := unix.Mkdirat(fd, "probe", 0o700); err != nil {
		t.Fatalf("Mkdirat(probe) = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "probe")); err != nil {
		t.Errorf("the descriptor does not address the store directory: %v", err)
	}
}

func TestLockCreateDirUnderCreatesMissingComponentsPrivately(t *testing.T) {
	t.Parallel()

	anchor := t.TempDir()
	target := filepath.Join(anchor, "held-locks")
	fd, err := lockCreateDirUnder(anchor, target)
	if err != nil {
		t.Fatalf("lockCreateDirUnder() = %v", err)
	}
	defer closeLockFD(fd)

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(%q) = %v", target, err)
	}
	if mode := info.Mode().Perm(); mode != config.DirMode {
		t.Errorf("created directory mode = %o, want %o", mode, config.DirMode)
	}

	// A second walk opens the same directory rather than failing on the
	// existing component.
	again, err := lockCreateDirUnder(anchor, target)
	if err != nil {
		t.Fatalf("a second lockCreateDirUnder() = %v", err)
	}
	closeLockFD(again)
}

func TestLockCreateDirUnderRefusesALinkAtTheFinalName(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	anchor := filepath.Join(tmp, "root")
	elsewhere := filepath.Join(tmp, "elsewhere")
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(anchor, "held-locks")); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}

	if fd, err := lockCreateDirUnder(anchor, filepath.Join(anchor, "held-locks")); err == nil {
		closeLockFD(fd)
		t.Fatalf("a link at the final name must be refused: records created through it would land in a directory of somebody else's choosing")
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v", elsewhere, err)
	}
	if len(entries) != 0 {
		t.Errorf("nothing may be created through the link, found %v", entries)
	}
}

func TestLockCreateNewFileAtIsExclusiveAndDurable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fd := openTestDir(t, dir)

	if err := lockCreateNewFileAt(fd, "r.json", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("lockCreateNewFileAt() = %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "r.json"))
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	if string(body) != `{"a":1}` {
		t.Errorf("body = %q, want the exact bytes", body)
	}
	info, err := os.Stat(filepath.Join(dir, "r.json"))
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	if mode := info.Mode().Perm(); mode != config.FileMode {
		t.Errorf("record mode = %o, want %o", mode, config.FileMode)
	}

	if err := lockCreateNewFileAt(fd, "r.json", []byte("x")); !errors.Is(err, ErrLockExists) {
		t.Errorf("a taken name = %v, want %v", err, ErrLockExists)
	}
}

func TestLockCreateNewFileAtRefusesALinkAtTheName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "r.json")); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}

	err := lockCreateNewFileAt(openTestDir(t, dir), "r.json", []byte("x"))
	if err == nil {
		t.Fatalf("a link at the record name must be refused")
	}
	body, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("ReadFile() = %v", readErr)
	}
	if string(body) != "untouched" {
		t.Errorf("the link's target was written through: %q", body)
	}
}

func TestLockReadFileAt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.json"), []byte(strings.Repeat("x", 5000)), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "small.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	fd := openTestDir(t, dir)

	tests := map[string]struct {
		name    string
		want    string
		wantErr bool
	}{
		"success: a small regular file is read whole": {name: "small.json", want: `{"ok":true}`},
		"error: a file past the cap is refused":       {name: "big.json", wantErr: true},
		"error: a symbolic link is refused":           {name: "link.json", wantErr: true},
		"error: a directory is refused":               {name: "dir.json", wantErr: true},
		"error: a missing file is refused":            {name: "absent.json", wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := lockReadFileAt(fd, tt.name, 4096)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("lockReadFileAt(%q) = %q, want an error", tt.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("lockReadFileAt(%q) = %v", tt.name, err)
			}
			if string(got) != tt.want {
				t.Errorf("lockReadFileAt(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
