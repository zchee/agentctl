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

package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// LockIsHeld reports whether some other holder owns the exclusive lock on
// path. It opens the file and tries a non-blocking flock, which is exactly
// what the binary does. False when the file does not exist yet, so a
// caller can poll this from the moment it starts a child.
func LockIsHeld(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return true
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return false
}

// HoldLock takes the exclusive lock on path and holds it until the
// returned file is closed; the test's cleanup closes it as a backstop.
//
// flock locks belong to the open file description, so a second open of the
// same path - even in the same process - is a genuine second holder, which
// is what makes "the second waits" assertions mean anything.
func HoldLock(tb testing.TB, path string) *os.File {
	tb.Helper()
	// 0700, the mode the store itself creates lock directories at, so a
	// test never passes against a laxer tree than production builds.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		tb.Fatalf("create the locks directory: %v", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		tb.Fatalf("create the lock file: %v", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		tb.Fatalf("take an unheld lock: %v", err)
	}
	tb.Cleanup(func() { _ = file.Close() })
	return file
}

// InodeOf returns a file's (device, inode) pair, for proving a replacement
// was atomic.
func InodeOf(tb testing.TB, path string) (dev, ino uint64) {
	tb.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		tb.Fatalf("stat %q: %v", path, err)
	}
	return uint64(stat.Dev), stat.Ino
}

// ModeOf returns a file's permission bits.
func ModeOf(tb testing.TB, path string) uint32 {
	tb.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		tb.Fatalf("stat %q: %v", path, err)
	}
	return uint32(stat.Mode) & 0o7777
}
