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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// holderEnv asks a re-executed copy of this test binary to hold an
// exclusive flock instead of running the tests, so a contention test has
// a genuinely separate process on the other side of the lock.
const holderEnv = "GO_TEST_FLOCK_HOLDER_PATH"

func TestMain(m *testing.M) {
	if path := os.Getenv(holderEnv); path != "" {
		holdFlockUntilStdinCloses(path)
		return
	}
	os.Exit(m.Run())
}

// holdFlockUntilStdinCloses opens path, takes the exclusive flock, says
// so on stdout, and holds the lock until stdin reaches end of file.
func holdFlockUntilStdinCloses(path string) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "holder: open %s: %v\n", path, err)
		os.Exit(1)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		fmt.Fprintf(os.Stderr, "holder: flock %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Println("held")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "holder: stdin: %v\n", err)
		os.Exit(1)
	}
	_ = file.Close()
	os.Exit(0)
}

func soon() time.Time {
	return time.Now().Add(5 * time.Second)
}

func TestDocumentedTimings(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		got  any
		want any
	}{
		"success: the retry interval is 250ms": {got: NamespaceLockRetry, want: 250 * time.Millisecond},
		"success: the command wait is 5s":      {got: NamespaceLockWait, want: 5 * time.Second},
		"success: eight create attempts":       {got: LockCreateAttempts, want: 8},
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

func TestAcquireCreatesTheLockAtTheDocumentedModes(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), "store", "claude", ".locks")
	guard, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	defer func() { _ = guard.Release() }()

	if want := filepath.Join(locksDir, "acct.org.lock"); guard.Path() != want {
		t.Errorf("guard path = %q, want %q", guard.Path(), want)
	}
	info, err := os.Stat(guard.Path())
	if err != nil {
		t.Fatalf("Stat(%q) = %v", guard.Path(), err)
	}
	if mode := info.Mode().Perm(); mode != config.FileMode {
		t.Errorf("lock file mode = %o, want %o", mode, config.FileMode)
	}
	dirInfo, err := os.Stat(locksDir)
	if err != nil {
		t.Fatalf("Stat(%q) = %v", locksDir, err)
	}
	if mode := dirInfo.Mode().Perm(); mode != config.DirMode {
		t.Errorf("locks dir mode = %o, want %o", mode, config.DirMode)
	}
}

func TestLockFileSurvivesTheGuard(t *testing.T) {
	t.Parallel()

	// Never unlinked: flock locks an inode, and a recreated file is a
	// second inode that two processes could hold at once.
	locksDir := filepath.Join(t.TempDir(), ".locks")
	guard, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	path := guard.Path()
	if err := guard.Release(); err != nil {
		t.Fatalf("Release() = %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Errorf("a second Release() must be a no-op: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the lock file must outlive the guard: %v", err)
	}
}

func TestAcquireRefusesANameThatIsNotOneComponentBeforeCreatingAnything(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), "store", "codex", ".locks")
	for _, name := range []string{"../escape.lock", "sub/x.lock", "..", ".", "", "x.lock/"} {
		_, err := Acquire(t.Context(), locksDir, name, soon())
		if _, ok := errors.AsType[*LockUnavailableError](err); !ok {
			t.Errorf("Acquire(name=%q) = %v, want a lock-unavailable error", name, err)
		}
	}
	if _, err := os.Stat(locksDir); !os.IsNotExist(err) {
		t.Errorf("nothing may be created on the way to a refusal, found %v", err)
	}
}

func TestASymlinkAtTheLockPathIsRefused(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	locksDir := filepath.Join(tmp, ".locks")
	if err := os.MkdirAll(locksDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v", locksDir, err)
	}
	elsewhere := filepath.Join(tmp, "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v", elsewhere, err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(locksDir, "acct.org.lock")); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}

	_, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if _, ok := errors.AsType[*RefusedSymlinkError](err); !ok {
		t.Fatalf("Acquire() = %v, want a refused-symlink error", err)
	}
	body, readErr := os.ReadFile(elsewhere)
	if readErr != nil {
		t.Fatalf("ReadFile(%q) = %v", elsewhere, readErr)
	}
	if string(body) != "{}" {
		t.Errorf("the link's target was written through: %q", body)
	}
}

func TestASymlinkedLocksDirectoryIsRefused(t *testing.T) {
	t.Parallel()

	// A link at the locks directory puts this store's locks — and with
	// them its idea of who may write a namespace — under somebody else's
	// control, so both the directory check and the descriptor the lock is
	// opened relative to refuse it.
	tmp := t.TempDir()
	elsewhere := filepath.Join(tmp, "someone-elses-locks")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v", elsewhere, err)
	}
	parent := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v", parent, err)
	}
	locksDir := filepath.Join(parent, ".locks")
	if err := os.Symlink(elsewhere, locksDir); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}

	_, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if _, ok := errors.AsType[*RefusedSymlinkError](err); !ok {
		t.Fatalf("Acquire() = %v, want a refused-symlink error", err)
	}
	entries, readErr := os.ReadDir(elsewhere)
	if readErr != nil {
		t.Fatalf("ReadDir(%q) = %v", elsewhere, readErr)
	}
	if len(entries) != 0 {
		t.Errorf("a lock file was created through the link: %v", entries)
	}
}

func TestAFileWhereTheLocksDirectoryBelongsIsRefused(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), ".locks")
	if err := os.WriteFile(locksDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v", locksDir, err)
	}

	_, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if _, ok := errors.AsType[*LockUnavailableError](err); !ok {
		t.Fatalf("Acquire() = %v, want a lock-unavailable error", err)
	}
}

func TestASymlinkedConfigurationDirectoryIsFollowedRatherThanRefused(t *testing.T) {
	t.Parallel()

	// The counterpart to the locks-directory rule, and the reason the two
	// paths are opened differently: the configuration directory is a path
	// the user names, and a dotfile manager pointing it at a repository is
	// ordinary. Everything else in the store follows it, so the lock has
	// to as well or the layout is only half-supported.
	tmp := t.TempDir()
	real := filepath.Join(tmp, "dotfiles-store")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v", real, err)
	}
	linked := filepath.Join(tmp, "store")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}

	guard, err := LockFile(t.Context(), filepath.Join(linked, ".config.lock"), soon())
	if err != nil {
		t.Fatalf("LockFile() = %v: a symlinked configuration directory is a supported layout", err)
	}
	defer func() { _ = guard.Release() }()
	if _, err := os.Stat(filepath.Join(real, ".config.lock")); err != nil {
		t.Errorf("the lock should land in the real directory: %v", err)
	}
}

func TestTwoNamesDoNotContend(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), ".locks")
	first, err := Acquire(t.Context(), locksDir, "acct.org-a.lock", soon())
	if err != nil {
		t.Fatalf("Acquire(org-a) = %v", err)
	}
	defer func() { _ = first.Release() }()
	second, err := Acquire(t.Context(), locksDir, "acct.org-b.lock", soon())
	if err != nil {
		t.Fatalf("Acquire(org-b) = %v: a different name must lock independently", err)
	}
	defer func() { _ = second.Release() }()
}

func TestAContendedLockReportsBusyAtTheDeadline(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), ".locks")
	held, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	defer func() { _ = held.Release() }()

	start := time.Now()
	_, err = Acquire(t.Context(), locksDir, "acct.org.lock", time.Now().Add(300*time.Millisecond))
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("Acquire() = %v, want %v", err, ErrLockBusy)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("it should give up promptly, waited %v", elapsed)
	}
}

func TestCancellationBeatsTheDeadline(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), ".locks")
	held, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	defer func() { _ = held.Release() }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	_, err = Acquire(ctx, locksDir, "acct.org.lock", time.Now().Add(60*time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() = %v, want a cancellation", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("a cancelled wait must not block, waited %v", elapsed)
	}
}

func TestASecondHolderWaitsAndThenAcquiresWhenTheFirstReleases(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), ".locks")
	first, err := Acquire(t.Context(), locksDir, "acct.org.lock", soon())
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}

	var released atomic.Bool
	type result struct {
		err        error
		waited     time.Duration
		sawRelease bool
	}
	results := make(chan result, 1)
	go func() {
		start := time.Now()
		guard, err := Acquire(t.Context(), locksDir, "acct.org.lock", time.Now().Add(10*time.Second))
		if guard != nil {
			defer func() { _ = guard.Release() }()
		}
		results <- result{err: err, waited: time.Since(start), sawRelease: released.Load()}
	}()

	// Long enough that the waiter has certainly failed at least one attempt.
	time.Sleep(600 * time.Millisecond)
	released.Store(true)
	if err := first.Release(); err != nil {
		t.Fatalf("Release() = %v", err)
	}

	got := <-results
	if got.err != nil {
		t.Fatalf("the second holder should get the lock once the first releases it: %v", got.err)
	}
	if got.waited < 500*time.Millisecond {
		t.Errorf("it waited only %v", got.waited)
	}
	if !got.sawRelease {
		t.Errorf("it acquired after the release, not before")
	}
}

func TestASecondProcessHoldingTheLockBlocksUntilItExits(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), ".config.lock")

	holder := exec.CommandContext(t.Context(), os.Args[0])
	holder.Env = append(os.Environ(), holderEnv+"="+lockPath)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() = %v", err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() = %v", err)
	}
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = holder.Wait()
	}()

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the holder's readiness line: %v", err)
	}
	if line != "held\n" {
		t.Fatalf("holder said %q, want %q", line, "held\n")
	}

	// The timeout path: the other process holds the lock, so a bounded
	// wait retries and then reports busy.
	start := time.Now()
	_, err = LockFile(t.Context(), lockPath, time.Now().Add(600*time.Millisecond))
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("LockFile() = %v, want %v", err, ErrLockBusy)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("the wait should have retried until the deadline, gave up after %v", elapsed)
	}

	// The wait path: once the holder exits, a waiter inside the 5 s
	// command budget acquires the same inode.
	if err := stdin.Close(); err != nil {
		t.Fatalf("closing the holder's stdin: %v", err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("the holder should exit cleanly: %v", err)
	}
	guard, err := LockFile(t.Context(), lockPath, time.Now().Add(NamespaceLockWait))
	if err != nil {
		t.Fatalf("LockFile() after the holder exited = %v", err)
	}
	defer func() { _ = guard.Release() }()
}

// The flock fail-closed seam and the split-create race live with the
// shared flock implementation; see the lockfile package's tests.
