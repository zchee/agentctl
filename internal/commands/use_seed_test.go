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

//go:build agentctl_testing

package commands

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

// seedLiveMembers are the members the live item keeps beside its account
// object in these tests: two MCP logins and a plugin secret, each holding
// the canary, so a value that travels anywhere but the item is found.
const seedLiveMembers = `"mcpOAuth":{"alpha|0001":{"accessToken":"` + siblingCanary + `-alpha"},"beta|0002":{"accessToken":"` + siblingCanary + `-beta"}},"pluginSecrets":{"plugin":"` + siblingCanary + `-plugin"}`

// seedFileMember is a member an owned file carries of its own, as Claude
// Code's plaintext fallback writes one.
const seedFileMember = `"mcpOAuth":{"own|0003":{"accessToken":"FILE-SIBLING-own"}}`

// seedKeptSentence is the sentence every kept-file warning ends with.
const seedKeptSentence = "`agentctl claude use --live account` reads this file until it is removed"

// seedWorld is one store with an owned namespace whose keychain item is
// absent, a live keychain item and the fake security(1) every call reaches.
type seedWorld struct {
	fixture *testutil.Fixture
	paths   *config.Paths
	record  *config.AccountRecord
	env     claude.EnvView
	process SessionProcess
	nsDir   string
	file    string
	service string
	owned   string
	live    string
	clock   secret.Clock
}

func newSeedWorld(t *testing.T) *seedWorld {
	t.Helper()
	fixture := testutil.New(t)
	fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
	paths := config.NewPaths(fixture.ConfigDir())
	registry, err := config.LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	record := registry.Get(testutil.Acct, testutil.Org)
	service := claude.LiveService + "-" + record.Kind.Owned.ExportSHA8
	owned := fixture.Blob("seed-access", "seed-refresh", testutil.FreshAt())
	file := fixture.WriteCredentials(testutil.Acct, testutil.Org, owned)
	live := withMembers(t, fixture.IdentifiedBlob("live-access", "live-refresh", testutil.FreshAt(), "77777777-7777-4777-8777-777777777777", testutil.Org), seedLiveMembers)
	fixture.WithKeychain()
	fixture.KeychainItem(claude.LiveService, live).AllowWrite(service).Dump(claude.LiveService)
	for _, entry := range fixture.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" || key == "LOGNAME" {
			t.Setenv(key, value)
		}
	}
	t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
	t.Setenv("AGENTCTL_FAULT", "")
	_, controller := signals.Install(t.Context())
	t.Cleanup(controller.Stop)
	return &seedWorld{
		fixture: fixture, paths: paths, record: record, env: claude.EnvWithHome(fixture.Home()),
		process: SessionProcess{Signals: controller},
		nsDir:   fixture.NamespaceDir(testutil.Acct, testutil.Org), file: file, service: service, owned: owned, live: live,
	}
}

func (w *seedWorld) seed(ctx context.Context, reader secret.Reader) *useSeedReport {
	if reader == nil {
		reader = secret.NewReader()
	}
	return w.process.seedNamespaceSiblings(ctx, seedInput{paths: w.paths, record: w.record, env: &w.env, reader: reader, id: "account", clock: w.clock})
}

// seedPaused runs a seed held at the named pause point, calls act there and
// resumes it. It returns the report and when the run was resumed.
func (w *seedWorld) seedPaused(t *testing.T, ctx context.Context, reader secret.Reader, point string, act func()) (*useSeedReport, time.Time) {
	t.Helper()
	resume := w.fixture.Scratch("resume-" + point)
	t.Setenv("AGENTCTL_FAULT", "pause_"+point)
	t.Setenv("AGENTCTL_FAULT_RESUME", resume)
	done := make(chan *useSeedReport, 1)
	go func() { done <- w.seed(ctx, reader) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(resume + ".reached"); err == nil {
			break
		}
		select {
		case report := <-done:
			t.Fatalf("the seed ended %s before it reached %s", report.Outcome, point)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the seed never reached %s", point)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if act != nil {
		act()
	}
	resumed := time.Now()
	if err := os.WriteFile(resume, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return <-done, resumed
}

func (w *seedWorld) itemPath() string { return w.fixture.KeychainItemPath(w.service) }

// writes counts the keychain writes the fake logged.
func (w *seedWorld) writes() int {
	n := 0
	for _, line := range w.fixture.SecurityLog() {
		if strings.HasPrefix(line, "add-generic-password") {
			n++
		}
	}
	return n
}

// reads counts the item reads the fake logged for service.
func (w *seedWorld) reads(service string) int {
	n := 0
	for _, line := range w.fixture.SecurityLog() {
		if strings.HasPrefix(line, "find-generic-password") && strings.HasSuffix(line, " -s "+service) {
			n++
		}
	}
	return n
}

// seedEntries returns every seed_write entry of the audit log.
func (w *seedWorld) seedEntries(t *testing.T) []*secret.SeedEvent {
	t.Helper()
	tail, err := secret.TailAuditLog(w.paths, 64)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(tail.Unreadable) != 0 {
		t.Fatalf("the audit log holds %d unreadable lines", len(tail.Unreadable))
	}
	var seeds []*secret.SeedEvent
	for _, entry := range tail.Entries {
		if event, ok := entry.Event.(*secret.SeedEvent); ok {
			seeds = append(seeds, event)
		}
	}
	return seeds
}

// lockLeftovers lists the peer-lock directories and held-lock records left
// in the store.
func (w *seedWorld) lockLeftovers(t *testing.T) []string {
	t.Helper()
	var left []string
	for _, path := range []string{filepath.Join(w.nsDir, secret.RefreshLockName), w.nsDir + secret.LegacyLockSuffix, filepath.Join(w.nsDir, secret.StorageWriteLockName)} {
		if _, err := os.Lstat(path); err == nil {
			left = append(left, filepath.Base(path))
		}
	}
	records, err := os.ReadDir(secret.HeldLocksDir(w.paths))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, record := range records {
		left = append(left, secret.HeldLocksDirName+"/"+record.Name())
	}
	return left
}

func (w *seedWorld) write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedRecorder wraps a reader, recording the deadline of every call and
// optionally blocking one call until its context ends or cancelling the run
// after a given read of one service returns.
type seedRecorder struct {
	next      secret.Reader
	blockList bool
	blockRead string
	cancelOn  string
	cancelAt  int
	cancel    context.CancelFunc

	mu        sync.Mutex
	calls     []string
	deadlines []time.Time
	reads     map[string]int
}

func (r *seedRecorder) note(ctx context.Context, call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	deadline, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, deadline)
}

func (r *seedRecorder) Preflight(ctx context.Context) secret.KeychainStatus {
	r.note(ctx, "preflight")
	return r.next.Preflight(ctx)
}

func (r *seedRecorder) ListServices(ctx context.Context, prefix string) ([]secret.ServiceEntry, error) {
	r.note(ctx, "list")
	if r.blockList {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.next.ListServices(ctx, prefix)
}

func (r *seedRecorder) Read(ctx context.Context, service string) (*secret.Secret, error) {
	r.note(ctx, "read "+service)
	if service == r.blockRead {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sealed, err := r.next.Read(ctx, service)
	r.mu.Lock()
	if r.reads == nil {
		r.reads = map[string]int{}
	}
	r.reads[service]++
	fire := r.cancel != nil && service == r.cancelOn && r.reads[service] == r.cancelAt
	r.mu.Unlock()
	if fire {
		r.cancel()
	}
	return sealed, err
}

// seedWant is what one seed must end with.
type seedWant struct {
	outcome   seedOutcome
	path      string
	ownedFile string
	readback  string
	note      []string
	notNote   []string
	writes    int
	audit     string
	auditFile string
	// file is "same" (the fixture's bytes), "gone", or "changed" (the bytes
	// the case's own action wrote).
	file string
	// parsed is the least number of credentials with member values the
	// seed must have parsed, every one of which it must have wiped.
	parsed int
	// leftovers are the lock artefacts the fixture itself made.
	leftovers []string
}

func TestSeedNamespaceSiblingsOutcomes(t *testing.T) {
	tests := map[string]struct {
		setup  func(t *testing.T, w *seedWorld)
		pause  string
		act    func(t *testing.T, w *seedWorld, cancel context.CancelFunc) string
		record func(r *seedRecorder, cancel context.CancelFunc)
		want   seedWant
		// check runs extra assertions after the common ones.
		check func(t *testing.T, w *seedWorld, report *useSeedReport)
	}{
		"absent item: created from the owned file and the live members, the owned file removed": {
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "removed", readback: "equal", writes: 1, audit: "applied", auditFile: "removed", file: "gone", parsed: 2},
			check: func(t *testing.T, w *seedWorld, report *useSeedReport) {
				two := 2
				want := []useCarriedMember{{Name: "mcpOAuth", Entries: &two}, {Name: "pluginSecrets", Entries: new(1)}}
				if diff := gocmp.Diff(want, report.SeededMembers); diff != "" {
					t.Errorf("seeded members (-want +got):\n%s", diff)
				}
				if got := report.prose(); got != "seeded mcpOAuth (2 entries), pluginSecrets (1 entry) into "+w.service+"; removed the owned credential file" {
					t.Errorf("prose = %q", got)
				}
			},
		},
		"present item in the listing: no item read, no write": {
			setup: func(t *testing.T, w *seedWorld) {
				w.fixture.KeychainItem(w.service, w.owned).Dump(claude.LiveService, w.service)
			},
			want: seedWant{outcome: seedUnchanged, path: "present", file: "same"},
			check: func(t *testing.T, w *seedWorld, report *useSeedReport) {
				if w.reads(w.service) != 0 || w.reads(claude.LiveService) != 0 {
					t.Errorf("a present item was read: %v", w.fixture.SecurityLog())
				}
				if report.prose() != "" {
					t.Errorf("an unchanged seed printed %q", report.prose())
				}
			},
		},
		"present item the listing missed: the confirming read finds it and nothing is written": {
			setup: func(t *testing.T, w *seedWorld) {
				w.fixture.KeychainItem(w.service, withMembers(t, w.owned, seedFileMember))
			},
			want: seedWant{outcome: seedUnchanged, path: "present", file: "same", parsed: 1},
			check: func(t *testing.T, w *seedWorld, _ *useSeedReport) {
				if w.reads(w.service) != 1 || w.reads(claude.LiveService) != 0 {
					t.Errorf("reads: %v", w.fixture.SecurityLog())
				}
			},
		},
		"secure-storage override in agentctl's environment: no keychain call at all": {
			setup: func(t *testing.T, w *seedWorld) { w.env.SecureStorageDir = new("/elsewhere") },
			want:  seedWant{outcome: seedSkipped, note: []string{"secure-storage override"}, file: "same"},
			check: func(t *testing.T, w *seedWorld, report *useSeedReport) {
				if log := w.fixture.SecurityLog(); len(log) != 0 || report.prose() != "" {
					t.Errorf("keychain calls %v, prose %q", log, report.prose())
				}
			},
		},
		"locked keychain: a silent skip with no item read": {
			setup: func(t *testing.T, _ *seedWorld) { t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT", "36") },
			want:  seedWant{outcome: seedSkipped, note: []string{"the keychain is locked"}, file: "same"},
			check: func(t *testing.T, w *seedWorld, report *useSeedReport) {
				if w.reads(w.service)+w.reads(claude.LiveService) != 0 || report.prose() != "" {
					t.Errorf("reads %v, prose %q", w.fixture.SecurityLog(), report.prose())
				}
			},
		},
		"two items carry the namespace service: the target is refused before any read": {
			setup: func(t *testing.T, w *seedWorld) { w.fixture.Dump(claude.LiveService, w.service, w.service) },
			want:  seedWant{outcome: seedSkipped, note: []string{"more than one keychain item carries this service name"}, file: "same"},
			check: func(t *testing.T, w *seedWorld, _ *useSeedReport) {
				if w.reads(w.service)+w.reads(claude.LiveService) != 0 {
					t.Errorf("reads: %v", w.fixture.SecurityLog())
				}
			},
		},
		"live item absent: skipped": {
			setup: func(t *testing.T, w *seedWorld) {
				if err := os.Remove(w.fixture.KeychainItemPath(claude.LiveService)); err != nil {
					t.Fatal(err)
				}
			},
			want: seedWant{outcome: seedSkipped, path: "absent", note: []string{seedLiveNote}, file: "same"},
		},
		"live item without members: unchanged and silent": {
			setup: func(t *testing.T, w *seedWorld) { w.fixture.KeychainItem(claude.LiveService, w.owned) },
			want:  seedWant{outcome: seedUnchanged, path: "absent", file: "same"},
		},
		"owned file absent: skipped with the login note": {
			setup: func(t *testing.T, w *seedWorld) {
				if err := os.Remove(w.file); err != nil {
					t.Fatal(err)
				}
			},
			want: seedWant{outcome: seedSkipped, path: "absent", note: []string{seedNoFileNote}, file: "gone", parsed: 1},
		},
		"pending write parked: skipped with the pending note": {
			setup: func(t *testing.T, w *seedWorld) { w.write(t, filepath.Join(w.nsDir, secret.PendingFile), "pending") },
			want:  seedWant{outcome: seedSkipped, path: "absent", note: []string{seedPendingNote}, file: "same", parsed: 1},
		},
		"owned file with a member of its own: own_state before any lock": {
			setup: func(t *testing.T, w *seedWorld) {
				w.owned = withMembers(t, w.owned, seedFileMember)
				w.write(t, w.file, w.owned)
			},
			want: seedWant{outcome: seedOwnState, path: "absent", note: []string{"holds MCP state of its own (`mcpOAuth`)", "it was left as it is and nothing was seeded"}, notNote: []string{"FILE-SIBLING"}, file: "same", parsed: 2},
			check: func(t *testing.T, w *seedWorld, report *useSeedReport) {
				if !strings.Contains(*report.Note, w.file) {
					t.Errorf("the note does not name %s: %s", w.file, *report.Note)
				}
			},
		},
		"oversize document: skipped as credential_too_large before any lock": {
			setup: func(t *testing.T, w *seedWorld) {
				w.fixture.KeychainItem(claude.LiveService, withMembers(t, w.live, `"padding":"`+strings.Repeat("z", secret.KeychainBlobLimit)+`"`))
			},
			want: seedWant{outcome: seedSkipped, path: "absent", note: []string{seedTooLargeNote}, file: "same", parsed: 1},
		},
		"item created before the hold: discarded and the item left as it was": {
			pause: fault.BeforeSeedHold,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				w.fixture.KeychainItem(w.service, withMembers(t, w.owned, seedFileMember))
				return ""
			},
			want: seedWant{outcome: seedDiscarded, path: "absent", note: []string{"Claude Code created the namespace item meanwhile"}, audit: "discarded", file: "same", parsed: 2},
			check: func(t *testing.T, w *seedWorld, _ *useSeedReport) {
				if got, _ := os.ReadFile(w.itemPath()); string(got) != withMembers(t, w.owned, seedFileMember) {
					t.Error("the item created meanwhile was changed")
				}
			},
		},
		"owned file changed before the hold: discarded": {
			pause: fault.BeforeSeedHold,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				changed := w.fixture.Blob("login-access", "login-refresh", testutil.FreshAt())
				w.write(t, w.file, changed)
				return changed
			},
			want: seedWant{outcome: seedDiscarded, path: "absent", note: []string{seedChangedNote}, audit: "discarded", file: "changed", parsed: 1},
		},
		"owned file gains a member before the hold: own_state at the held re-read": {
			pause: fault.BeforeSeedHold,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				changed := withMembers(t, w.owned, seedFileMember)
				w.write(t, w.file, changed)
				return changed
			},
			want: seedWant{outcome: seedOwnState, path: "absent", note: []string{"holds MCP state of its own (`mcpOAuth`)"}, audit: "own_state", file: "changed", parsed: 2},
		},
		"pending write parked before the hold: discarded": {
			pause: fault.BeforeSeedHold,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				w.write(t, filepath.Join(w.nsDir, secret.PendingFile), "pending")
				return ""
			},
			want: seedWant{outcome: seedDiscarded, path: "absent", note: []string{seedChangedNote}, audit: "discarded", file: "same", parsed: 1},
		},
		"interrupted before the hold: skipped with no audit entry": {
			pause: fault.BeforeSeedHold,
			act: func(_ *testing.T, _ *seedWorld, cancel context.CancelFunc) string {
				cancel()
				return ""
			},
			want: seedWant{outcome: seedSkipped, path: "absent", note: []string{seedInterruptedNote}, file: "same", parsed: 1},
		},
		"failing write: failed, the exit class only, the owned file kept": {
			setup: func(t *testing.T, _ *seedWorld) {
				t.Setenv("AGCTL_FAKE_SECURITY_WRITE_EXIT", "1")
				t.Setenv("AGCTL_FAKE_SECURITY_STDERR", "private stderr detail")
			},
			want: seedWant{outcome: seedFailed, path: "absent", ownedFile: "kept", note: []string{"the keychain write failed ("}, notNote: []string{"private stderr detail"}, writes: 1, audit: "failed", auditFile: "kept", file: "same", parsed: 2},
		},
		"member rewritten after the write: differs, the owned file kept without a warning": {
			pause: fault.SeedAfterWrite,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				written, err := os.ReadFile(w.itemPath())
				if err != nil {
					t.Fatal(err)
				}
				w.write(t, w.itemPath(), strings.Replace(string(written), "-beta", "-rotated", 1))
				return ""
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "differs", note: []string{"members mcpOAuth did not read back as written"}, notNote: []string{"reads this file until"}, writes: 1, audit: "applied", auditFile: "kept", file: "same", parsed: 3},
		},
		"readback cannot read the item: unverified, unknown, the owned file kept": {
			setup: func(t *testing.T, w *seedWorld) {
				t.Setenv("AGCTL_FAKE_SECURITY_FIND_EXIT_IF", w.fixture.Scratch("find-fails"))
			},
			pause: fault.SeedAfterWrite,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				w.write(t, w.fixture.Scratch("find-fails"), "")
				return ""
			},
			want: seedWant{outcome: seedUnknown, path: "absent", ownedFile: "kept", readback: "unverified", note: []string{"could not be confirmed"}, notNote: []string{"reads this file until"}, writes: 1, audit: "unknown", auditFile: "kept", file: "same", parsed: 2},
		},
		"another account written after the write: unknown, the owned file kept": {
			pause: fault.SeedAfterWrite,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				w.write(t, w.itemPath(), w.fixture.Blob("other-access", "other-refresh", testutil.FreshAt()))
				return ""
			},
			want: seedWant{outcome: seedUnknown, path: "absent", ownedFile: "kept", readback: "differs", note: []string{"could not be confirmed"}, writes: 1, audit: "unknown", auditFile: "kept", file: "same", parsed: 2},
		},
		"namespace guard busy at the removal: kept with the busy warning": {
			setup: func(t *testing.T, w *seedWorld) {
				guard, err := secret.Acquire(t.Context(), w.paths.LocksDir(), filepath.Base(w.paths.LockPath(testutil.Acct, testutil.Org)), time.Now().Add(time.Second))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = guard.Release() })
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "equal", note: []string{"was kept: its namespace is busy", seedKeptSentence}, writes: 1, audit: "applied", auditFile: "kept", file: "same", parsed: 3},
		},
		"storage-write lock taken after the write: kept with the busy warning": {
			pause: fault.SeedAfterWrite,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				if err := os.Mkdir(filepath.Join(w.nsDir, secret.StorageWriteLockName), 0o700); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "equal", note: []string{"was kept: its namespace is busy", seedKeptSentence}, writes: 1, audit: "applied", auditFile: "kept", file: "same", parsed: 3, leftovers: []string{secret.StorageWriteLockName}},
		},
		"owned file changed before the removal: kept with the changed warning": {
			pause: fault.SeedBeforeFileRemove,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				changed := w.fixture.Blob("login-access", "login-refresh", testutil.FreshAt())
				w.write(t, w.file, changed)
				return changed
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "equal", note: []string{"was kept: it changed after the seed", seedKeptSentence}, writes: 1, audit: "applied", auditFile: "kept", file: "changed", parsed: 3},
		},
		"plaintext fallback before the removal: the same account with a member kept, the item deleted": {
			pause: fault.SeedBeforeFileRemove,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				changed := withMembers(t, w.owned, seedFileMember)
				w.write(t, w.file, changed)
				if err := os.Remove(w.itemPath()); err != nil {
					t.Fatal(err)
				}
				return changed
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "equal", note: []string{"was kept: it changed after the seed", seedKeptSentence}, writes: 1, audit: "applied", auditFile: "kept", file: "changed", parsed: 4},
		},
		"unlink refused: kept with the could-not-be-removed warning": {
			pause: fault.SeedBeforeFileRemove,
			act: func(t *testing.T, w *seedWorld, _ context.CancelFunc) string {
				if err := os.Chmod(w.nsDir, 0o500); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "equal", note: []string{"could not be removed (", seedKeptSentence}, writes: 1, audit: "applied", auditFile: "kept", file: "same", parsed: 3, leftovers: []string{secret.RefreshLockName, secret.StorageWriteLockName}},
		},
		"interrupted before the removal's locks: kept with the interrupted warning": {
			record: func(r *seedRecorder, cancel context.CancelFunc) {
				r.cancelOn, r.cancelAt, r.cancel = "", 3, cancel
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "equal", note: []string{"was kept: the run was interrupted before it was removed", seedKeptSentence}, notNote: []string{"busy"}, writes: 1, audit: "applied", auditFile: "kept", file: "same", parsed: 3},
		},
		"interrupted between the final read and the unlink: kept with the interrupted warning": {
			pause: fault.SeedBeforeFileRemove,
			act: func(_ *testing.T, _ *seedWorld, cancel context.CancelFunc) string {
				cancel()
				return ""
			},
			want: seedWant{outcome: seedSeeded, path: "absent", ownedFile: "kept", readback: "equal", note: []string{"was kept: the run was interrupted before it was removed", seedKeptSentence}, notNote: []string{"busy"}, writes: 1, audit: "applied", auditFile: "kept", file: "same", parsed: 3},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := newSeedWorld(t)
			if tt.setup != nil {
				tt.setup(t, w)
			}
			t.Cleanup(func() { _ = os.Chmod(w.nsDir, 0o700) })
			itemBefore, _ := os.ReadFile(w.itemPath())
			tracked := trackParsed(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var reader secret.Reader
			if tt.record != nil {
				recorder := &seedRecorder{next: secret.NewReader()}
				tt.record(recorder, cancel)
				if recorder.cancelOn == "" {
					recorder.cancelOn = w.service
				}
				reader = recorder
			}
			var report *useSeedReport
			changed := ""
			if tt.pause != "" {
				report, _ = w.seedPaused(t, ctx, reader, tt.pause, func() {
					if tt.act != nil {
						changed = tt.act(t, w, cancel)
					}
				})
			} else {
				report = w.seed(ctx, reader)
			}
			if err := os.Chmod(w.nsDir, 0o700); err != nil {
				t.Fatal(err)
			}

			want := tt.want
			if report.Outcome != want.outcome {
				t.Fatalf("outcome = %s, want %s (note %v)", report.Outcome, want.outcome, report.Note)
			}
			if report.Target != w.service {
				t.Errorf("target = %s, want %s", report.Target, w.service)
			}
			for field, pair := range map[string][2]string{"path": {want.path, seedDeref(report.Path)}, "owned_file": {want.ownedFile, string(seedDeref(report.OwnedFile))}, "siblings_readback": {want.readback, string(seedDeref(report.SiblingsReadback))}} {
				if pair[0] != pair[1] {
					t.Errorf("%s = %q, want %q", field, pair[1], pair[0])
				}
			}
			if (report.Transport != nil) != (want.writes != 0) {
				t.Errorf("transport = %v with %d writes", report.Transport, want.writes)
			}
			note := seedDeref(report.Note)
			for _, part := range want.note {
				if !strings.Contains(note, part) {
					t.Errorf("note %q does not contain %q", note, part)
				}
			}
			for _, part := range append(want.notNote, siblingCanary, "sk-ant", "seed-access") {
				if strings.Contains(note, part) || strings.Contains(report.prose(), part) {
					t.Errorf("the report carries %q: %q", part, note)
				}
			}
			if strings.Contains(note, "reads this file until") && !strings.Contains(note, w.file) {
				t.Errorf("a kept-file warning does not name %s: %q", w.file, note)
			}
			if got := w.writes(); got != want.writes {
				t.Errorf("writes = %d, want %d: %v", got, want.writes, w.fixture.SecurityLog())
			}
			if want.writes == 0 && !bytes.Equal(itemBefore, mustReadOrNil(w.itemPath())) && tt.pause != fault.BeforeSeedHold {
				t.Error("a seed that issued no write changed the namespace item")
			}
			seeds := w.seedEntries(t)
			switch {
			case want.audit == "" && len(seeds) != 0:
				t.Errorf("audit entries %+v, want none", seeds)
			case want.audit != "" && len(seeds) != 1:
				t.Fatalf("audit entries %+v, want one %s", seeds, want.audit)
			case want.audit != "":
				if string(seeds[0].Outcome) != want.audit || string(seeds[0].OwnedFile) != want.auditFile || seeds[0].Target != secret.NamespaceTarget(w.record.Kind.Owned.ExportSHA8) {
					t.Errorf("audit = %+v, want outcome %s owned_file %q", seeds[0], want.audit, want.auditFile)
				}
			}
			switch data, err := os.ReadFile(w.file); want.file {
			case "gone":
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the owned file is still there: %v", err)
				}
			case "same":
				if string(data) != w.owned {
					t.Errorf("the owned file changed: %v", err)
				}
			case "changed":
				if changed == "" || string(data) != changed {
					t.Errorf("the owned file is not the one the action wrote: %v", err)
				}
			}
			if diff := gocmp.Diff(want.leftovers, w.lockLeftovers(t)); diff != "" {
				t.Errorf("lock artefacts left (-want +got):\n%s", diff)
			}
			for _, name := range []string{secret.AdoptedFile, secret.PendingMetaFile} {
				if _, err := os.Lstat(filepath.Join(w.nsDir, name)); err == nil {
					t.Errorf("the seed created %s", name)
				}
			}
			tracked.requireWiped(t, want.parsed)
			if tt.check != nil {
				tt.check(t, w, report)
			}
		})
	}
}

func seedDeref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func mustReadOrNil(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

func TestSeedDocumentEqualsTheCarryOfTheLiveSiblingsOntoTheHeldFileCredential(t *testing.T) {
	tests := map[string]struct {
		live string
	}{
		"success: seed document equals the carry of the live siblings onto the held file credential": {live: seedLiveMembers},
		"success: a third live member travels exactly as the carry copies it":                        {live: seedLiveMembers + `,"mcpOAuthClientConfig":{"alpha|0001":{"clientId":"client"}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := newSeedWorld(t)
			live := withMembers(t, w.fixture.IdentifiedBlob("live-access", "live-refresh", testutil.FreshAt(), "77777777-7777-4777-8777-777777777777", testutil.Org), tt.live)
			w.fixture.KeychainItem(claude.LiveService, live)
			report := w.seed(t.Context(), nil)
			if report.Outcome != seedSeeded {
				t.Fatalf("outcome = %s (%v)", report.Outcome, report.Note)
			}
			held, err := claude.ParseBlob([]byte(w.owned))
			if err != nil {
				t.Fatal(err)
			}
			source, err := claude.ParseBlob([]byte(live))
			if err != nil {
				t.Fatal(err)
			}
			held.CarrySiblingsFrom(source)
			want, err := held.KeychainBlobJSON()
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(w.itemPath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(want, got) {
				t.Errorf("the written document differs from the carry: %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

func TestSeedPreHoldDeadline(t *testing.T) {
	tests := map[string]struct {
		blockList bool
		blockLive bool
		calls     []string
	}{
		"error: a listing that never answers ends at the deadline":   {blockList: true, calls: []string{"preflight", "list"}},
		"error: a live read that never answers ends at the deadline": {blockLive: true, calls: []string{"preflight", "list", "read " + "%service%", "read " + claude.LiveService}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := newSeedWorld(t)
			recorder := &seedRecorder{next: secret.NewReader(), blockList: tt.blockList}
			if tt.blockLive {
				recorder.blockRead = claude.LiveService
			}
			started := time.Now()
			report := w.seed(t.Context(), recorder)
			elapsed := time.Since(started)
			if report.Outcome != seedSkipped || !strings.Contains(seedDeref(report.Note), "within the seeding deadline (3000 ms)") {
				t.Fatalf("report = %s %v", report.Outcome, report.Note)
			}
			if elapsed > SeedPreHoldBudget+250*time.Millisecond {
				t.Errorf("the seed took %v, past the %v deadline plus 250 ms", elapsed, SeedPreHoldBudget)
			}
			want := make([]string, 0, len(tt.calls))
			for _, call := range tt.calls {
				want = append(want, strings.ReplaceAll(call, "%service%", w.service))
			}
			if diff := gocmp.Diff(want, recorder.calls); diff != "" {
				t.Errorf("calls (-want +got):\n%s", diff)
			}
			for i, deadline := range recorder.deadlines {
				if deadline.IsZero() || !deadline.Equal(recorder.deadlines[0]) {
					t.Errorf("call %d (%s) has deadline %v, want the phase's %v", i, recorder.calls[i], deadline, recorder.deadlines[0])
				}
			}
			if limit := started.Add(SeedPreHoldBudget + 50*time.Millisecond); recorder.deadlines[0].After(limit) {
				t.Errorf("the phase deadline %v is later than %v", recorder.deadlines[0], limit)
			}
			if w.writes() != 0 || len(w.seedEntries(t)) != 0 {
				t.Error("a seed that ran out of time wrote or audited")
			}
		})
	}
}

// capClock is the system clock, except that a sleep the contention wait
// starts on the namespace's primary lock ends just before the acquisition's
// own deadline, and releases the lock as it ends: a holder that lets go at
// the cap.
type capClock struct {
	secret.Clock
	primary string
}

func (c capClock) Sleep(ctx context.Context, howLong time.Duration) error {
	if deadline, ok := ctx.Deadline(); ok {
		howLong = min(howLong, time.Until(deadline)-20*time.Millisecond)
	}
	if err := c.Clock.Sleep(ctx, howLong); err != nil {
		return err
	}
	_ = os.Remove(c.primary)
	return nil
}

func TestSeedLockCaps(t *testing.T) {
	tests := map[string]struct {
		// before makes the primary lock before the run; after makes it at
		// the pause after the write.
		before, after, released bool
		wantOutcome             seedOutcome
		wantOwned               string
		limit                   time.Duration
	}{
		"error: a held primary lock ends the wait for the hold at its cap":                {before: true, wantOutcome: seedSkipped, limit: SeedLockWait},
		"error: a primary lock taken after the write keeps the file at the removal's cap": {after: true, wantOutcome: seedSeeded, wantOwned: "kept", limit: SeedFileRemoveWait},
		"success: a hold returned as the lock wait ends is used and released":             {before: true, released: true, wantOutcome: seedSeeded, wantOwned: "removed", limit: SeedLockWait + secret.HoldBudget},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := newSeedWorld(t)
			primary := filepath.Join(w.nsDir, secret.RefreshLockName)
			if tt.before {
				if err := os.Mkdir(primary, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.released {
				w.clock = capClock{Clock: secret.SystemClock(), primary: primary}
			}
			point := fault.BeforeSeedHold
			if tt.after {
				point = fault.SeedAfterWrite
			}
			tracked := trackParsed(t)
			report, resumed := w.seedPaused(t, t.Context(), nil, point, func() {
				if tt.after {
					if err := os.Mkdir(primary, 0o700); err != nil {
						t.Error(err)
					}
				}
			})
			elapsed := time.Since(resumed)
			if report.Outcome != tt.wantOutcome || string(seedDeref(report.OwnedFile)) != tt.wantOwned {
				t.Fatalf("report = %s owned %v note %v", report.Outcome, report.OwnedFile, report.Note)
			}
			if !tt.released && !strings.Contains(seedDeref(report.Note), "busy") {
				t.Errorf("note %v does not say busy", report.Note)
			}
			if elapsed > tt.limit+250*time.Millisecond {
				t.Errorf("the seed took %v after the pause, past %v plus 250 ms", elapsed, tt.limit)
			}
			var want []string
			if !tt.released {
				want = []string{secret.RefreshLockName}
			}
			if diff := gocmp.Diff(want, w.lockLeftovers(t)); diff != "" {
				t.Errorf("lock artefacts left (-want +got):\n%s", diff)
			}
			tracked.requireWiped(t, 2)
		})
	}
}

func TestRunUseSeedFlagsSkipTheKeychain(t *testing.T) {
	tests := map[string]struct {
		opts cli.ClaudeUseOptions
	}{
		"success: --no-mcp-seed keeps the servers and makes no keychain call": {opts: cli.ClaudeUseOptions{ID: "account", JSON: true, NoMCPSeed: true}},
		"success: --no-mcp makes no keychain call":                            {opts: cli.ClaudeUseOptions{ID: "account", JSON: true, NoMCP: true}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := testutil.New(t)
			f.WriteRegistry([]any{f.OwnedRecord("account", "org")})
			if err := os.WriteFile(filepath.Join(f.Home(), ".claude.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.BinDir(), "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARG_LOG\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", f.Home())
			t.Setenv("PATH", f.BinDir())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
			t.Setenv("ARG_LOG", f.Scratch("argv"))
			opened := 0
			var out, stderr bytes.Buffer
			process := SessionProcess{Out: &out, Err: &stderr, Keychain: func() secret.Reader { opened++; return secret.DisabledReader{} }}
			if err := process.RunUse(t.Context(), cli.Globals{ConfigDir: f.ConfigDir()}, tt.opts); err != nil {
				t.Fatal(err)
			}
			if opened != 0 || stderr.Len() != 0 {
				t.Errorf("keychain opened %d times, stderr %q", opened, stderr.String())
			}
			var doc map[string]any
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if seed, ok := doc["mcp_seed"]; !ok || seed != nil {
				t.Errorf("mcp_seed = %v (present %v), want null", seed, ok)
			}
			argv, err := os.ReadFile(f.Scratch("argv"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(argv), "--mcp-config"); got != !tt.opts.NoMCP {
				t.Errorf("argv %q: --mcp-config present %v", argv, got)
			}
		})
	}
}

func TestRunUseSeedCancellationKeepsTheFileAndRefusesLaunch(t *testing.T) {
	tests := map[string]struct {
		json bool
	}{
		"error: cancellation after readback refuses the human session": {},
		"error: cancellation after readback refuses the JSON session":  {json: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w := newSeedWorld(t)
			f := w.fixture
			w.write(t, filepath.Join(f.Home(), ".claude.json"), "{}")
			if err := os.WriteFile(filepath.Join(f.BinDir(), "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARG_LOG\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", f.Home())
			t.Setenv("PATH", f.BinDir()+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
			t.Setenv("ARG_LOG", f.Scratch("argv"))
			before, err := os.ReadFile(w.file)
			if err != nil {
				t.Fatal(err)
			}
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, controller := signals.Install(parent)
			defer controller.Stop()
			reader := &seedRecorder{next: secret.NewReader(), cancelOn: w.service, cancelAt: 3, cancel: cancel}
			var out, stderr bytes.Buffer
			process := SessionProcess{Out: &out, Err: &stderr, Signals: controller, Keychain: func() secret.Reader { return reader }}
			err = process.RunUse(ctx, cli.Globals{ConfigDir: f.ConfigDir()}, cli.ClaudeUseOptions{ID: testutil.Acct, JSON: tt.json})
			refused, ok := errors.AsType[*errs.RefusedError](err)
			if !ok || refused.Reason != "cancelled before the command could start" {
				t.Fatalf("RunUse returned %v, want the pre-spawn cancellation refusal", err)
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("the readback did not cancel the run: %v", ctx.Err())
			}
			after, err := os.ReadFile(w.file)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(before, after); diff != "" {
				t.Error("the cancellation changed the owned credential file")
			}
			warning := "the owned credential file `" + w.file + "` was kept: the run was interrupted before it was removed; "
			if !strings.Contains(stderr.String(), warning) || !strings.Contains(stderr.String(), "reads this file until it is removed") {
				t.Errorf("stderr %q does not carry the interrupted-file warning", stderr.String())
			}
			if got := w.writes(); got != 1 {
				t.Errorf("keychain writes = %d, want 1", got)
			}
			seeds := w.seedEntries(t)
			if len(seeds) != 1 {
				t.Fatalf("seed audit entries = %d, want 1", len(seeds))
			}
			if diff := gocmp.Diff([]string{"applied", "kept", "equal"}, []string{string(seeds[0].Outcome), string(seeds[0].OwnedFile), string(seeds[0].SiblingsReadback)}); diff != "" {
				t.Errorf("seed audit outcome/file/readback (-want +got):\n%s", diff)
			}
			if _, err := os.Lstat(f.Scratch("argv")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the fake claude was launched: argv marker error %v", err)
			}
		})
	}
}

func TestSeedReportShapeAndProse(t *testing.T) {
	two := 2
	kept, removed := secret.OwnedFileKept, secret.OwnedFileRemoved
	tests := map[string]struct {
		report useSeedReport
		keys   []string
		prose  string
	}{
		"success: a skip names its note on one line": {
			report: useSeedReport{Target: "Claude Code-credentials-1a2b3c4d", Outcome: seedSkipped, SeededMembers: []useCarriedMember{}, Note: new(seedBusyNote)},
			prose:  "MCP credentials not seeded: " + seedBusyNote,
		},
		"success: a silent skip prints nothing": {
			report: useSeedReport{Target: "Claude Code-credentials-1a2b3c4d", Outcome: seedSkipped, SeededMembers: []useCarriedMember{}, Note: new("the keychain is locked; nothing seeded"), silent: true},
		},
		"success: a seed that removed the file says so last": {
			report: useSeedReport{Target: "Claude Code-credentials-1a2b3c4d", Outcome: seedSeeded, SeededMembers: []useCarriedMember{{Name: "mcpOAuth", Entries: &two}, {Name: "pluginSecrets"}}, OwnedFile: &removed},
			prose:  "seeded mcpOAuth (2 entries), pluginSecrets into Claude Code-credentials-1a2b3c4d; removed the owned credential file",
		},
		"success: a seed that kept the file ends with its warning": {
			report: useSeedReport{Target: "Claude Code-credentials-1a2b3c4d", Outcome: seedSeeded, SeededMembers: []useCarriedMember{{Name: "pluginSecrets"}}, OwnedFile: &kept, warning: "the owned credential file `/ns/.credentials.json` was kept: its namespace is busy"},
			prose:  "seeded pluginSecrets into Claude Code-credentials-1a2b3c4d; the owned credential file `/ns/.credentials.json` was kept: its namespace is busy",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.report.prose(); got != tt.prose {
				t.Errorf("prose = %q, want %q", got, tt.prose)
			}
			data, err := json.Marshal(&tt.report)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			want := []string{"note", "outcome", "owned_file", "path", "seeded_members", "siblings_readback", "target", "transport"}
			if diff := gocmp.Diff(want, slices.Sorted(maps.Keys(doc))); diff != "" {
				t.Errorf("mcp_seed keys (-want +got):\n%s", diff)
			}
		})
	}
}
