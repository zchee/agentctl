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
	"golang.org/x/sys/unix"
)

func configLockFixture(t *testing.T) (string, *fakeClock, *Seams) {
	t.Helper()
	clock := newFakeClockAt(time.Now())
	seams := RealSeams(clock)
	seams.Cleanup = &localLockCleanup{}
	return filepath.Join(t.TempDir(), ".claude.json"), clock, seams
}

func requireConfigLockError(t *testing.T, err error, kind ConfigLockErrorKind) *ConfigLockError {
	t.Helper()
	got, ok := errors.AsType[*ConfigLockError](err)
	if !ok || got.Kind != kind {
		t.Fatalf("configuration lock error=%v, want kind %d", err, kind)
	}
	return got
}

func TestConfigLockLiteralPath(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ linkParent bool }{
		"success: beside the named link rather than its target": {},
		"success: follows links only to the literal parent":     {linkParent: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, _, seams := configLockFixture(t)
			target := filepath.Join(t.TempDir(), "target.json")
			if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if tt.linkParent {
				link := filepath.Join(t.TempDir(), "home")
				if err := os.Symlink(filepath.Dir(path), link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, filepath.Base(path))
			}
			hold, err := AcquireConfigLock(t.Context(), path, seams)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(hold.Release)
			if hold.Shown() != path+".lock" {
				t.Fatalf("shown=%q, want literal %q", hold.Shown(), path+".lock")
			}
			info, err := os.Stat(hold.Shown())
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("lock directory=%v,%v", info, err)
			}
			requireAbsent(t, target+".lock")
			if err := hold.DriftCheck(); err != nil {
				t.Fatal(err)
			}
			hold.Release()
			requireAbsent(t, path+".lock")
		})
	}
}

func TestConfigLockContention(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		release bool
		jitter  time.Duration
		sleeps  []time.Duration
	}{
		"success: peer releases on first wait":   {release: true, sleeps: []time.Duration{200 * time.Millisecond}},
		"error: four attempts with three waits":  {sleeps: []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}},
		"error: jitter is applied to every rung": {jitter: 50 * time.Millisecond, sleeps: []time.Duration{250 * time.Millisecond, 450 * time.Millisecond, 850 * time.Millisecond}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, clock, seams := configLockFixture(t)
			child := spawnLockHolder(t, path+".lock")
			child.order("mkdir")
			fs := &observedLockFS{}
			seams.FS = fs
			clock.pinned = tt.jitter
			clock.onSleep = func(c *fakeClock, d time.Duration) {
				if tt.release {
					child.order("rmdir")
				}
				c.advance(d, d)
			}
			hold, err := AcquireConfigLock(t.Context(), path, seams)
			attempts := len(tt.sleeps) + 1
			if len(fs.ops) != attempts {
				t.Fatalf("attempts=%v, want %d mkdirs", fs.ops, attempts)
			}
			if tt.release {
				if err != nil {
					t.Fatal(err)
				}
				hold.Release()
			} else {
				_ = requireConfigLockError(t, err, ConfigLockBusy)
				child.order("rmdir")
			}
			if diff := gocmp.Diff(tt.sleeps, clock.sleeps); diff != "" {
				t.Fatal(diff)
			}
			child.exit()
		})
	}
}

func TestConfigLockStale(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		age time.Duration
		try bool
	}{
		"error: exactly stale is never broken": {age: 10 * time.Second},
		"error: old lock is never broken":      {age: time.Hour},
		"error: try once reports stale":        {age: 10 * time.Second, try: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, clock, seams := configLockFixture(t)
			child := spawnLockHolder(t, path+".lock")
			child.order("mkdir")
			stamp := clock.Wall().Add(-tt.age)
			if err := os.Chtimes(path+".lock", stamp, stamp); err != nil {
				t.Fatal(err)
			}
			acquire := AcquireConfigLock
			if tt.try {
				acquire = TryConfigLock
			}
			_, err := acquire(t.Context(), path, seams)
			failure := requireConfigLockError(t, err, ConfigLockStale)
			if failure.Age != tt.age {
				t.Fatalf("stale age=%v, want %v", failure.Age, tt.age)
			}
			info, err := os.Stat(path + ".lock")
			if err != nil || !info.ModTime().Equal(stamp) {
				t.Fatalf("stale lock changed: %v,%v", info, err)
			}
			if len(clock.sleeps) != 0 {
				t.Fatalf("stale lock caused sleeps: %v", clock.sleeps)
			}
			child.order("rmdir")
			child.exit()
		})
	}
}

func TestConfigLockVanished(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		flicker bool
		try     bool
		wantOps int
	}{
		"success: vanished holder retried immediately":   {wantOps: 2},
		"error: flickering holder retries once per rung": {flicker: true, wantOps: 8},
		"error: try once never retries vanished holder":  {try: true, wantOps: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, clock, seams := configLockFixture(t)
			child := spawnLockHolder(t, path+".lock")
			fs := &observedLockFS{}
			seams.FS = fs
			fs.beforeMkdir = func(LockSlot) {
				if tt.flicker || len(fs.ops) == 0 {
					child.order("mkdir")
				}
			}
			fs.afterMkdir = func(_ LockSlot, err error) {
				if errors.Is(err, ErrLockExists) {
					child.order("rmdir")
				}
			}
			acquire := AcquireConfigLock
			if tt.try {
				acquire = TryConfigLock
			}
			hold, err := acquire(t.Context(), path, seams)
			if len(fs.ops) != tt.wantOps {
				t.Fatalf("operations=%v, want %d attempts", fs.ops, tt.wantOps)
			}
			if tt.flicker || tt.try {
				_ = requireConfigLockError(t, err, ConfigLockBusy)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				hold.Release()
			}
			var sleeps []time.Duration
			if tt.flicker {
				sleeps = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
			}
			if diff := gocmp.Diff(sleeps, clock.sleeps); diff != "" {
				t.Fatal(diff)
			}
			child.exit()
		})
	}
}

func TestConfigHoldDriftAndElapsed(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		change   bool
		missing  bool
		baseline bool
	}{
		"success: elapsed uses monotonic time and budget belongs to caller": {},
		"error: changed mtime":                    {change: true},
		"error: missing held directory":           {missing: true},
		"error: unreadable baseline is not equal": {baseline: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, clock, seams := configLockFixture(t)
			if tt.baseline {
				fs := &observedLockFS{}
				fs.afterMkdir = func(at LockSlot, err error) {
					if err == nil {
						if err := os.Remove(at.Shown); err != nil {
							t.Fatal(err)
						}
					}
				}
				seams.FS = fs
			}
			hold, err := TryConfigLock(t.Context(), path, seams)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(hold.Release)
			if tt.change {
				info, err := os.Stat(path + ".lock")
				if err != nil {
					t.Fatal(err)
				}
				stamp := info.ModTime().Add(time.Nanosecond)
				if err := os.Chtimes(path+".lock", stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			if tt.missing {
				if err := os.Remove(path + ".lock"); err != nil {
					t.Fatal(err)
				}
			}
			elapsed := ConfigHoldBudget + time.Nanosecond
			clock.advance(-time.Hour, elapsed)
			if hold.Elapsed() != elapsed {
				t.Fatalf("elapsed=%v, want %v", hold.Elapsed(), elapsed)
			}
			err = hold.DriftCheck()
			if tt.change || tt.missing || tt.baseline {
				_ = requireConfigLockError(t, err, ConfigLockCompromised)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigHoldRelease(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		emergency bool
		untrack   bool
	}{
		"success: normal release removes tracked temp":    {},
		"success: emergency removes tracked temp":         {emergency: true},
		"success: untracked temp survives normal release": {untrack: true},
		"success: untracked temp survives emergency":      {emergency: true, untrack: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, _, seams := configLockFixture(t)
			hold, err := AcquireConfigLock(t.Context(), path, seams)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(hold.Release)
			parent := filepath.Dir(path)
			dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			err = hold.TrackTemp(dir, "pending.json")
			_ = unix.Close(dir)
			if err != nil {
				t.Fatal(err)
			}
			// Both the lock and the tracked temp keep their original directory.
			moved := parent + "-moved"
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(moved) })
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			temp := filepath.Join(moved, "pending.json")
			if err := os.WriteFile(temp, []byte("pending"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.untrack {
				hold.UntrackTemp()
			}
			if tt.emergency {
				seams.Cleanup.(*localLockCleanup).registry.Run()
				lock := filepath.Join(moved, filepath.Base(path)+".lock")
				if err := os.Mkdir(lock, 0o700); err != nil {
					t.Fatal(err)
				}
				hold.Release()
				if _, err := os.Stat(lock); err != nil {
					t.Fatalf("normal release removed a peer's retaken lock: %v", err)
				}
			} else {
				hold.Release()
				seams.Cleanup.(*localLockCleanup).registry.Run()
				requireAbsent(t, filepath.Join(moved, filepath.Base(path)+".lock"))
			}
			hold.Release()
			if tt.untrack {
				if _, err := os.Stat(temp); err != nil {
					t.Fatalf("untracked file removed: %v", err)
				}
			} else {
				requireAbsent(t, temp)
			}
		})
	}
}

func TestConfigHoldCleanupInFlight(t *testing.T) {
	t.Parallel()
	path, _, seams := configLockFixture(t)
	registry := seams.Cleanup.(*localLockCleanup)
	started, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	registry.registry.Register(func() { close(started); <-resume })
	hold, err := AcquireConfigLock(t.Context(), path, seams)
	if err != nil {
		t.Fatal(err)
	}
	go func() { defer close(done); registry.registry.Run() }()
	<-started
	hold.Release()
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("normal release removed a lock owned by in-flight cleanup: %v", err)
	}
	close(resume)
	<-done
	requireAbsent(t, path+".lock")
}

func TestConfigLockTryOnce(t *testing.T) {
	t.Parallel()
	path, clock, seams := configLockFixture(t)
	child := spawnLockHolder(t, path+".lock")
	child.order("mkdir")
	fs := &observedLockFS{}
	seams.FS = fs
	_, err := TryConfigLock(t.Context(), path, seams)
	_ = requireConfigLockError(t, err, ConfigLockBusy)
	if diff := gocmp.Diff([]string{"mkdir .claude.json.lock"}, fs.ops); diff != "" {
		t.Fatal(diff)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("try once slept: %v", clock.sleeps)
	}
	child.order("rmdir")
	child.exit()
}

func TestConfigHoldTrackTempRefusals(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		name     string
		badDir   bool
		released bool
	}{
		"error: parent traversal is not a temporary filename": {name: "../outside"},
		"error: dot is not a temporary filename":              {name: "."},
		"error: descriptor cannot be retained":                {name: "temp", badDir: true},
		"error: released hold cannot register a file":         {name: "temp", released: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, _, seams := configLockFixture(t)
			hold, err := AcquireConfigLock(t.Context(), path, seams)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(hold.Release)
			dir, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unix.Close(dir) }()
			if tt.released {
				hold.Release()
			}
			fd := dir
			if tt.badDir {
				fd = -1
			}
			_ = requireConfigLockError(t, hold.TrackTemp(fd, tt.name), ConfigLockIO)
		})
	}
}

func TestConfigLockParentDisappears(t *testing.T) {
	t.Parallel()
	path, _, seams := configLockFixture(t)
	fs := &observedLockFS{}
	fs.beforeMkdir = func(LockSlot) {
		if err := os.Remove(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
	}
	seams.FS = fs
	_, err := AcquireConfigLock(t.Context(), path, seams)
	_ = requireConfigLockError(t, err, ConfigLockUnreachable)
}

func TestConfigLockCancellation(t *testing.T) {
	t.Parallel()
	path, clock, seams := configLockFixture(t)
	child := spawnLockHolder(t, path+".lock")
	child.order("mkdir")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock.onSleep = func(*fakeClock, time.Duration) { cancel() }
	_, err := AcquireConfigLock(ctx, path, seams)
	_ = requireConfigLockError(t, err, ConfigLockCancelled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("cancelled waiter removed peer's lock: %v", err)
	}
	child.order("rmdir")
	child.exit()
}

func TestConfigLockUnreachable(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ path string }{
		"error: no filename":    {path: "/"},
		"error: missing parent": {path: "missing/.claude.json"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, _, seams := configLockFixture(t)
			if tt.path != "/" {
				tt.path = filepath.Join(filepath.Dir(path), tt.path)
			}
			_, err := AcquireConfigLock(t.Context(), tt.path, seams)
			_ = requireConfigLockError(t, err, ConfigLockUnreachable)
		})
	}
}
