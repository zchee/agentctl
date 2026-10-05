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
	"time"
)

func TestLockIsHeldFollowsTheHolder(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "locks", "ns.lock")

	if LockIsHeld(path) {
		t.Fatalf("a lock file that does not exist reports held")
	}

	holder := HoldLock(t, path)
	if !LockIsHeld(path) {
		t.Fatalf("the lock is held by this test and must report so")
	}

	if err := holder.Close(); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	// A flock belongs to the open file description, and a concurrent
	// fork+exec elsewhere in this test binary briefly inherits a duplicate
	// of the lock's descriptor until exec closes it (close-on-exec). The
	// release is therefore guaranteed only once that transient duplicate is
	// gone, so the free state is observed within a bound, not instantly.
	if !WaitUntil(5*time.Second, func() bool { return !LockIsHeld(path) }) {
		t.Fatalf("the lock was released and must report free")
	}
}

func TestHoldLockCreatesParentsAndPrivateMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "deep", "nested", "ns.lock")
	holder := HoldLock(t, path)
	defer func() { _ = holder.Close() }()

	if got := ModeOf(t, path); got != 0o600 {
		t.Fatalf("lock file mode = %04o, want 0600", got)
	}
}

func TestInodeOfSeesAReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	devBefore, inoBefore := InodeOf(t, path)

	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("two"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	devAfter, inoAfter := InodeOf(t, path)

	if devBefore == devAfter && inoBefore == inoAfter {
		t.Fatalf("the inode did not change across an atomic replacement")
	}
}

func TestModeOfReadsPermissionBits(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if got := ModeOf(t, path); got != 0o640 {
		t.Fatalf("ModeOf = %04o, want 0640", got)
	}
}
