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
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/cleanup"
)

// observedLockFS records real operations and lets a test arrange filesystem
// changes at syscall boundaries without substituting their results.
type observedLockFS struct {
	RealFS
	ops         []string
	beforeMkdir func(LockSlot)
	afterMkdir  func(LockSlot, error)
	afterRmdir  func(LockSlot)
}

func (f *observedLockFS) Mkdir(at LockSlot) error {
	if f.beforeMkdir != nil {
		f.beforeMkdir(at)
	}
	err := f.RealFS.Mkdir(at)
	f.ops = append(f.ops, "mkdir "+at.Name)
	if f.afterMkdir != nil {
		f.afterMkdir(at, err)
	}
	return err
}

func (f *observedLockFS) Rmdir(at LockSlot) error {
	err := f.RealFS.Rmdir(at)
	f.ops = append(f.ops, "rmdir "+at.Name)
	if f.afterRmdir != nil {
		f.afterRmdir(at)
	}
	return err
}

type localLockCleanup struct{ registry cleanup.Registry }

func (r *localLockCleanup) Register(fn func()) func() bool {
	token := r.registry.Register(fn)
	return func() bool { return r.registry.Unregister(token) }
}

func peerFixture(t *testing.T) (*config.Paths, LockSubject, *fakeClock, *Seams) {
	t.Helper()
	paths := testStore(t)
	store := paths.NamespaceDir("acct", "org")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClockAt(time.Now())
	seams := RealSeams(clock)
	seams.Cleanup = &localLockCleanup{}
	return paths, LockSubject{StoreDir: store, Tree: TreeOwn}, clock, seams
}

func acquirePeerFixture(t *testing.T, subject LockSubject, paths *config.Paths, seams *Seams) *HeldLocks {
	t.Helper()
	got, err := AcquirePeerLocks(t.Context(), subject, paths, nil, seams)
	if err != nil {
		t.Fatalf("AcquirePeerLocks() = %v", err)
	}
	if got.Busy() {
		t.Fatal("AcquirePeerLocks() = busy, want held")
	}
	t.Cleanup(got.Held.Release)
	return got.Held
}

func requireAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s remains: %v", path, err)
		}
	}
}

func requireNoHeldRecords(t *testing.T, paths *config.Paths) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(paths.NamespaceRoot(), "held-locks"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("held records remain: %v", entries)
	}
}

func TestPeerLocksAcquisitionOrder(t *testing.T) {
	t.Parallel()
	paths, subject, _, seams := peerFixture(t)
	fs := &observedLockFS{}
	seams.FS = fs
	fs.beforeMkdir = func(LockSlot) {
		entries, err := os.ReadDir(filepath.Join(paths.NamespaceRoot(), "held-locks"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("record must precede mkdir: %v, %v", entries, err)
		}
	}
	hold := acquirePeerFixture(t, subject, paths, seams)
	wantPaths := []string{filepath.Join(subject.StoreDir, RefreshLockName), subject.StoreDir + LegacyLockSuffix, filepath.Join(subject.StoreDir, StorageWriteLockName)}
	if diff := gocmp.Diff(wantPaths, hold.Paths()); diff != "" {
		t.Fatal(diff)
	}
	for _, path := range wantPaths {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("lock %s: %v, %v", path, info, err)
		}
	}
	if err := hold.DriftCheck(); err != nil {
		t.Fatal(err)
	}
	record := hold.RecordPath()
	fs.afterRmdir = func(LockSlot) {
		fs.afterRmdir = nil
		seams.Cleanup.(*localLockCleanup).registry.Run()
	}
	hold.Release()
	hold.Release()
	want := []string{"mkdir " + RefreshLockName, "mkdir org.lock", "mkdir " + StorageWriteLockName, "rmdir " + StorageWriteLockName, "rmdir org.lock", "rmdir " + RefreshLockName}
	if diff := gocmp.Diff(want, fs.ops); diff != "" {
		t.Fatal(diff)
	}
	requireAbsent(t, append(wantPaths, record)...)
}

func TestPeerLocksContention(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		release bool
		beat    bool
		jitter  time.Duration
		sleeps  []time.Duration
	}{
		"success: holder releases during contention":     {release: true, jitter: 250 * time.Millisecond, sleeps: []time.Duration{1250 * time.Millisecond}},
		"error: busy after five rounds and floor top up": {sleeps: []time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second, 2500 * time.Millisecond}},
		"error: beating holder needs no floor top up":    {beat: true, sleeps: []time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second}},
		"error: jitter reaches floor in five rounds":     {jitter: 500 * time.Millisecond, sleeps: []time.Duration{1500 * time.Millisecond, 1500 * time.Millisecond, 1500 * time.Millisecond, 1500 * time.Millisecond, 1500 * time.Millisecond}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, clock, seams := peerFixture(t)
			primary := filepath.Join(subject.StoreDir, RefreshLockName)
			child := spawnLockHolder(t, primary)
			child.order("mkdir")
			before, err := os.Stat(primary)
			if err != nil {
				t.Fatal(err)
			}
			clock.pinned = tt.jitter
			clock.onSleep = func(c *fakeClock, d time.Duration) {
				requireAbsent(t, subject.StoreDir+LegacyLockSuffix, filepath.Join(subject.StoreDir, StorageWriteLockName))
				requireNoHeldRecords(t, paths)
				if tt.release {
					child.order("rmdir")
				}
				if tt.beat {
					child.order("touch")
				}
				c.advance(d, d)
			}
			got, err := AcquirePeerLocks(t.Context(), subject, paths, nil, seams)
			if err != nil {
				t.Fatal(err)
			}
			if got.Busy() == tt.release {
				t.Fatalf("busy=%t, release=%t", got.Busy(), tt.release)
			}
			if got.HolderAlive != tt.beat {
				t.Fatalf("holder alive=%t, want %t", got.HolderAlive, tt.beat)
			}
			if diff := gocmp.Diff(tt.sleeps, clock.sleeps); diff != "" {
				t.Fatal(diff)
			}
			if got.Held != nil {
				got.Held.Release()
			} else {
				after, statErr := os.Stat(primary)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if !tt.beat && !after.ModTime().Equal(before.ModTime()) {
					t.Fatal("busy holder was modified")
				}
				child.order("rmdir")
			}
			child.exit()
		})
	}
}

func TestPeerLocksRestartExhaustion(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ position int }{
		"error: primary race": {0}, "error: legacy race": {1}, "error: storage race": {2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, clock, seams := peerFixture(t)
			lockPaths := []string{filepath.Join(subject.StoreDir, RefreshLockName), subject.StoreDir + LegacyLockSuffix, filepath.Join(subject.StoreDir, StorageWriteLockName)}
			child := spawnLockHolder(t, lockPaths[tt.position])
			fs := &observedLockFS{}
			seams.FS = fs
			fs.beforeMkdir = func(at LockSlot) {
				if at.Shown == lockPaths[tt.position] {
					child.order("mkdir")
				}
			}
			fs.afterMkdir = func(at LockSlot, err error) {
				if at.Shown == lockPaths[tt.position] && errors.Is(err, ErrLockExists) {
					child.order("rmdir")
				}
			}
			got, err := AcquirePeerLocks(t.Context(), subject, paths, nil, seams)
			if err != nil || !got.Busy() {
				t.Fatalf("acquire=%v, %v", got, err)
			}
			var round []string
			for _, path := range lockPaths[:tt.position+1] {
				round = append(round, "mkdir "+filepath.Base(path))
			}
			for i := tt.position - 1; i >= 0; i-- {
				round = append(round, "rmdir "+filepath.Base(lockPaths[i]))
			}
			var want []string
			for range MaxRestarts + 1 {
				want = append(want, round...)
			}
			if diff := gocmp.Diff(want, fs.ops); diff != "" {
				t.Fatal(diff)
			}
			if len(clock.sleeps) != 0 {
				t.Fatalf("waited while restarting: %v", clock.sleeps)
			}
			requireAbsent(t, lockPaths...)
			requireNoHeldRecords(t, paths)
			child.exit()
		})
	}
}

func TestPeerLocksPartialFailure(t *testing.T) {
	t.Parallel()
	paths, subject, _, seams := peerFixture(t)
	fs := &observedLockFS{}
	seams.FS = fs
	// Losing the just-created innermost directory makes its baseline unreadable.
	fs.afterMkdir = func(at LockSlot, err error) {
		if at.Name == StorageWriteLockName && err == nil {
			if err := os.Remove(at.Shown); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := AcquirePeerLocks(t.Context(), subject, paths, nil, seams)
	if got != nil || err == nil {
		t.Fatalf("acquire=%v,%v", got, err)
	}
	if _, ok := errors.AsType[*LockIOError](err); !ok {
		t.Fatalf("error type = %T: %v", err, err)
	}
	want := []string{"mkdir " + RefreshLockName, "mkdir org.lock", "mkdir " + StorageWriteLockName, "rmdir " + StorageWriteLockName, "rmdir org.lock", "rmdir " + RefreshLockName}
	if diff := gocmp.Diff(want, fs.ops); diff != "" {
		t.Fatal(diff)
	}
	requireNoHeldRecords(t, paths)
}

func TestPeerLocksDriftCheck(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		position int
		missing  bool
		elapsed  time.Duration
		code     int
	}{
		"success: exact budget is allowed": {elapsed: HoldBudget},
		"error: budget exceeded":           {elapsed: HoldBudget + time.Nanosecond, code: 10},
		"error: changed primary":           {position: 0, code: 10},
		"error: changed legacy":            {position: 1, code: 10},
		"error: changed storage":           {position: 2, code: 10},
		"error: unreadable primary":        {missing: true, code: 10},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, clock, seams := peerFixture(t)
			hold := acquirePeerFixture(t, subject, paths, seams)
			if tt.elapsed != 0 {
				clock.advance(tt.elapsed, tt.elapsed)
			} else {
				path := hold.Paths()[tt.position]
				if tt.missing {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					stamp := info.ModTime().Add(time.Nanosecond)
					if err := os.Chtimes(path, stamp, stamp); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := errs.ExitCode(LockRefusal(hold.DriftCheck())); got != tt.code {
				t.Fatalf("exit=%d,want %d", got, tt.code)
			}
		})
	}
}

func TestPeerLocksWriteAdmission(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		elapsed time.Duration
		code    int
	}{
		"success: exact remaining window": {elapsed: 1800 * time.Millisecond},
		"error: one nanosecond too late":  {elapsed: 1800*time.Millisecond + time.Nanosecond, code: 17},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, clock, seams := peerFixture(t)
			hold := acquirePeerFixture(t, subject, paths, seams)
			clock.advance(tt.elapsed, tt.elapsed)
			if got := errs.ExitCode(LockRefusal(hold.WriteAdmission())); got != tt.code {
				t.Fatalf("exit=%d,want %d", got, tt.code)
			}
		})
	}
}

func TestPeerLocksSingleBreak(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		second     bool
		retake     bool
		failRecord bool
	}{
		"success: one stale lock then hold":              {},
		"error: a second stale lock remains":             {second: true},
		"error: retaken lock is never broken again":      {retake: true},
		"error: completed break survives record failure": {failRecord: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, clock, seams := peerFixture(t)
			primary := filepath.Join(subject.StoreDir, RefreshLockName)
			child := spawnLockHolder(t, primary)
			child.order("mkdir")
			child.exit()
			old := clock.Wall().Add(-2 * time.Minute)
			if err := os.Chtimes(primary, old, old); err != nil {
				t.Fatal(err)
			}
			if tt.second {
				path := subject.StoreDir + LegacyLockSuffix
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			fs := &observedLockFS{}
			seams.FS = fs
			if tt.retake {
				fs.afterRmdir = func(at LockSlot) {
					if at.Name == RefreshLockName {
						if err := os.Mkdir(at.Shown, 0o700); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if tt.failRecord {
				if err := os.Symlink(t.TempDir(), filepath.Join(paths.NamespaceRoot(), "held-locks")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := AcquirePeerLocks(t.Context(), subject, paths, nil, seams)
			var record *BreakDraft
			if tt.failRecord {
				failure, ok := errors.AsType[*AcquireError](err)
				if !ok {
					t.Fatalf("error=%v", err)
				}
				record = failure.BreakRecord
			} else {
				if err != nil {
					t.Fatal(err)
				}
				record = got.BreakRecord
				if got.Busy() != (tt.second || tt.retake) {
					t.Fatalf("busy=%t", got.Busy())
				}
			}
			if record == nil || record.Outcome != OutcomeBroken {
				t.Fatalf("completed break lost: %+v", record)
			}
			if tt.retake && record.Reason != ReasonRetaken {
				t.Fatalf("reason=%q", record.Reason)
			}
			removals := 0
			for _, op := range fs.ops {
				if op == "rmdir "+RefreshLockName {
					removals++
				}
			}
			if removals != 1 {
				t.Fatalf("break removals=%v", fs.ops)
			}
			if diff := gocmp.Diff([]time.Duration{StaleSampleInterval}, clock.sleeps); diff != "" {
				t.Fatal(diff)
			}
			if got != nil && got.Held != nil {
				got.Held.Release()
			}
		})
	}
}

func TestPeerLocksCancellation(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ restart bool }{
		"error: cancellation during contention": {}, "error: cancellation after partial release": {restart: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, clock, seams := peerFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			primary := filepath.Join(subject.StoreDir, RefreshLockName)
			if tt.restart {
				fs := &observedLockFS{}
				seams.FS = fs
				fs.beforeMkdir = func(at LockSlot) {
					if at.Name == StorageWriteLockName {
						if err := os.Mkdir(at.Shown, 0o700); err != nil {
							t.Fatal(err)
						}
					}
				}
				fs.afterMkdir = func(at LockSlot, err error) {
					if at.Name == StorageWriteLockName && errors.Is(err, ErrLockExists) {
						if err := os.Remove(at.Shown); err != nil {
							t.Fatal(err)
						}
						cancel()
					}
				}
			} else {
				child := spawnLockHolder(t, primary)
				child.order("mkdir")
				clock.onSleep = func(*fakeClock, time.Duration) { child.order("rmdir"); cancel() }
			}
			got, err := AcquirePeerLocks(ctx, subject, paths, nil, seams)
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("acquire=%v,%v", got, err)
			}
			requireAbsent(t, primary, subject.StoreDir+LegacyLockSuffix, filepath.Join(subject.StoreDir, StorageWriteLockName))
			requireNoHeldRecords(t, paths)
		})
	}
}

func TestPeerLocksEmergencyRelease(t *testing.T) {
	t.Parallel()
	paths, subject, _, seams := peerFixture(t)
	registry := &localLockCleanup{}
	seams.Cleanup = registry
	fs := &observedLockFS{}
	seams.FS = fs
	hold := acquirePeerFixture(t, subject, paths, seams)
	registry.registry.Run()
	requireAbsent(t, append(hold.Paths(), hold.RecordPath())...)
	want := []string{"rmdir " + StorageWriteLockName, "rmdir org.lock", "rmdir " + RefreshLockName}
	if diff := gocmp.Diff(want, fs.ops[3:]); diff != "" {
		t.Fatal(diff)
	}
	// A peer may acquire immediately after the emergency release finishes.
	for _, path := range hold.Paths() {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hold.Release()
	for _, path := range hold.Paths() {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("normal release removed the peer's replacement %s: %v", path, err)
		}
	}
	if diff := gocmp.Diff(want, fs.ops[3:]); diff != "" {
		t.Fatalf("normal release repeated emergency removal: %s", diff)
	}
}

func TestPeerLockAnchor(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		tree     Tree
		linked   bool
		outside  bool
		env      string
		wantCode int
	}{
		"success: own store":                 {tree: TreeOwn},
		"success: linked live store":         {tree: TreeLive, linked: true},
		"error: linked own store":            {tree: TreeOwn, linked: true, wantCode: 1},
		"error: outside own root":            {tree: TreeOwn, outside: true, wantCode: 10},
		"error: namespaced live environment": {tree: TreeLive, env: "namespace", wantCode: 1},
		"error: unknown tree":                {tree: Tree("unknown"), wantCode: 10},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, _, _ := peerFixture(t)
			subject.Tree = tt.tree
			target := subject.StoreDir
			if tt.outside {
				subject.StoreDir = t.TempDir()
			}
			if tt.linked {
				target = t.TempDir()
				if err := os.Remove(subject.StoreDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, subject.StoreDir); err != nil {
					t.Fatal(err)
				}
			}
			anchor, err := OpenLockAnchor(subject, paths, &LiveStoreEnv{NamedStoreDir: subject.StoreDir, SecureStorageDir: tt.env})
			if code := errs.ExitCode(LockRefusal(err)); code != tt.wantCode {
				t.Fatalf("anchor error=%v,exit=%d,want %d", err, code, tt.wantCode)
			}
			if anchor != nil {
				defer anchor.Close()
				if tt.tree == TreeLive {
					resolved, resolveErr := filepath.EvalSymlinks(target)
					if resolveErr != nil {
						t.Fatal(resolveErr)
					}
					if anchor.StoreDir() != resolved {
						t.Fatalf("anchor=%s,target=%s", anchor.StoreDir(), resolved)
					}
				}
			}
		})
	}
}

func TestPeerLockAnchorSurvivesRename(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ tree Tree }{
		"success: owned store stays descriptor anchored": {tree: TreeOwn},
		"success: live store stays descriptor anchored":  {tree: TreeLive},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths, subject, _, seams := peerFixture(t)
			subject.Tree = tt.tree
			anchor, err := OpenLockAnchor(subject, paths, &LiveStoreEnv{NamedStoreDir: subject.StoreDir})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(anchor.Close)
			parent := filepath.Dir(subject.StoreDir)
			moved := parent + "-moved"
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(subject.StoreDir, 0o700); err != nil {
				t.Fatal(err)
			}
			got, err := AcquirePeerLocksWith(t.Context(), anchor, paths, seams)
			if err != nil || got.Busy() {
				t.Fatalf("acquisition after rename=%v,%v", got, err)
			}
			t.Cleanup(got.Held.Release)
			store := filepath.Join(moved, filepath.Base(subject.StoreDir))
			anchoredPaths := []string{filepath.Join(store, RefreshLockName), store + LegacyLockSuffix, filepath.Join(store, StorageWriteLockName)}
			for _, path := range anchoredPaths {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("missing descriptor-anchored lock %s: %v", path, err)
				}
			}
			requireAbsent(t, got.Held.Paths()...)
			if err := got.Held.DriftCheck(); err != nil {
				t.Fatal(err)
			}
			got.Held.Release()
			requireAbsent(t, anchoredPaths...)
		})
	}
}

func TestLockRefusal(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		err  error
		code int
	}{
		"success: no error":                {},
		"error: busy":                      {err: BusyRefusal(false, nil), code: 16},
		"error: compromised":               {err: LockRefusal(&CompromisedError{Path: "lock"}), code: 10},
		"error: budget":                    {err: LockRefusal(&BudgetExceededError{}), code: 10},
		"error: closed write window":       {err: LockRefusal(&WriteWindowClosedError{}), code: 17},
		"error: unreachable":               {err: LockRefusal(&UnreachableError{}), code: 1},
		"error: other acquisition failure": {err: LockRefusal(&LockIOError{}), code: 10},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if code := errs.ExitCode(tt.err); code != tt.code {
				t.Fatalf("exit=%d,want %d", code, tt.code)
			}
		})
	}
	if !errors.Is(LockRefusal(context.Canceled), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
