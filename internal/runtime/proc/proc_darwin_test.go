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

package proc

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// impossiblePID is a process id no running process can have: it is above
// every process-id limit this platform supports, and zero and negative
// values address process groups rather than single processes.
const impossiblePID = math.MaxInt32

// settle bounds how long a state transition is waited for, because
// "spawned" and "has run its first instruction" are different moments.
const settle = 5 * time.Second

// child is a process these tests spawned and therefore may signal; no other
// process id is ever signalled.
type child struct {
	t   *testing.T
	cmd *exec.Cmd
}

// spawnNamed starts a copy of the system sleep binary under the given file
// name, which is how the kernel comes to account for a process under that
// name, and waits until it is observably alive.
func spawnNamed(t *testing.T, name string) *child {
	t.Helper()

	binary := filepath.Join(t.TempDir(), name)
	source, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatalf("reading the sleep binary: %v", err)
	}
	if err := os.WriteFile(binary, source, 0o755); err != nil {
		t.Fatalf("copying the sleep binary to %q: %v", binary, err)
	}

	cmd := exec.Command(binary, "600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawning %q: %v", binary, err)
	}
	c := &child{t: t, cmd: cmd}
	t.Cleanup(c.reap)
	c.waitFor(HolderAlive)
	return c
}

func (c *child) pid() int {
	return c.cmd.Process.Pid
}

// stop suspends this child and waits for the kernel to show it.
func (c *child) stop() {
	c.t.Helper()
	if err := c.cmd.Process.Signal(unix.SIGSTOP); err != nil {
		c.t.Fatalf("stopping child %d: %v", c.pid(), err)
	}
	c.waitFor(HolderStopped)
}

// reap resumes, kills and collects this child. A stopped process stays
// stopped until it is resumed, so the resume comes first.
func (c *child) reap() {
	_ = c.cmd.Process.Signal(unix.SIGCONT)
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
}

// waitFor polls until the child reports the wanted state or the settle
// budget runs out.
func (c *child) waitFor(want Holder) {
	c.t.Helper()
	deadline := time.Now().Add(settle)
	var last Process
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = Lookup(c.t.Context(), c.pid())
		if lastErr == nil && last.Holder == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("child %d never reached %q: last observation %+v, err %v", c.pid(), want.Label(), last, lastErr)
}

func TestLookupOwnProcess(t *testing.T) {
	t.Parallel()

	p, err := Lookup(t.Context(), os.Getpid())
	if err != nil {
		t.Fatalf("Lookup(self): %v", err)
	}
	if got, want := p.PID, int32(os.Getpid()); got != want {
		t.Errorf("PID = %d, want %d", got, want)
	}
	if got, want := p.PPID, int32(os.Getppid()); got != want {
		t.Errorf("PPID = %d, want %d", got, want)
	}
	if got, want := p.PGID, int32(unix.Getpgrp()); got != want {
		t.Errorf("PGID = %d, want %d", got, want)
	}
	if p.Holder != HolderAlive {
		t.Errorf("Holder = %q, want %q", p.Holder.Label(), HolderAlive.Label())
	}
	if got, want := p.RealUID, uint32(os.Getuid()); got != want {
		t.Errorf("RealUID = %d, want %d", got, want)
	}
	if got, want := p.EffectiveUID, uint32(os.Geteuid()); got != want {
		t.Errorf("EffectiveUID = %d, want %d", got, want)
	}
	if p.Start.IsZero() {
		t.Error("Start is zero for the running test process")
	}
	if now := time.Now(); p.Start.After(now) {
		t.Errorf("a process cannot start in the future: %v > %v", p.Start, now)
	}
	if p.StartIdentity() == "" {
		t.Error("StartIdentity() is empty for the running test process")
	}

	again, err := Lookup(t.Context(), os.Getpid())
	if err != nil {
		t.Fatalf("second Lookup(self): %v", err)
	}
	if diff := gocmp.Diff(p.StartIdentity(), again.StartIdentity()); diff != "" {
		t.Errorf("start identity drifted between two reads of one process (-first +second):\n%s", diff)
	}
}

func TestLookupGoneOrImpossiblePIDs(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pid int
	}{
		"error: a pid above every limit is gone":          {pid: impossiblePID},
		"error: a large unused pid is gone":               {pid: 99999999},
		"error: pid zero names a group and never a peer":  {pid: 0},
		"error: a negative pid names a group, not a peer": {pid: -1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Lookup(t.Context(), tt.pid)
			if !errors.Is(err, ErrProcessGone) {
				t.Errorf("Lookup(%d) = %v, want ErrProcessGone", tt.pid, err)
			}
		})
	}
}

func TestLookupAnotherUsersProcess(t *testing.T) {
	t.Parallel()

	// Process id 1 is root's on every running system, which is the one
	// different-owner process a test can rely on without a second user
	// account on the machine.
	p, err := Lookup(t.Context(), 1)
	if err != nil {
		t.Fatalf("Lookup(1): %v", err)
	}
	if p.RealUID != 0 || p.EffectiveUID != 0 {
		t.Errorf("process 1 reports ruid=%d euid=%d, want root", p.RealUID, p.EffectiveUID)
	}
	if uid := os.Getuid(); uid == 0 {
		t.Fatal("this test needs a non-root caller so the ownership comparison has two sides")
	}
	if p.RealUID == uint32(os.Getuid()) {
		t.Errorf("process 1 (ruid %d) must not look owned by uid %d", p.RealUID, os.Getuid())
	}
	if p.Start.IsZero() {
		t.Error("another user's start time is readable here and must not come back refused")
	}
}

func TestListContainsSelfAndInit(t *testing.T) {
	t.Parallel()

	processes, err := List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(processes) <= 1 {
		t.Fatalf("a machine running this test runs more than one process, got %d", len(processes))
	}
	var foundSelf, foundInit bool
	for _, p := range processes {
		if p.PID <= 0 {
			t.Errorf("padding entries must be dropped, got pid %d", p.PID)
		}
		switch int(p.PID) {
		case os.Getpid():
			foundSelf = true
		case 1:
			foundInit = true
		}
	}
	if !foundSelf {
		t.Error("the list must include the reader")
	}
	if !foundInit {
		t.Error("the list must include other users' processes such as pid 1")
	}
}

func TestListRefusesACancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := List(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("List with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := Lookup(ctx, os.Getpid()); !errors.Is(err, context.Canceled) {
		t.Errorf("Lookup with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestChildLifecycleIsObserved(t *testing.T) {
	t.Parallel()

	before := time.Now()
	c := spawnNamed(t, "sleeper")

	p, err := Lookup(t.Context(), c.pid())
	if err != nil {
		t.Fatalf("Lookup(child): %v", err)
	}
	if p.Holder != HolderAlive {
		t.Errorf("a fresh child is %q, want alive", p.Holder.Label())
	}
	if got, want := p.PPID, int32(os.Getpid()); got != want {
		t.Errorf("child PPID = %d, want the test process %d", got, want)
	}
	if got, want := p.RealUID, uint32(os.Getuid()); got != want {
		t.Errorf("child RealUID = %d, want the spawning user %d", got, want)
	}
	if got, want := p.EffectiveUID, uint32(os.Geteuid()); got != want {
		t.Errorf("child EffectiveUID = %d, want the spawning user %d", got, want)
	}
	if got, want := p.Name, "sleeper"; got != want {
		t.Errorf("child Name = %q, want %q", got, want)
	}
	if gap := p.Start.Sub(before).Abs(); gap > time.Second {
		t.Errorf("child start %v is %v away from its spawn at %v", p.Start, gap, before)
	}

	// A stopped process answers an existence probe exactly as a running
	// one does, which is why the run state has to come from the kernel's
	// accounting record.
	c.stop()
	p, err = Lookup(t.Context(), c.pid())
	if err != nil {
		t.Fatalf("Lookup(stopped child): %v", err)
	}
	if p.Holder != HolderStopped {
		t.Errorf("a suspended child is %q, want stopped", p.Holder.Label())
	}

	if err := c.cmd.Process.Signal(unix.SIGCONT); err != nil {
		t.Fatalf("resuming child %d: %v", c.pid(), err)
	}
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("killing child %d: %v", c.pid(), err)
	}
	// Killed but not yet collected: the kernel still has a record, and
	// that record means dead, because an uncollected exit releases
	// nothing either.
	c.waitFor(HolderDead)

	if _, err := c.cmd.Process.Wait(); err != nil {
		t.Fatalf("collecting child %d: %v", c.pid(), err)
	}
	if _, err := Lookup(t.Context(), c.pid()); !errors.Is(err, ErrProcessGone) {
		t.Errorf("Lookup(reaped child) = %v, want ErrProcessGone", err)
	}
}

func TestWriterGone(t *testing.T) {
	t.Parallel()

	c := spawnNamed(t, "sleeper")
	identity, err := Lookup(t.Context(), c.pid())
	if err != nil {
		t.Fatalf("Lookup(child): %v", err)
	}
	recorded := identity.StartIdentity()
	if recorded == "" {
		t.Fatal("a live child must have a renderable start identity")
	}

	tests := map[string]struct {
		recorded string
		want     bool
	}{
		"success: a live writer with its own identity is not gone": {
			recorded: recorded,
			want:     false,
		},
		"success: a missing recorded identity proves nothing": {
			recorded: "",
			want:     false,
		},
		"success: a differing identity proves the pid was recycled": {
			recorded: "2001-01-01T00:00:00.000001Z",
			want:     true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := WriterGone(t.Context(), c.pid(), tt.recorded); got != tt.want {
				t.Errorf("WriterGone(%d, %q) = %v, want %v", c.pid(), tt.recorded, got, tt.want)
			}
		})
	}

	// A suspended writer is not gone under any recorded identity: stopped
	// is exactly the state the caller must keep waiting on or report.
	c.stop()
	if WriterGone(t.Context(), c.pid(), recorded) {
		t.Error("a stopped writer with a matching identity must not count as gone")
	}

	c.reap()
	if !WriterGone(t.Context(), c.pid(), recorded) {
		t.Error("a collected writer is gone under its recorded identity")
	}
	if !WriterGone(t.Context(), c.pid(), "") {
		t.Error("a collected writer is gone even without a recorded identity")
	}
}

func TestSameUserNamedMatchesExactly(t *testing.T) {
	t.Parallel()

	exact := spawnNamed(t, "claude")
	helper := spawnNamed(t, "Claude Helper")
	prefixed := spawnNamed(t, "claude-code")

	find := func(processes []Process, pid int) (Process, bool) {
		for _, p := range processes {
			if int(p.PID) == pid {
				return p, true
			}
		}
		return Process{}, false
	}

	found, err := SameUserNamed(t.Context(), "claude")
	if err != nil {
		t.Fatalf("SameUserNamed: %v", err)
	}
	p, ok := find(found, exact.pid())
	if !ok {
		t.Fatalf("a same-user process named exactly claude must be found, got %+v", found)
	}
	if p.Holder != HolderAlive {
		t.Errorf("the running match is %q, want alive", p.Holder.Label())
	}
	for _, other := range []*child{helper, prefixed} {
		if _, ok := find(found, other.pid()); ok {
			t.Errorf("a name that merely contains the wanted name must not match, pid %d did", other.pid())
		}
	}
	for _, p := range found {
		if got, want := p.RealUID, uint32(os.Getuid()); got != want {
			t.Errorf("every match must belong to the caller, pid %d has ruid %d, want %d", p.PID, got, want)
		}
	}

	// The question the caller actually asks: is any same-user process of
	// this name stopped. The suspended child must be listed as stopped.
	exact.stop()
	found, err = SameUserNamed(t.Context(), "claude")
	if err != nil {
		t.Fatalf("SameUserNamed after stop: %v", err)
	}
	p, ok = find(found, exact.pid())
	if !ok {
		t.Fatalf("a stopped same-user match must still be found, got %+v", found)
	}
	if p.Holder != HolderStopped {
		t.Errorf("the suspended match is %q, want stopped", p.Holder.Label())
	}
}

func TestSameUserNamedExcludesAnotherUsersProcess(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Fatal("this test needs a non-root caller so pid 1 belongs to somebody else")
	}
	// Process id 1 is root's process named launchd; a different user
	// account is not available on a test machine, so root's one guaranteed
	// process is the other side of the ownership comparison.
	found, err := SameUserNamed(t.Context(), "launchd")
	if err != nil {
		t.Fatalf("SameUserNamed: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("another user's process must never match, got %+v", found)
	}
}

func TestLongNameTruncatesToSixteenBytes(t *testing.T) {
	t.Parallel()

	// The kernel accounts for at most 16 bytes of command name, so an
	// executable with a longer file name is observed truncated. For a
	// 6-byte exact-match target this is harmless: the truncated spelling
	// can never equal the short name, so the observation is a documented
	// non-match rather than a failure.
	const long = "claudeclaudeclaude"
	c := spawnNamed(t, long)

	p, err := Lookup(t.Context(), c.pid())
	if err != nil {
		t.Fatalf("Lookup(long-named child): %v", err)
	}
	if diff := gocmp.Diff(long[:16], p.Name); diff != "" {
		t.Errorf("observed name mismatch (-want +got):\n%s", diff)
	}
	if p.Name == long {
		t.Errorf("an 18-byte name cannot be observed whole, got %q", p.Name)
	}
	if p.Name == "claude" {
		t.Errorf("a truncated long name must not collapse into the exact short name, got %q", p.Name)
	}

	found, err := SameUserNamed(t.Context(), "claude")
	if err != nil {
		t.Fatalf("SameUserNamed: %v", err)
	}
	for _, match := range found {
		if int(match.PID) == c.pid() {
			t.Errorf("the long-named child must not match the exact short name, got %+v", match)
		}
	}
}

func TestHolderFromStatus(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status int8
		want   Holder
	}{
		"success: a process being created is alive": {
			status: statusIdle,
			want:   HolderAlive,
		},
		"success: a runnable process is alive": {
			status: statusRunnable,
			want:   HolderAlive,
		},
		"success: a sleeping process is alive": {
			status: statusSleeping,
			want:   HolderAlive,
		},
		"success: a suspended process is stopped": {
			status: statusStopped,
			want:   HolderStopped,
		},
		"success: an uncollected exit is dead": {
			status: statusZombie,
			want:   HolderDead,
		},
		"success: an unknown state is alive because the record exists": {
			status: 99,
			want:   HolderAlive,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := holderFromStatus(tt.status); got != tt.want {
				t.Errorf("holderFromStatus(%d) = %q, want %q", tt.status, got.Label(), tt.want.Label())
			}
		})
	}
}

func TestStartFrom(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		tv   unix.Timeval
		want string
	}{
		"success: microseconds survive into the identity": {
			tv:   unix.Timeval{Sec: 1757400000, Usec: 123456},
			want: "2025-09-09T06:40:00.123456Z",
		},
		"success: one microsecond later is a different identity": {
			tv:   unix.Timeval{Sec: 1757400000, Usec: 123457},
			want: "2025-09-09T06:40:00.123457Z",
		},
		"success: the last believable second renders": {
			tv:   unix.Timeval{Sec: maxStartSecond, Usec: 0},
			want: "9999-12-31T23:59:59Z",
		},
		"error: a negative second is refused": {
			tv:   unix.Timeval{Sec: -1, Usec: 0},
			want: "",
		},
		"error: a second past the year 9999 is refused": {
			tv:   unix.Timeval{Sec: maxStartSecond + 1, Usec: 0},
			want: "",
		},
		"error: microseconds overflowing a second are refused": {
			tv:   unix.Timeval{Sec: 1757400000, Usec: 1000000},
			want: "",
		},
		"error: negative microseconds are refused": {
			tv:   unix.Timeval{Sec: 1757400000, Usec: -1},
			want: "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p := Process{Start: startFrom(tt.tv)}
			if diff := gocmp.Diff(tt.want, p.StartIdentity()); diff != "" {
				t.Errorf("startFrom(%+v) identity mismatch (-want +got):\n%s", tt.tv, diff)
			}
		})
	}
}

func TestCommandName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		comm [17]byte
		want string
	}{
		"success: the bytes before the terminator are the name": {
			comm: [17]byte{'c', 'l', 'a', 'u', 'd', 'e', 0},
			want: "claude",
		},
		"success: a full-width name keeps all sixteen bytes": {
			comm: [17]byte{'c', 'l', 'a', 'u', 'd', 'e', 'c', 'l', 'a', 'u', 'd', 'e', 'c', 'l', 'a', 'u', 0},
			want: "claudeclaudeclau",
		},
		"success: an empty record has an empty name": {
			comm: [17]byte{},
			want: "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := gocmp.Diff(tt.want, commandName(tt.comm)); diff != "" {
				t.Errorf("commandName mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
