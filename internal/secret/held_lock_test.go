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
	jsonv2 "encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/runtime/proc"
)

// fakeClock is a Clock every test can steer: both readings move only
// when the test says so, sleeps advance them and are recorded, and the
// jitter draw is pinned.
type fakeClock struct {
	mu     sync.Mutex
	wall   time.Time
	mono   time.Duration
	pinned time.Duration
	sleeps []time.Duration
	// onSleep, when set, replaces the default advance of both readings
	// by the slept duration — the hook a clock-skew scenario uses.
	onSleep func(c *fakeClock, slept time.Duration)
}

func newFakeClock() *fakeClock {
	return &fakeClock{wall: time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *fakeClock) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *fakeClock) Sleep(ctx context.Context, howLong time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, howLong)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook(c, howLong)
		return ctx.Err()
	}
	c.advance(howLong, howLong)
	return nil
}

func (c *fakeClock) Jitter(span time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if span <= 0 {
		return 0
	}
	return min(c.pinned, span-time.Nanosecond)
}

// advance moves the two readings independently, which is exactly what a
// clock step is.
func (c *fakeClock) advance(wall, mono time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(wall)
	c.mono += mono
}

// testStore returns a Paths whose namespace root exists, the state every
// hold writes its record into.
func testStore(t *testing.T) *config.Paths {
	t.Helper()
	paths := config.NewPaths(filepath.Join(t.TempDir(), "store"))
	if err := os.MkdirAll(paths.NamespaceRoot(), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v", paths.NamespaceRoot(), err)
	}
	return paths
}

func TestTheSerializedRecordIsTheExactDocumentTheDiagnosticsRead(t *testing.T) {
	t.Parallel()

	// The field names, their order and the tree spelling are the store
	// format: a change to any of them would orphan every record already
	// on disk, and would fail here rather than in somebody's stale-lock
	// removal months later.
	start := "1757400000"
	record := &HeldLockRecord{
		WriterPID:       4242,
		WriterStartTime: &start,
		Tree:            TreeLive,
		StoreDir:        "/Users/someone/.claude",
		Paths: []string{
			"/Users/someone/.claude/.oauth_refresh.lock",
			"/Users/someone/.claude.lock",
		},
		TakenAt: "2026-09-09T12:00:00Z",
	}

	body, err := jsonv2.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	want := `{"agctl_pid":4242,"agctl_start_time":"1757400000","tree":"live",` +
		`"store_dir":"/Users/someone/.claude",` +
		`"paths":["/Users/someone/.claude/.oauth_refresh.lock","/Users/someone/.claude.lock"],` +
		`"taken_at":"2026-09-09T12:00:00Z"}`
	if string(body) != want {
		t.Errorf("the on-disk document, exactly:\n got %s\nwant %s", body, want)
	}

	var parsed HeldLockRecord
	if err := jsonv2.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	if diff := gocmp.Diff(record, &parsed); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}

	// A record written by a build that predates the start time still
	// parses, and still reads as unknown rather than as a mismatch.
	older := []byte(`{"agctl_pid":7,"tree":"agctl","store_dir":"/store",` +
		`"paths":["/store/.oauth_refresh.lock"],"taken_at":"2026-01-01T00:00:00Z"}`)
	var legacy HeldLockRecord
	if err := jsonv2.Unmarshal(older, &legacy); err != nil {
		t.Fatalf("Unmarshal(older) = %v", err)
	}
	if legacy.WriterStartTime != nil {
		t.Errorf("WriterStartTime = %q, want unknown", *legacy.WriterStartTime)
	}
	if legacy.Tree != TreeOwn {
		t.Errorf("Tree = %q, want %q", legacy.Tree, TreeOwn)
	}
}

func TestWriteHeldLockRecordLandsWhereTheReaderReads(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	clock := newFakeClock()
	clock.advance(0, 1234*time.Millisecond)
	record := &HeldLockRecord{
		WriterPID: uint32(os.Getpid()),
		Tree:      TreeOwn,
		StoreDir:  filepath.Join(paths.NamespaceRoot(), "acct", "org"),
		Paths:     []string{filepath.Join(paths.NamespaceRoot(), "acct", "org", RefreshLockName)},
		TakenAt:   rfc3339UTC(clock.Wall()),
	}

	file, err := writeHeldLockRecord(t.Context(), paths, record, clock)
	if err != nil {
		t.Fatalf("writeHeldLockRecord() = %v", err)
	}
	if filepath.Dir(file) != HeldLocksDir(paths) {
		t.Errorf("record landed at %q, want inside %q", file, HeldLocksDir(paths))
	}
	name := filepath.Base(file)
	if want := fmt.Sprintf("%d-1234.json", os.Getpid()); name != want {
		t.Errorf("record name = %q, want %q", name, want)
	}

	got := ReadAllHeldLockRecords(paths)
	if len(got) != 1 {
		t.Fatalf("ReadAllHeldLockRecords() returned %d records, want 1", len(got))
	}
	if got[0].File != file {
		t.Errorf("File = %q, want %q", got[0].File, file)
	}
	if diff := gocmp.Diff(record, &got[0].Record); diff != "" {
		t.Errorf("record mismatch (-want +got):\n%s", diff)
	}

	// A second record under the same clock reading moves to the next
	// stamp instead of failing or disturbing the first.
	second, err := writeHeldLockRecord(t.Context(), paths, record, clock)
	if err != nil {
		t.Fatalf("a second writeHeldLockRecord() = %v", err)
	}
	if second == file {
		t.Errorf("the second record reused the first record's name %q", file)
	}
	if entries := ReadAllHeldLockRecords(paths); len(entries) != 2 {
		t.Errorf("ReadAllHeldLockRecords() returned %d records, want 2", len(entries))
	}
}

func TestReadAllIsEmptyWhenThereIsNoDirectoryAtAll(t *testing.T) {
	t.Parallel()

	// Every machine that has never held one of these locks.
	if got := ReadAllHeldLockRecords(testStore(t)); len(got) != 0 {
		t.Errorf("ReadAllHeldLockRecords() = %v, want none", got)
	}
}

func TestReadAllSkipsWhatIsNotAReadableRecord(t *testing.T) {
	t.Parallel()

	paths := testStore(t)
	dir := HeldLocksDir(paths)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	good := `{"agctl_pid":1,"agctl_start_time":null,"tree":"agctl","store_dir":"/s",` +
		`"paths":["/s/.oauth_refresh.lock"],"taken_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "1-1.json"), []byte(good), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	// None of these may surface, and none may stop the good one from
	// being returned: a report that refuses because one file is
	// malformed cannot diagnose anything.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a record"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2-2.json"), []byte(`{"agctl_pid":`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "3-3.json"), []byte(`{"agctl_pid":3,`+strings.Repeat(" ", MaxHeldLockRecordBytes)+`}`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "1-1.json"), filepath.Join(dir, "4-4.json")); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "5-5.json"), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}

	got := ReadAllHeldLockRecords(paths)
	if len(got) != 1 {
		t.Fatalf("ReadAllHeldLockRecords() returned %d records, want only the readable one: %v", len(got), got)
	}
	if got[0].Record.WriterPID != 1 {
		t.Errorf("WriterPID = %d, want 1", got[0].Record.WriterPID)
	}
}

func TestReadAllRefusesAPlantedHeldLocksDirectory(t *testing.T) {
	t.Parallel()

	// These records are the only thing that lets stale-lock removal act
	// outside the namespace root, so a listing somebody else could
	// redirect would be a permit somebody else could redirect.
	paths := testStore(t)
	elsewhere := t.TempDir()
	record := `{"agctl_pid":1,"agctl_start_time":null,"tree":"live","store_dir":"/s",` +
		`"paths":["/s/.oauth_refresh.lock"],"taken_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(elsewhere, "1-1.json"), []byte(record), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := os.Symlink(elsewhere, HeldLocksDir(paths)); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}

	if got := ReadAllHeldLockRecords(paths); len(got) != 0 {
		t.Errorf("a planted directory must read as no records, got %v", got)
	}

	// And the writer refuses the same plant before anything is written.
	clock := newFakeClock()
	if _, err := writeHeldLockRecord(t.Context(), paths, &HeldLockRecord{WriterPID: 1, Tree: TreeOwn, StoreDir: "/s", TakenAt: "t"}, clock); err == nil {
		t.Fatalf("writeHeldLockRecord() through a planted directory must refuse")
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatalf("ReadDir() = %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the planted directory gained a file: %v", entries)
	}
}

func TestAttestsComparesWholePathsAndNeverAParent(t *testing.T) {
	t.Parallel()

	record := &HeldLockRecord{
		Paths: []string{"/store/acct/org/.oauth_refresh.lock", "/store/acct/org.lock"},
	}

	tests := map[string]struct {
		path string
		want bool
	}{
		"success: an exact path is attested":              {path: "/store/acct/org/.oauth_refresh.lock", want: true},
		"success: the legacy lock is attested":            {path: "/store/acct/org.lock", want: true},
		"success: an unnormalised spelling still matches": {path: "/store/acct/org/./.oauth_refresh.lock", want: true},
		"error: a parent is not attested":                 {path: "/store/acct/org", want: false},
		"error: a child is not attested":                  {path: "/store/acct/org/.oauth_refresh.lock/x", want: false},
		"error: a prefix string is not attested":          {path: "/store/acct/org/.oauth", want: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := record.Attests(tt.path); got != tt.want {
				t.Errorf("Attests(%q) = %t, want %t", tt.path, got, tt.want)
			}
		})
	}
}

func TestTheAnchorIsTheStoreDirectorysParent(t *testing.T) {
	t.Parallel()

	record := &HeldLockRecord{StoreDir: "/Users/someone/.claude"}
	anchor, ok := record.Anchor()
	if !ok || anchor != "/Users/someone" {
		t.Errorf("Anchor() = %q, %t; want /Users/someone, true", anchor, ok)
	}

	rooted := &HeldLockRecord{StoreDir: "/"}
	if anchor, ok := rooted.Anchor(); ok {
		t.Errorf("a store with no parent has no anchor, got %q", anchor)
	}
}

func TestARecordWhoseProcessIsGoneIsALeak(t *testing.T) {
	t.Parallel()

	// A child that has already exited and been collected: its id names
	// nothing now.
	child := exec.CommandContext(t.Context(), "/usr/bin/true")
	if err := child.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	pid := child.Process.Pid
	if err := child.Wait(); err != nil {
		t.Fatalf("Wait() = %v", err)
	}

	gone := &HeldLockRecord{WriterPID: uint32(pid)}
	if !gone.WriterIsGone(t.Context()) {
		t.Errorf("an exited process must read as gone")
	}
}

func TestALiveProcessThatDidNotWriteTheRecordIsGoneToo(t *testing.T) {
	t.Parallel()

	// This process is alive, but the record claims a different start
	// identity: the kernel has handed the id to somebody else, and
	// treating the new tenant as the writer would block recovery from a
	// real leak for as long as it ran.
	recycled := "1999-12-31T23:59:59.000001Z"
	record := &HeldLockRecord{WriterPID: uint32(os.Getpid()), WriterStartTime: &recycled}
	if !record.WriterIsGone(t.Context()) {
		t.Errorf("a mismatched start identity must read as gone")
	}
}

func TestTheWriterOfARecordWithItsOwnIdentityIsNotGone(t *testing.T) {
	t.Parallel()

	self, err := proc.Lookup(t.Context(), os.Getpid())
	if err != nil {
		t.Fatalf("Lookup(self) = %v", err)
	}
	identity := self.StartIdentity()
	if identity == "" {
		t.Fatalf("this process must have a readable start identity")
	}
	record := &HeldLockRecord{WriterPID: uint32(os.Getpid()), WriterStartTime: &identity}
	if record.WriterIsGone(t.Context()) {
		t.Errorf("the live writer must not read as gone")
	}
}

func TestAnUnknownStartTimeIsNotEvidenceOfAnything(t *testing.T) {
	t.Parallel()

	// A record without the identity — an older build's — must not turn
	// into a permitted removal while its process id is alive.
	record := &HeldLockRecord{WriterPID: uint32(os.Getpid())}
	if record.WriterIsGone(t.Context()) {
		t.Errorf("a live process id with no recorded identity must not read as gone")
	}
}
