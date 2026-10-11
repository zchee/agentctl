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
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// StatID returns the (device, inode) pair of path without following a
// final symbolic link, and fails when path is not a directory.
func StatID(path string) (FileID, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return FileID{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return FileID{}, fmt.Errorf("%s is not a directory", path)
	}
	return FileID{Dev: uint64(st.Dev), Ino: st.Ino}, nil
}

// Exists reports whether anything at all, a symbolic link included, is at
// path; an error other than absence is returned as is.
func Exists(path string) (bool, error) {
	var st unix.Stat_t
	err := unix.Lstat(path, &st)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return false, &fs.PathError{Op: "lstat", Path: path, Err: err}
}

// ErrDestinationExists reports a rename refused because something already
// sits at the destination; nothing was moved.
var ErrDestinationExists = errors.New("the destination already exists")

// ErrSourceChanged reports a source directory that does not carry the
// (device, inode) pair the caller recorded; nothing was moved.
var ErrSourceChanged = errors.New("the source is not the recorded directory")

// MoveError reports a rename that ran but did not land where it should:
// the destination does not carry the source's pair, or the source path
// still exists. The directories are wherever the fields say.
type MoveError struct {
	Src, Dst string
	Want     FileID
	Reason   string
}

// Error describes where the rename left things.
func (e *MoveError) Error() string {
	return fmt.Sprintf("moving %s to %s (%s): %s", e.Src, e.Dst, e.Want, e.Reason)
}

// Move renames the directory src to dst, both on one file system, and
// proves where it landed. When want is not nil, src must carry that pair
// before anything moves. The rename is exclusive, so a directory that
// appears at dst between any check and the rename is a refusal
// ([ErrDestinationExists]), never a nesting; there is no copy fallback, so
// a rename across file systems fails. Afterwards dst must carry the
// source's pair and src must be gone, else a [*MoveError] says what was
// found. Neither parent is followed through a symbolic link.
func Move(src, dst string, want *FileID) (FileID, error) {
	srcBase, dstBase := filepath.Base(src), filepath.Base(dst)
	if !config.IsSingleComponent(srcBase) || !config.IsSingleComponent(dstBase) {
		return FileID{}, fmt.Errorf("moving %s to %s: not plain directory names", src, dst)
	}
	srcDir, err := openDirNoFollow(filepath.Dir(src))
	if err != nil {
		return FileID{}, err
	}
	defer func() { _ = unix.Close(srcDir) }()
	dstDir, err := openDirNoFollow(filepath.Dir(dst))
	if err != nil {
		return FileID{}, err
	}
	defer func() { _ = unix.Close(dstDir) }()

	before, err := fstatatID(srcDir, srcBase)
	if err != nil {
		return FileID{}, fmt.Errorf("moving %s: %w", src, err)
	}
	if want != nil && before != *want {
		return FileID{}, fmt.Errorf("moving %s: carries %s, recorded %s: %w", src, before, want, ErrSourceChanged)
	}
	if err := renameExclusive(srcDir, srcBase, dstDir, dstBase); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			return FileID{}, fmt.Errorf("moving %s to %s: %w", src, dst, ErrDestinationExists)
		}
		return FileID{}, fmt.Errorf("moving %s to %s: %w", src, dst, err)
	}
	after, err := fstatatID(dstDir, dstBase)
	if err != nil {
		return FileID{}, &MoveError{Src: src, Dst: dst, Want: before, Reason: "the destination cannot be read back: " + err.Error()}
	}
	if after != before {
		return FileID{}, &MoveError{Src: src, Dst: dst, Want: before, Reason: "the destination carries " + after.String()}
	}
	var st unix.Stat_t
	if err := unix.Fstatat(srcDir, srcBase, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return FileID{}, &MoveError{Src: src, Dst: dst, Want: before, Reason: "the source path still exists"}
	}
	return before, nil
}

func openDirNoFollow(dir string) (int, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	return fd, nil
}

func fstatatID(dirFD int, name string) (FileID, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return FileID{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return FileID{}, errors.New("not a directory")
	}
	return FileID{Dev: uint64(st.Dev), Ino: st.Ino}, nil
}
