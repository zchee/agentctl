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

package desktop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// ErrSymlinkInProfile reports a walk that met a symbolic link where it
// expected a directory, or an entry whose type changed while it walked;
// the walk stops there rather than act on something it did not inspect.
var ErrSymlinkInProfile = errors.New("a symbolic link or a changed entry was found inside the profile")

// PartialRemoveError reports a deletion that stopped midway. Remaining
// counts the entries, the profile directory itself included, still on
// disk.
type PartialRemoveError struct {
	Name      string
	Remaining int
	Err       error
}

// Error states how much is left.
func (e *PartialRemoveError) Error() string {
	return fmt.Sprintf("removing profile %s stopped with %d entries left: %v", e.Name, e.Remaining, e.Err)
}

// Unwrap returns the cause.
func (e *PartialRemoveError) Unwrap() error { return e.Err }

// walkHook runs between reading a directory's entries and acting on them,
// so a test can change an entry exactly inside that window.
var walkHook func(dirFD int, name string)

// RemoveProfile deletes the stored profile name and everything in it,
// relative to an open descriptor of profilesDir, never following a
// symbolic link. The profile itself must be a real directory. Inside it,
// every directory is opened with no-follow before it is entered, so a link
// standing where a directory was listed stops the walk; a non-directory
// entry is unlinked, which never follows a link, but only after checking
// that its type is still the one the listing reported. A walk that stops
// returns a [*PartialRemoveError] with the count of what remains.
func RemoveProfile(profilesDir, name string) error {
	if _, err := ParseProfileName(name); err != nil {
		return err
	}
	if !config.IsSingleComponent(name) {
		return fmt.Errorf("%q is not a profile name", name)
	}
	parent, err := openDirNoFollow(profilesDir)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(parent) }()
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "lstat", Path: profilesDir + "/" + name, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("profile %s: %w", name, ErrSymlinkInProfile)
	}
	if err := removeTree(parent, name); err != nil {
		remaining := countTree(parent, name)
		return &PartialRemoveError{Name: name, Remaining: remaining, Err: err}
	}
	return nil
}

// removeTree empties and removes the directory name inside dirFD.
func removeTree(dirFD int, name string) error {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return fmt.Errorf("%s: %w", name, ErrSymlinkInProfile)
		}
		return fmt.Errorf("opening %s: %w", name, err)
	}
	dir := os.NewFile(uintptr(fd), name)
	entries, err := dir.ReadDir(-1)
	if err != nil {
		_ = dir.Close()
		return fmt.Errorf("listing %s: %w", name, err)
	}
	if walkHook != nil {
		walkHook(fd, name)
	}
	for _, entry := range entries {
		child := entry.Name()
		if !config.IsSingleComponent(child) {
			_ = dir.Close()
			return fmt.Errorf("%s: unexpected entry name %q", name, child)
		}
		if entry.IsDir() {
			if err := removeTree(fd, child); err != nil {
				_ = dir.Close()
				return fmt.Errorf("%s/%w", name, err)
			}
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, child, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = dir.Close()
			return fmt.Errorf("%s/%s: %w", name, child, err)
		}
		if fileModeOf(st.Mode) != entry.Type() {
			_ = dir.Close()
			return fmt.Errorf("%s/%s: %w", name, child, ErrSymlinkInProfile)
		}
		if err := unix.Unlinkat(fd, child, 0); err != nil {
			_ = dir.Close()
			return fmt.Errorf("removing %s/%s: %w", name, child, err)
		}
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}
	if err := unix.Unlinkat(dirFD, name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	return nil
}

// fileModeOf maps a stat mode onto the type bits a directory listing
// reports, so the two can be compared.
func fileModeOf[M uint16 | uint32](mode M) fs.FileMode {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return fs.ModeDir
	case unix.S_IFLNK:
		return fs.ModeSymlink
	case unix.S_IFIFO:
		return fs.ModeNamedPipe
	case unix.S_IFSOCK:
		return fs.ModeSocket
	case unix.S_IFCHR:
		return fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		return fs.ModeDevice
	default:
		return 0
	}
}

// countTree counts what is left of name inside dirFD, itself included,
// without following a link; an unreadable directory counts as one entry.
func countTree(dirFD int, name string) int {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return 1
	}
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 1
	}
	dir := os.NewFile(uintptr(fd), name)
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 1
	}
	total := 1
	for _, entry := range entries {
		total += countTree(fd, entry.Name())
	}
	return total
}
