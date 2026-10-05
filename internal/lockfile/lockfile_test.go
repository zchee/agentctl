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

package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDocumentedTimings(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		got  any
		want any
	}{
		"success: the retry interval is 250ms": {got: RetryInterval, want: 250 * time.Millisecond},
		"success: eight create attempts":       {got: CreateAttempts, want: 8},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Errorf("constant = %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestAnUnsupportedFlockFailsClosed(t *testing.T) {
	// Not parallel: it swaps the package's flock function, and the swap
	// must not overlap any other test.
	original := flock
	t.Cleanup(func() { flock = original })
	flock = func(fd, how int) error {
		if how&unix.LOCK_UN != 0 {
			return original(fd, how)
		}
		return unix.ENOTSUP
	}

	locksDir := filepath.Join(t.TempDir(), ".locks")
	start := time.Now()
	_, err := LockIn(t.Context(), locksDir, "acct.org.lock", time.Now().Add(10*time.Second))
	if _, ok := errors.AsType[*LockUnavailableError](err); !ok {
		t.Fatalf("LockIn() = %v, want a lock-unavailable error: a filesystem without flock never authorises a write", err)
	}
	if !errors.Is(err, unix.ENOTSUP) {
		t.Errorf("the underlying errno should survive: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("only contention may retry; ENOTSUP waited %v", elapsed)
	}
}

func TestManyGoroutinesRacingToCreateOneFreshLockFileAllOpenIt(t *testing.T) {
	t.Parallel()

	// A regression test for a fault a single-shot test misses: the
	// kernel's openat does not retry its lookup when another thread wins
	// an O_CREAT race on this platform, so a combined create returns
	// ENOENT — not EEXIST — for a large fraction of the racers. The split
	// open-then-create keeps every racer successful.
	const workers = 8
	const rounds = 60

	for round := range rounds {
		dir := t.TempDir()
		path := filepath.Join(dir, ".config.lock")
		dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("round %d: Open(%q) = %v", round, dir, err)
		}

		var wg sync.WaitGroup
		failures := make(chan error, workers)
		for range workers {
			wg.Go(func() {
				file, err := openLockFile(dirFD, ".config.lock", path)
				if err != nil {
					failures <- err
					return
				}
				_ = file.Close()
			})
		}
		wg.Wait()
		close(failures)
		_ = unix.Close(dirFD)

		for err := range failures {
			t.Errorf("round %d: %v", round, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("round %d: the lock file should have been created: %v", round, err)
		}
	}
}

func TestLockCreatesTheFileAtTheDocumentedModes(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), ".locks")
	guard, err := LockIn(t.Context(), locksDir, "acct.org.lock", time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("LockIn() = %v", err)
	}
	defer func() { _ = guard.Release() }()

	info, err := os.Stat(guard.Path())
	if err != nil {
		t.Fatalf("Stat(%q) = %v", guard.Path(), err)
	}
	if mode := info.Mode().Perm(); mode != fileMode {
		t.Errorf("lock file mode = %o, want %o", mode, fileMode)
	}
	dirInfo, err := os.Stat(locksDir)
	if err != nil {
		t.Fatalf("Stat(%q) = %v", locksDir, err)
	}
	if mode := dirInfo.Mode().Perm(); mode != dirMode {
		t.Errorf("locks dir mode = %o, want %o", mode, dirMode)
	}
}
