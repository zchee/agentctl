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

package commands

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/agentctl/fixtures"
	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/usage"
)

// ownedOnly narrows a pass to the fixture's owned account. The live
// row always exists, and under the disabled reader it is a degraded row
// of its own; these tests are about the owned one.
var ownedOnly = []string{testutil.Email}

// usageBody is the captured usage response every healthy fetch answers
// with.
func usageBody(t *testing.T) []byte {
	t.Helper()
	body, err := fixtures.FS.ReadFile("claude/usage-2026-09-08.json")
	if err != nil {
		t.Fatalf("read the captured usage body: %v", err)
	}
	return body
}

// statusWorld is one test's pass against a temporary store and a local
// server: the fixture, the command under test, its stdout, and the
// count of usage requests the server answered.
type statusWorld struct {
	fixture *testutil.Fixture
	status  *Status
	globals cli.Globals
	stdout  *bytes.Buffer
	calls   *atomic.Int64
}

// newStatusWorld builds the world around handler.
func newStatusWorld(t *testing.T, handler http.HandlerFunc) *statusWorld {
	t.Helper()
	fixture := testutil.New(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	env := claude.EnvWithHome(fixture.Home())
	stdout := &bytes.Buffer{}
	return &statusWorld{
		fixture: fixture,
		status: &Status{
			Reader: secret.DisabledReader{},
			Client: claude.NewUsageClient(server.URL, "agentctl-test", 5*time.Second),
			Env:    &env,
			Stdout: stdout,
			Zone:   time.UTC,
		},
		globals: cli.Globals{ConfigDir: fixture.ConfigDir()},
		stdout:  stdout,
		calls:   &calls,
	}
}

// run runs one pass with fresh output, returning what it printed.
func (w *statusWorld) run(t *testing.T, opts cli.ClaudeStatusOptions) (string, error) {
	t.Helper()
	w.stdout.Reset()
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	err := w.status.Run(t.Context(), w.globals, opts)
	return w.stdout.String(), err
}

// seedOwned writes the registry and one owned account's credential file.
func (w *statusWorld) seedOwned(t *testing.T, blob string) {
	t.Helper()
	w.fixture.WriteRegistry([]any{w.fixture.OwnedRecord(testutil.Acct, testutil.Org)})
	w.fixture.WriteCredentials(testutil.Acct, testutil.Org, blob)
}

// serveBody answers every request with one body.
func serveBody(status int, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// assertPartial fails unless err is the partial error counting failed
// shown rows.
func assertPartial(t *testing.T, err error, failed int) {
	t.Helper()
	partialErr, ok := errors.AsType[*errs.PartialError](err)
	if !ok {
		t.Fatalf("err = %v, want a partial error", err)
	}
	if partialErr.Failed != failed {
		t.Fatalf("failed = %d, want %d", partialErr.Failed, failed)
	}
	if code := errs.ExitCode(err); code != errs.ExitPartial {
		t.Fatalf("exit = %d, want %d", code, errs.ExitPartial)
	}
}

func TestStatusFetchesAndRendersAFreshOwnedAccount(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))

	stdout, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stdout)
	}
	for _, want := range []string{"Fable (weekly)", testutil.Email, "max", "ok"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Error("the printed table ends with one newline")
	}
	if got := world.calls.Load(); got != 1 {
		t.Errorf("usage calls = %d, want 1", got)
	}

	// A second pass inside the TTL answers from the cache and makes no
	// request at all.
	if _, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := world.calls.Load(); got != 1 {
		t.Errorf("usage calls after the cached pass = %d, want still 1", got)
	}
}

func TestStatusReturnsOutputErrors(t *testing.T) {
	tests := map[string]struct {
		json bool
		raw  bool
	}{
		"error: table output fails":               {},
		"error: JSON output fails":                {json: true},
		"error: raw output fails after the table": {raw: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))
			world.status.Now = func() time.Time { return time.UnixMilli(testutil.FreshAt() - 600_000) }
			table, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly})
			if err != nil {
				t.Fatalf("render the table: %v", err)
			}
			reader, writer := io.Pipe()
			defer func() { _ = writer.Close() }()
			if tt.raw {
				finished := make(chan error, 1)
				go func() {
					_, err := io.CopyN(io.Discard, reader, int64(len(table)))
					_ = reader.Close()
					finished <- err
				}()
				t.Cleanup(func() {
					if err := <-finished; err != nil {
						t.Errorf("read table before closing stdout: %v", err)
					}
				})
			} else if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			world.status.Stdout = writer
			err = world.status.Run(t.Context(), world.globals, cli.ClaudeStatusOptions{JSON: tt.json, Raw: tt.raw, Accounts: ownedOnly})
			if !errors.Is(err, io.ErrClosedPipe) || errs.ExitCode(err) != errs.ExitFatal {
				t.Fatalf("Run error = %v, exit = %d; want closed pipe and fatal exit", err, errs.ExitCode(err))
			}
		})
	}
}

func TestStatusHonoursARateLimitPastTheEndOfTheProcess(t *testing.T) {
	body := usageBody(t)
	var limited atomic.Bool
	world := newStatusWorld(t, func(w http.ResponseWriter, _ *http.Request) {
		if limited.Load() {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("{}"))
			return
		}
		_, _ = w.Write(body)
	})
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))

	// First run fills the cache.
	if _, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	limited.Store(true)

	// Second run meets the rate limit and shows the cached figures
	// beside it.
	stdout, err := world.run(t, cli.ClaudeStatusOptions{NoCache: true, Accounts: ownedOnly})
	assertPartial(t, err, 1)
	for _, want := range []string{"rate-limited (retry in 30s)", "showing the cached value", "21"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	if got := world.calls.Load(); got != 2 {
		t.Fatalf("usage calls = %d, want 2", got)
	}

	// Third run, inside the window, declines to call at all — even
	// though the no-cache flag asks for a fresh fetch.
	stdout, err = world.run(t, cli.ClaudeStatusOptions{NoCache: true, Accounts: ownedOnly})
	assertPartial(t, err, 1)
	if !strings.Contains(stdout, "rate-limited") {
		t.Errorf("stdout is missing the rate-limited state:\n%s", stdout)
	}
	if got := world.calls.Load(); got != 2 {
		t.Errorf("usage calls = %d, want still 2: the window survived the second pass", got)
	}
}

func TestStatusReportsWhyAnExpiredOwnedCredentialWasLeftAlone(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("access-stale", "refresh-stale", testutil.ExpiredAt()))

	stdout, err := world.run(t, cli.ClaudeStatusOptions{Refresh: true, Accounts: ownedOnly})
	assertPartial(t, err, 1)
	if !strings.Contains(stdout, "expired ("+refreshUnavailableNote+")") {
		t.Errorf("stdout is missing the expired state and its reason:\n%s", stdout)
	}
	if got := world.calls.Load(); got != 0 {
		t.Errorf("usage calls = %d, want none for a row whose token is known dead", got)
	}
}

func TestStatusMapsFetchFailuresOntoRowStates(t *testing.T) {
	tests := map[string]struct {
		fill      bool
		status    int
		body      string
		wantState string
		wantCalls int64
	}{
		"error: a rejected token on an owned row reports the refresh gap": {
			status:    http.StatusUnauthorized,
			body:      "{}",
			wantState: "expired (" + refreshUnavailableNote + ")",
			wantCalls: 1,
		},
		"success: a transient failure shows the cached value as stale": {
			fill:      true,
			status:    http.StatusInternalServerError,
			body:      "{}",
			wantState: "stale",
			wantCalls: 2,
		},
		"error: a permanent failure names the status instead of waiting": {
			status:    http.StatusNotFound,
			body:      "{}",
			wantState: "HTTP 404",
			wantCalls: 1,
		},
		"error: a response with no windows is its own degraded state": {
			status:    http.StatusOK,
			body:      "{}",
			wantState: "no subscription limits (API/console account?)",
			wantCalls: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			healthy := usageBody(t)
			var degrade atomic.Bool
			world := newStatusWorld(t, func(w http.ResponseWriter, _ *http.Request) {
				if degrade.Load() {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
					return
				}
				_, _ = w.Write(healthy)
			})
			world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))
			if tt.fill {
				if _, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly}); err != nil {
					t.Fatalf("fill the cache: %v", err)
				}
			}
			degrade.Store(true)

			stdout, err := world.run(t, cli.ClaudeStatusOptions{NoCache: true, Accounts: ownedOnly})
			// Every case degrades the shown row: a windowless answer is
			// "you asked for numbers and there are none" too.
			assertPartial(t, err, 1)
			if !strings.Contains(stdout, tt.wantState) {
				t.Errorf("stdout is missing %q:\n%s", tt.wantState, stdout)
			}
			if got := world.calls.Load(); got != tt.wantCalls {
				t.Errorf("usage calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestStatusJSONDocumentValidatesAndCarriesRawBodies(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))

	stdout, err := world.run(t, cli.ClaudeStatusOptions{JSON: true, Accounts: ownedOnly})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stdout)
	}
	testutil.ValidateSchema(t, "status.v1.json", []byte(stdout))
	if strings.Contains(stdout, `"raw"`) {
		t.Errorf("the raw member needs its flag:\n%s", stdout)
	}

	stdout, err = world.run(t, cli.ClaudeStatusOptions{JSON: true, Raw: true, Accounts: ownedOnly})
	if err != nil {
		t.Fatalf("run with raw: %v\n%s", err, stdout)
	}
	testutil.ValidateSchema(t, "status.v1.json", []byte(stdout))
	if !strings.Contains(stdout, `"raw"`) || !strings.Contains(stdout, `"`+testutil.Acct+`"`) {
		t.Errorf("the raw member is keyed by row id:\n%s", stdout)
	}
}

func TestStatusRawPrintsTheBodyAfterTheTable(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))

	stdout, err := world.run(t, cli.ClaudeStatusOptions{Raw: true, Accounts: ownedOnly})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "--- raw: "+testutil.Email+" ---") {
		t.Errorf("stdout is missing the raw banner:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"five_hour"`) {
		t.Errorf("stdout is missing the raw body:\n%s", stdout)
	}
}

func TestStatusRefusesASelectorThatMatchesNothing(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))

	stdout, err := world.run(t, cli.ClaudeStatusOptions{Accounts: []string{"nobody@example.com"}})
	if _, ok := errors.AsType[*errs.ConfigError](err); !ok {
		t.Fatalf("err = %v, want a config error", err)
	}
	if code := errs.ExitCode(err); code != errs.ExitFatal {
		t.Fatalf("exit = %d, want %d", code, errs.ExitFatal)
	}
	if stdout != "" {
		t.Errorf("a refused selector renders nothing:\n%s", stdout)
	}
}

func TestStatusHiddenRowsNeverDriveTheExitStatus(t *testing.T) {
	// A forgotten record is hidden and healthy; the owned row succeeds.
	// The footer counts the hidden row without the show-everything flag
	// and the exit stays clean.
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	forgotten := world.fixture.OwnedRecord("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "ffffffff-0000-4111-8222-333333333333")
	forgotten["forgotten"] = true
	world.fixture.WriteRegistry([]any{world.fixture.OwnedRecord(testutil.Acct, testutil.Org), forgotten})
	world.fixture.WriteCredentials(testutil.Acct, testutil.Org, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))

	stdout, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "1 entry hidden (--all)") {
		t.Errorf("stdout is missing the hidden footer:\n%s", stdout)
	}

	stdout, err = world.run(t, cli.ClaudeStatusOptions{All: true, Accounts: ownedOnly})
	if err != nil {
		t.Fatalf("run with --all: %v\n%s", err, stdout)
	}
	if strings.Contains(stdout, "hidden (--all)") {
		t.Errorf("the footer has nothing to count under --all:\n%s", stdout)
	}
	if !strings.Contains(stdout, "forgotten") {
		t.Errorf("--all shows the forgotten row:\n%s", stdout)
	}
}

func TestMarkSameIdentityAndFolding(t *testing.T) {
	liveRecord := config.AccountRecord{AccountUUID: testutil.Acct, OrganizationUUID: testutil.Org, Kind: config.AccountKindLive()}
	ownedRecord := config.AccountRecord{AccountUUID: testutil.Acct, OrganizationUUID: testutil.Org, Kind: config.AccountKindOwned("/store", "aaaaaaaa")}
	strangerRecord := config.AccountRecord{AccountUUID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", OrganizationUUID: testutil.Org, Kind: config.AccountKindOwned("/store2", "bbbbbbbb")}

	outcomes := []rowOutcome{
		{index: 0, record: liveRecord, state: claude.StateOfOK(), visibleByDefault: true},
		{index: 1, record: ownedRecord, state: claude.StateOfOK(), visibleByDefault: true},
		{index: 2, record: strangerRecord, state: claude.StateOfOK(), visibleByDefault: true},
	}
	markSameIdentity(outcomes)
	if outcomes[0].sameIdentityAsLive {
		t.Error("a live row is not its own twin")
	}
	if !outcomes[1].sameIdentityAsLive {
		t.Error("the owned twin is marked")
	}
	if outcomes[2].sameIdentityAsLive {
		t.Error("a stranger is not marked")
	}

	plain := statusRows(outcomes, false)
	if len(plain) != 3 {
		t.Fatalf("plain rows = %d, want 3", len(plain))
	}
	if !plain[1].SameIdentityAsLive {
		t.Error("the plain view keeps the note")
	}

	folded := statusRows(outcomes, true)
	if len(folded) != 2 {
		t.Fatalf("folded rows = %d, want 2: the healthy live row folds away", len(folded))
	}
	if folded[0].Kind != render.LiveAndOwnedKind {
		t.Errorf("kind = %q, want %q", folded[0].Kind, render.LiveAndOwnedKind)
	}
	if folded[0].SameIdentityAsLive {
		t.Error("the folded row drops the note: the Kind cell already says it")
	}

	// A live row in a failing state is never folded away: the exit
	// status is computed from the outcomes, and hiding a failing row
	// would leave the run exiting partial with nothing on screen to
	// explain it.
	outcomes[0].state = claude.StateOfKeychainLocked("")
	failing := statusRows(outcomes, true)
	if len(failing) != 3 {
		t.Fatalf("rows = %d, want 3: a failing live row survives the fold", len(failing))
	}
}

func TestStatusRateLimitWindowUsesTheCachedBody(t *testing.T) {
	// The persisted window keeps the earlier body, so the next run
	// inside the window still renders numbers. This drives the cache
	// entry pair directly to pin the persistence shape.
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt()))
	if _, err := world.run(t, cli.ClaudeStatusOptions{Accounts: ownedOnly}); err != nil {
		t.Fatalf("fill the cache: %v", err)
	}

	paths := config.NewPaths(world.fixture.ConfigDir())
	cachePath := usage.CachePath(paths, testutil.Acct, testutil.Org)
	entry := usage.LoadCache(t.Context(), cachePath)
	if entry == nil {
		t.Fatal("the first pass wrote a cache entry")
	}
	if entry.RateLimitedUntilMs != nil {
		t.Fatal("a healthy pass records no rate-limit window")
	}
}
