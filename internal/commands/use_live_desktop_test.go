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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
)

const (
	useDesktopOwner    = "11111111-2222-3333-4444-555555555555"
	useDesktopIncoming = "44444444-4444-4444-8444-444444444444"
	useDesktopThird    = "55555555-5555-4555-8555-555555555555"
	useDesktopSwapID   = "2026-10-13T07:00:00Z#4242"
)

// useDesktopProfile creates a profile directory whose config.json carries
// body, or no config.json when body is empty.
func useDesktopProfile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func useDesktopConfig(uuid string) string {
	return `{"lastKnownAccountUuid":"` + uuid + `","darkMode":"system"}`
}

// TestDesktopUndoGuard pins which switch the undo of a swap reverses: only
// the newest result recorded for that swap, only when it applied, and only
// while the data path still holds the directory it installed under one of
// the three identities the switch can account for.
func TestDesktopUndoGuard(t *testing.T) {
	type layout struct {
		// config is the data path's config.json; "-" leaves the data
		// path absent, "" leaves it without config.json.
		config string
	}
	applied := func(observed *desktop.IdentityRecord) desktop.Result {
		return desktop.Result{Type: desktop.TypeResult, OpID: "00000000000000aa", Command: useDesktopForward, RequestedAccount: useDesktopIncoming, SwapAuditID: useDesktopSwapID, Outcome: desktop.OutcomeApplied, Word: wordSelected, Observed: observed}
	}
	tests := map[string]struct {
		layout  layout
		results func(installed desktop.FileID) []desktop.Result
		// want is "" for allowed, "nothing" for nothing to undo, or the
		// refusal word.
		want       string
		wantDetail string
		// outgoing is the name the switch's intent stored the profile
		// leaving the data path under, or empty for no intent row.
		outgoing     string
		wantLeftover string
		// wantBefore, when set, is the identity the guard must report as
		// read at the data path.
		wantBefore *desktop.IdentityRecord
	}{
		"success: an outgoing profile stored under a drawn name is named as left behind": {
			layout:   layout{config: useDesktopConfig(useDesktopIncoming)},
			outgoing: "unknown-00000000000000aa",
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &installed
				return []desktop.Result{r}
			},
			wantLeftover: "unknown-00000000000000aa",
		},
		"success: an outgoing profile stored under its account is brought back, nothing left behind": {
			layout:   layout{config: useDesktopConfig(useDesktopIncoming)},
			outgoing: useDesktopOwner,
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &installed
				return []desktop.Result{r}
			},
		},
		"error: a refused undo names nothing as left behind": {
			layout:   layout{config: useDesktopConfig(useDesktopThird)},
			outgoing: "fresh-00000000000000aa",
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &installed
				return []desktop.Result{r}
			},
			want: desktopProfileMismatch,
		},
		"success: the profile still reads as the switch observed it": {
			layout: layout{config: ""},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &installed
				return []desktop.Result{r}
			},
		},
		"success: the first sign-in finished since, as the account switched to": {
			layout: layout{config: useDesktopConfig(useDesktopIncoming)},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &installed
				return []desktop.Result{r}
			},
		},
		"success: a stored profile observed as its account and unchanged": {
			layout: layout{config: useDesktopConfig(useDesktopIncoming)},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopIncoming})
				r.Installed = &installed
				return []desktop.Result{r}
			},
		},
		"success: a profile nobody signed in to, whatever was observed": {
			layout: layout{config: ""},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateUnknown})
				r.Installed = &installed
				return []desktop.Result{r}
			},
		},
		"success: a later result for the same swap, written by repair, decides": {
			layout: layout{config: ""},
			results: func(installed desktop.FileID) []desktop.Result {
				first := applied(nil)
				first.Outcome, first.Word = desktop.OutcomeUnknown, wordJournalNotRecorded
				repaired := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				repaired.Word, repaired.Installed = wordRepaired, &installed
				return []desktop.Result{first, repaired}
			},
		},
		"error: no switch was recorded for this swap": {
			layout: layout{config: ""},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed, r.SwapAuditID = &installed, "2026-10-12T07:00:00Z#1"
				return []desktop.Result{r}
			},
			want:       "nothing",
			wantDetail: "no desktop switch was recorded for the swap being undone",
		},
		"error: a switch for the swap but not for this one is never matched by a missing id": {
			layout: layout{config: ""},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed, r.SwapAuditID = &installed, ""
				return []desktop.Result{r}
			},
			want:       "nothing",
			wantDetail: "no desktop switch was recorded",
		},
		"error: a reverted switch has nothing to put back": {
			layout: layout{config: useDesktopConfig(useDesktopOwner)},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopOwner})
				r.Outcome, r.Word, r.Installed = desktop.OutcomeReverted, wordRevertedRelaunched, &installed
				return []desktop.Result{r}
			},
			want:       "nothing",
			wantDetail: "ended reverted_relaunched, so there is nothing to put back",
		},
		"error: an already-selected switch has nothing to put back": {
			layout: layout{config: useDesktopConfig(useDesktopIncoming)},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopIncoming})
				r.Outcome, r.Word, r.Installed = desktop.OutcomeNoop, wordAlreadySelected, &installed
				return []desktop.Result{r}
			},
			want:       "nothing",
			wantDetail: "ended already_selected",
		},
		"error: an aborted switch has nothing to put back": {
			layout: layout{config: useDesktopConfig(useDesktopOwner)},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(nil)
				r.Outcome, r.Word = desktop.OutcomeAborted, desktopQuitTimeout
				return []desktop.Result{r}
			},
			want:       "nothing",
			wantDetail: "ended desktop_quit_timeout",
		},
		"error: an applied switch that found no profile when it finished": {
			layout: layout{config: ""},
			results: func(desktop.FileID) []desktop.Result {
				return []desktop.Result{applied(nil)}
			},
			want:       desktopProfileMismatch,
			wantDetail: "`agentctl claude desktop use " + useDesktopOwner + "` switches the app back",
		},
		"error: another directory at the data path, signed in to the same account": {
			layout: layout{config: useDesktopConfig(useDesktopIncoming)},
			results: func(installed desktop.FileID) []desktop.Result {
				other := installed
				other.Ino++
				r := applied(&desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopIncoming})
				r.Installed = &other
				return []desktop.Result{r}
			},
			want:       desktopProfileMismatch,
			wantDetail: "is not the one the desktop switch for the swap being undone installed",
		},
		"error: no profile at the data path at all": {
			layout: layout{config: "-"},
			results: func(desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &desktop.FileID{Dev: 1, Ino: 2}
				return []desktop.Result{r}
			},
			want:       desktopProfileMismatch,
			wantDetail: "the data path now holds no profile that can be read",
		},
		"error: a third account signed in since the switch": {
			layout: layout{config: useDesktopConfig(useDesktopThird)},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &installed
				return []desktop.Result{r}
			},
			want:       desktopProfileMismatch,
			wantDetail: "now reads as account " + useDesktopThird,
			wantBefore: &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopThird},
		},
		"error: a profile that cannot be read is no identity the switch accounts for": {
			layout: layout{config: `["not", "an", "object"]`},
			results: func(installed desktop.FileID) []desktop.Result {
				r := applied(&desktop.IdentityRecord{State: desktop.StateFresh})
				r.Installed = &installed
				return []desktop.Result{r}
			},
			want:       desktopProfileMismatch,
			wantDetail: "now reads as unreadable",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "Claude")
			var installed desktop.FileID
			if tt.layout.config != "-" {
				useDesktopProfile(t, data, tt.layout.config)
				id, err := desktop.StatID(data)
				if err != nil {
					t.Fatal(err)
				}
				installed = id
			}
			var entries []desktop.Entry
			if tt.outgoing != "" {
				entries = append(entries, desktop.Entry{Line: 1, Intent: &desktop.Intent{Type: desktop.TypeIntent, OpID: "00000000000000aa", Command: useDesktopForward, RequestedAccount: useDesktopIncoming, SwapAuditID: useDesktopSwapID, Outgoing: &desktop.Outgoing{Name: tt.outgoing}}})
			}
			for i, res := range tt.results(installed) {
				entries = append(entries, desktop.Entry{Line: i + 1, Result: &res})
			}
			var leftover string
			var before *desktop.IdentityRecord
			err := desktopUndoGuard(useDesktopSwapID, useDesktopOwner, data, &before, &leftover)(entries)
			got := ""
			var nothing *nothingToUndo
			var refusal *desktopRefusal
			switch {
			case err == nil:
			case errors.As(err, &nothing):
				got = "nothing"
			case errors.As(err, &refusal):
				got = refusal.Word
			default:
				t.Fatalf("unexpected error type %T: %v", err, err)
			}
			t.Logf("guard: %v", err)
			if got != tt.want {
				t.Fatalf("guard = %q, want %q (error: %v)", got, tt.want, err)
			}
			if tt.wantDetail != "" && !strings.Contains(err.Error(), tt.wantDetail) {
				t.Errorf("guard error %q does not say %q", err.Error(), tt.wantDetail)
			}
			if leftover != tt.wantLeftover {
				t.Errorf("leftover = %q, want %q", leftover, tt.wantLeftover)
			}
			// A guard that found the switch to undo reports what it read at
			// the data path, whether it then allows the undo or refuses it;
			// one that found nothing to undo looked at nothing.
			var wantBefore *desktop.IdentityRecord
			if tt.layout.config != "-" && got != "nothing" {
				wantBefore = new(desktop.RecordOf(desktop.ReadDesktopIdentity(data)))
			}
			if tt.wantBefore != nil {
				wantBefore = tt.wantBefore
			}
			if diff := gocmp.Diff(wantBefore, before); diff != "" {
				t.Errorf("before (-want +got):\n%s", diff)
			}
		})
	}
}

// useDesktopSystem is the unit tests' system with a fixed process listing,
// or an injected failure from the system itself.
func useDesktopSystem(t *testing.T, root string, list rdesktop.Lister, sysErr error) func(string) (*rdesktop.System, error) {
	t.Helper()
	base := desktopTestSystem(t, root)
	return func(dataDir string) (*rdesktop.System, error) {
		if sysErr != nil {
			return nil, sysErr
		}
		sys, err := base(dataDir)
		if err != nil {
			return nil, err
		}
		if list != nil {
			sys.Observer.List = list
		}
		return sys, nil
	}
}

// TestDesktopConsentClause pins the part of the swap's question that names
// what the desktop switch will close and move, and proves the look it is
// composed from creates nothing: the desktop store must not exist after it.
func TestDesktopConsentClause(t *testing.T) {
	start := time.Date(2026, 10, 13, 7, 0, 0, 0, time.UTC)
	uid := uint32(os.Getuid())
	running := func(context.Context, uint32) ([]rdesktop.Record, error) {
		return []rdesktop.Record{
			{PID: 500, PPID: 1, UID: uid, Start: start, Path: rdesktop.MainPath},
			{PID: 501, PPID: 500, UID: uid, Start: start, Path: "/Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper"},
		}, nil
	}
	tail := "; the switch looks at the app again before it moves anything, and a switch that does not finish leaves the swap as it is"
	tests := map[string]struct {
		data     string // config.json at the data path; "-" for no data path
		stored   bool   // the account's profile is stored
		list     rdesktop.Lister
		sysErr   error
		account  string
		undo     bool
		want     string
		wantPart string
		wantKind desktopExpectKind
	}{
		"success: an account profile leaves and a new one is created": {
			data: useDesktopConfig(useDesktopOwner), list: running, account: useDesktopIncoming,
			want:     ". Then quit the Claude desktop app (2 running process(es) close), the profile at the data path (account " + useDesktopOwner + ") moves to " + useDesktopOwner + ", a new profile is created at the next start (sign in once), and start the app for account " + useDesktopIncoming + tail,
			wantKind: expectSwitch,
		},
		"success: the stored profile moves in for an undo": {
			data: useDesktopConfig(useDesktopIncoming), stored: true, account: useDesktopOwner, undo: true,
			want:     ". Then quit the Claude desktop app (0 running process(es) close), the profile at the data path (account " + useDesktopIncoming + ") moves to " + useDesktopIncoming + ", the stored profile " + useDesktopOwner + " moves in, and start the app for account " + useDesktopOwner + ", if the desktop switch recorded for the swap being undone is still in place" + tail,
			wantKind: expectSwitch,
		},
		"success: a profile nobody signed in to gets a name drawn by the switch": {
			data: "", account: useDesktopIncoming,
			wantPart: "the profile at the data path (fresh) moves to fresh-<operation id>",
			wantKind: expectSwitch,
		},
		"success: no data path yet": {
			data: "-", account: useDesktopIncoming,
			wantPart: "no profile is at the data path, a new profile is created",
			wantKind: expectSwitch,
		},
		"success: the account already in place": {
			data: useDesktopConfig(useDesktopIncoming), account: useDesktopIncoming,
			want:     ". The desktop app already runs on that account's profile, so the desktop switch quits and moves nothing",
			wantKind: expectNoop,
		},
		"error: another platform": {
			data: "-", sysErr: rdesktop.ErrUnsupportedPlatform, account: useDesktopIncoming,
			want:     ". The desktop app switch will be refused (desktop_unsupported_platform: the desktop app switch is supported only on macOS); the swap is not affected",
			wantKind: expectRefused,
		},
		"error: the processes cannot be listed": {
			data: "-", account: useDesktopIncoming,
			list:     func(context.Context, uint32) ([]rdesktop.Record, error) { return nil, errors.New("EIO") },
			wantPart: ". The desktop app switch will be refused (desktop_observe_failed: could not observe the app's processes: ",
			wantKind: expectRefused,
		},
		"error: the profile at the data path cannot be read": {
			data: "[1]", account: useDesktopIncoming,
			wantPart: ". The desktop app switch will be refused (the profile at the data path cannot be read: ",
			wantKind: expectRefused,
		},
		"error: an account without a UUID the app could record": {
			data: "-", account: "not-a-uuid",
			want:     ". The desktop app switch will be refused (the account has no account UUID the desktop app could record); the swap is not affected",
			wantKind: expectRefused,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "home")
			t.Setenv("HOME", home)
			layout := desktop.Paths(home)
			if tt.data != "-" {
				useDesktopProfile(t, layout.DefaultData, tt.data)
			}
			if tt.stored {
				useDesktopProfile(t, layout.Profile(tt.account), useDesktopConfig(tt.account))
			}
			p := SessionProcess{Desktop: DesktopProcess{System: useDesktopSystem(t, root, tt.list, tt.sysErr), LockWait: time.Second}}
			got, expect := p.desktopConsentClause(t.Context(), tt.account, tt.undo)
			t.Logf("clause: %s; expectation: %+v", got, expect)
			if expect == nil || expect.kind != tt.wantKind {
				t.Errorf("expectation = %+v, want kind %d", expect, tt.wantKind)
			}
			if tt.want != "" {
				if diff := gocmp.Diff(tt.want, got); diff != "" {
					t.Errorf("clause (-want +got):\n%s", diff)
				}
			}
			if tt.wantPart != "" && !strings.Contains(got, tt.wantPart) {
				t.Errorf("clause %q does not contain %q", got, tt.wantPart)
			}
			if !tt.stored {
				if _, err := os.Lstat(layout.Root); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the clause's look created the desktop store %s (err %v)", layout.Root, err)
				}
			}
		})
	}
}

// TestDesktopUseQuestionUnchanged pins the question `desktop use` asks, so
// sharing its words with the swap's question changes nothing it says.
func TestDesktopUseQuestionUnchanged(t *testing.T) {
	snap := rdesktop.Snapshot{Mains: []rdesktop.Proc{{PID: 500}}, Family: []rdesktop.FamilyProc{{}}}
	in := &desktop.Intent{
		RequestedAccount: useDesktopIncoming,
		Outgoing:         &desktop.Outgoing{Name: useDesktopOwner, Identity: desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopOwner}},
		Target:           desktop.Target{Name: useDesktopIncoming, Existed: true},
	}
	want := "Quit the Claude desktop app (2 running process(es) close), the profile at the data path (account " + useDesktopOwner + ") moves to " + useDesktopOwner + ", the stored profile " + useDesktopIncoming + " moves in, and start the app for account " + useDesktopIncoming + "?"
	if diff := gocmp.Diff(want, consentQuestion(snap, in)); diff != "" {
		t.Errorf("question (-want +got):\n%s", diff)
	}
}

// TestFollowUpDesktopMovesNothingWhenItMustNotRun covers every way the
// follow-up ends without a switch: no flag, a swap that did not apply, and
// a context cancelled before the desktop step. None of them may write the
// journal, create the store or touch the profile at the data path.
func TestFollowUpDesktopMovesNothingWhenItMustNotRun(t *testing.T) {
	tests := map[string]struct {
		flag        bool
		outcome     claude.SwapOutcomeKind
		cancel      bool
		wantObject  bool
		wantDetail  string
		wantWarning string
	}{
		"success: no flag, no object": {
			outcome: claude.SwapApplied,
		},
		"success: a swap that did not apply leaves the app alone without a warning": {
			flag: true, outcome: claude.SwapCancelled, wantObject: true,
			wantDetail: "the swap did not apply, so the desktop app was left alone",
		},
		"error: a context cancelled before the desktop step records nothing and moves nothing": {
			flag: true, outcome: claude.SwapApplied, cancel: true, wantObject: true,
			wantDetail:  "the swap was interrupted before the desktop step, so nothing was quit or moved",
			wantWarning: "the desktop app was not switched: the swap was interrupted before the desktop step, so nothing was quit or moved",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "home")
			t.Setenv("HOME", home)
			layout := desktop.Paths(home)
			useDesktopProfile(t, layout.DefaultData, useDesktopConfig(useDesktopOwner))
			before, err := desktop.StatID(layout.DefaultData)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var stderr bytes.Buffer
			p := SessionProcess{Err: &stderr, Desktop: DesktopProcess{System: useDesktopSystem(t, root, nil, nil), LockWait: time.Second}}
			report := &useReport{outcome: claude.SwapOutcome{Kind: tt.outcome}, auditID: new(useDesktopSwapID)}
			p.followUpDesktop(ctx, cli.ClaudeUseOptions{Desktop: tt.flag}, report, useDesktopFollow{account: useDesktopIncoming, command: useDesktopForward})

			if got := report.desktop != nil; got != tt.wantObject {
				t.Fatalf("desktop object present = %v, want %v", got, tt.wantObject)
			}
			if tt.wantObject {
				if report.desktop.Outcome != nil {
					t.Errorf("outcome = %q, want null", *report.desktop.Outcome)
				}
				if report.desktop.Detail != tt.wantDetail {
					t.Errorf("detail = %q, want %q", report.desktop.Detail, tt.wantDetail)
				}
			}
			var wantWarnings []string
			if tt.wantWarning != "" {
				wantWarnings = []string{tt.wantWarning}
			}
			if diff := gocmp.Diff(wantWarnings, report.warnings); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
			if tt.wantWarning != "" && stderr.String() != "warning: "+tt.wantWarning+"\n" {
				t.Errorf("stderr = %q", stderr.String())
			}
			if _, err := os.Lstat(layout.Root); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the desktop store was created: %v", err)
			}
			after, err := desktop.StatID(layout.DefaultData)
			if err != nil || after != before {
				t.Errorf("the data path changed: before %s, after %s (%v)", before, after, err)
			}
		})
	}
}

// TestNothingToUndoKeepsTheObservedIdentity covers an undo whose consent
// guard read the profile at the data path and whose undo guard then found
// no switch to reverse: the object still reports what was read as before,
// with a null outcome and no after.
func TestNothingToUndoKeepsTheObservedIdentity(t *testing.T) {
	tests := map[string]struct {
		before *desktop.IdentityRecord
	}{
		"success: the owner's profile was read before nothing was found": {
			before: &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopOwner},
		},
		"success: nothing was read": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := &desktopEnv{before: tt.before}
			rep := &desktopReport{Version: 1, Command: "use", Account: useDesktopIncoming}
			got := useDesktopOf(rep, env, &nothingToUndo{reason: "no desktop switch was recorded for the swap being undone, so nothing was quit or moved"})
			want := &useDesktop{Before: tt.before, Detail: "no desktop switch was recorded for the swap being undone, so nothing was quit or moved"}
			if diff := gocmp.Diff(want, got, gocmp.AllowUnexported(useDesktop{})); diff != "" {
				t.Errorf("object (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDesktopUseStopsOnACancelledContext drives the switch itself with a
// context cancelled after the follow-up's own check: it must stop before
// the lock and before any intent, so the journal stays absent and the
// profile at the data path stays where it is.
func TestDesktopUseStopsOnACancelledContext(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	layout := desktop.Paths(home)
	useDesktopProfile(t, layout.DefaultData, useDesktopConfig(useDesktopOwner))
	before, err := desktop.StatID(layout.DefaultData)
	if err != nil {
		t.Fatal(err)
	}
	dp := DesktopProcess{System: useDesktopSystem(t, root, nil, nil), LockWait: time.Second}
	env, err := dp.newEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rep := &desktopReport{Version: 1, Command: "use"}
	err = env.use(ctx, dp, rep, useDesktopIncoming, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("use = %v, want the context's cancellation", err)
	}
	got := useDesktopOf(rep, env, err)
	if got.Outcome != nil || got.Detail != "the swap was interrupted before the desktop step, so nothing was quit or moved" {
		t.Errorf("report object = %+v", got)
	}
	if _, err := os.Lstat(layout.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the journal was written: %v", err)
	}
	after, err := desktop.StatID(layout.DefaultData)
	if err != nil || after != before {
		t.Errorf("the data path changed: before %s, after %s (%v)", before, after, err)
	}
}

// TestUseDesktopWarning pins which switch outcomes the swap reports as a
// warning: every one after which the app does not run on the requested
// profile, and every switch that did not run.
func TestUseDesktopWarning(t *testing.T) {
	tests := map[string]struct {
		desktop *useDesktop
		want    string
	}{
		"success: selected":            {desktop: &useDesktop{Outcome: new(wordSelected)}},
		"success: already selected":    {desktop: &useDesktop{Outcome: new(wordAlreadySelected)}},
		"success: launched unverified": {desktop: &useDesktop{Outcome: new(wordLaunchedUnverified)}},
		"error: a finished undo that left a profile it could not bring back": {
			desktop: &useDesktop{Outcome: new(wordSelected), leftover: "the profile the switch being undone moved aside is still stored as fresh-00000000000000aa and was not put back"},
			want:    "the profile the switch being undone moved aside is still stored as fresh-00000000000000aa and was not put back",
		},
		"error: reverted and relaunched": {
			desktop: &useDesktop{Outcome: new(wordRevertedRelaunched), Detail: "the app did not start on the requested profile"},
			want:    "the desktop app switch ended reverted_relaunched: the app did not start on the requested profile",
		},
		"error: a refusal": {
			desktop: &useDesktop{Outcome: new(desktopIntentOpen), Detail: "operation 0123456789abcdef recorded its intent and no result"},
			want:    "the desktop app switch ended desktop_intent_open: operation 0123456789abcdef recorded its intent and no result",
		},
		"error: a switch that did not run": {
			desktop: &useDesktop{Detail: "no desktop switch was recorded for the swap being undone, so nothing was quit or moved"},
			want:    "the desktop app was not switched: no desktop switch was recorded for the swap being undone, so nothing was quit or moved",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, useDesktopWarning(tt.desktop)); diff != "" {
				t.Errorf("warning (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDesktopExpectationStale pins the comparison between what the swap's
// question promised and what the switch sees under the desktop lock.
func TestDesktopExpectationStale(t *testing.T) {
	owner := &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopOwner}
	incoming := &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopIncoming}
	third := &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopThird}
	fresh := &desktop.IdentityRecord{State: desktop.StateFresh}
	switchFrom := func(leaving *desktop.IdentityRecord, stored bool) *desktopExpectation {
		return &desktopExpectation{kind: expectSwitch, leaving: leaving, target: useDesktopIncoming, targetExisted: stored}
	}
	tests := map[string]struct {
		expect     *desktopExpectation
		now        desktopSeen
		wantStale  bool
		wantDetail string
	}{
		"success: the same account still leaves and the target is still absent": {
			expect: switchFrom(owner, false), now: desktopSeen{dataPath: owner},
		},
		"success: an empty data path is still empty and the target still stored": {
			expect: switchFrom(nil, true), now: desktopSeen{targetExisted: true},
		},
		"success: a profile nobody signed in to is still that": {
			expect: switchFrom(fresh, false), now: desktopSeen{dataPath: fresh},
		},
		"success: the app still runs on the account": {
			expect: &desktopExpectation{kind: expectNoop}, now: desktopSeen{dataPath: incoming},
		},
		"error: a third account signed in at the data path": {
			expect: switchFrom(owner, false), now: desktopSeen{dataPath: third}, wantStale: true,
			wantDetail: "the swap's question described a switch from account " + useDesktopOwner + " at the data path to a new profile for " + useDesktopIncoming + ", but the data path now holds account " + useDesktopThird + " and the profile " + useDesktopIncoming + " is not stored",
		},
		"error: the profile nobody signed in to was signed in since": {
			expect: switchFrom(fresh, false), now: desktopSeen{dataPath: owner}, wantStale: true,
		},
		"error: a profile appeared at an empty data path": {
			expect: switchFrom(nil, false), now: desktopSeen{dataPath: fresh}, wantStale: true,
		},
		"error: the profile at the data path went away": {
			expect: switchFrom(owner, false), now: desktopSeen{}, wantStale: true,
			wantDetail: "the data path now holds no profile",
		},
		"error: the account's profile was stored since": {
			expect: switchFrom(owner, false), now: desktopSeen{dataPath: owner, targetExisted: true}, wantStale: true,
			wantDetail: "is stored;",
		},
		"error: the account's stored profile was removed since": {
			expect: switchFrom(owner, true), now: desktopSeen{dataPath: owner}, wantStale: true,
			wantDetail: "to the stored profile " + useDesktopIncoming,
		},
		"error: the account the question named is now in place": {
			expect: switchFrom(owner, false), now: desktopSeen{dataPath: incoming}, wantStale: true,
		},
		"error: a target named for another account": {
			expect: &desktopExpectation{kind: expectSwitch, leaving: owner, target: useDesktopThird}, now: desktopSeen{dataPath: owner}, wantStale: true,
		},
		"error: the app no longer runs on the account": {
			expect: &desktopExpectation{kind: expectNoop}, now: desktopSeen{dataPath: owner}, wantStale: true,
			wantDetail: "the swap's question said the app already ran on account " + useDesktopIncoming + " and nothing would be quit or moved, but the data path now holds account " + useDesktopOwner,
		},
		"error: the data path emptied under an app that was already selected": {
			expect: &desktopExpectation{kind: expectNoop}, now: desktopSeen{}, wantStale: true,
		},
		"error: a promised refusal stays a refusal whatever the state now": {
			expect: &desktopExpectation{kind: expectRefused, reason: desktopObserveFailed}, now: desktopSeen{dataPath: owner}, wantStale: true,
			wantDetail: "the swap's question said the desktop switch would be refused (desktop_observe_failed), and the state has changed since; only what the question described was agreed to, so nothing was quit or moved; `agentctl claude desktop use " + useDesktopIncoming + "` asks about the switch as it now stands",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			detail, stale := tt.expect.stale(useDesktopIncoming, tt.now)
			t.Logf("stale = %v: %s", stale, detail)
			if stale != tt.wantStale {
				t.Fatalf("stale = %v, want %v", stale, tt.wantStale)
			}
			if !stale && detail != "" {
				t.Errorf("a matching expectation gave a detail: %q", detail)
			}
			if tt.wantDetail != "" && !strings.Contains(detail, tt.wantDetail) {
				t.Errorf("detail %q does not contain %q", detail, tt.wantDetail)
			}
		})
	}
}

// useDesktopTreeSig is a signature of every entry under dir: its relative
// path, mode, size and contents, so a test can prove nothing there moved
// or changed.
func useDesktopTreeSig(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b.WriteString(rel + " " + info.Mode().String())
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.WriteString(" " + string(body))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("signature of %s: %v", dir, err)
	}
	return b.String()
}

// TestFollowUpDesktopRefusesAStaleConsent drives the follow-up with the
// expectation the swap's question was composed from, after the state it
// described has changed. The harness changes the profile between the two
// looks, the way a sign-in during the swap would. The switch must refuse
// with desktop_consent_stale: the swap keeps its outcome and exit status,
// the app is not quit, no intent is written and no profile moves.
func TestFollowUpDesktopRefusesAStaleConsent(t *testing.T) {
	start := time.Date(2026, 10, 13, 7, 0, 0, 0, time.UTC)
	uid := uint32(os.Getuid())
	running := func(context.Context, uint32) ([]rdesktop.Record, error) {
		return []rdesktop.Record{{PID: 500, PPID: 1, UID: uid, Start: start, Path: rdesktop.MainPath}}, nil
	}
	tests := map[string]struct {
		// asked is config.json at the data path when the question is
		// composed, now the one the switch finds.
		asked, now string
		wantKind   desktopExpectKind
		wantBefore *desktop.IdentityRecord
		wantDetail string
	}{
		"error: a third account signed in after consent to switch away from the owner": {
			asked: useDesktopConfig(useDesktopOwner), now: useDesktopConfig(useDesktopThird), wantKind: expectSwitch,
			wantBefore: &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopThird},
			wantDetail: "the swap's question described a switch from account " + useDesktopOwner + " at the data path to a new profile for " + useDesktopIncoming + ", but the data path now holds account " + useDesktopThird,
		},
		"error: the app no longer runs on the account the question found selected": {
			asked: useDesktopConfig(useDesktopIncoming), now: useDesktopConfig(useDesktopOwner), wantKind: expectNoop,
			wantBefore: &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopOwner},
			wantDetail: "the swap's question said the app already ran on account " + useDesktopIncoming,
		},
		"error: a promised refusal that has cleared is still a refusal": {
			asked: `["not", "an", "object"]`, now: useDesktopConfig(useDesktopOwner), wantKind: expectRefused,
			wantDetail: "the swap's question said the desktop switch would be refused (" + desktopProfileUnreadable + "), and the state has changed since",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "home")
			t.Setenv("HOME", home)
			layout := desktop.Paths(home)
			useDesktopProfile(t, layout.DefaultData, tt.asked)

			// A stand-in for osascript and open that records every call, so
			// a quit or a launch cannot go unnoticed.
			calls := filepath.Join(root, "calls.log")
			stub := filepath.Join(root, "stub")
			if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$0 $*\" >> '"+calls+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			base := useDesktopSystem(t, root, running, nil)
			system := func(dataDir string) (*rdesktop.System, error) {
				sys, err := base(dataDir)
				if err != nil {
					return nil, err
				}
				sys.Osascript, sys.Open = stub, stub
				return sys, nil
			}
			var stderr bytes.Buffer
			p := SessionProcess{Err: &stderr, Desktop: DesktopProcess{System: system, LockWait: time.Second}}

			clause, expect := p.desktopConsentClause(t.Context(), useDesktopIncoming, false)
			t.Logf("clause: %s", clause)
			if expect == nil || expect.kind != tt.wantKind {
				t.Fatalf("expectation = %+v, want kind %d", expect, tt.wantKind)
			}
			// The state changes between the question and the switch.
			if err := os.WriteFile(filepath.Join(layout.DefaultData, "config.json"), []byte(tt.now), 0o600); err != nil {
				t.Fatal(err)
			}
			beforeID, err := desktop.StatID(layout.DefaultData)
			if err != nil {
				t.Fatal(err)
			}
			beforeSig := useDesktopTreeSig(t, layout.DefaultData)

			report := &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapApplied}, auditID: new(useDesktopSwapID)}
			p.followUpDesktop(t.Context(), cli.ClaudeUseOptions{Desktop: true}, report, useDesktopFollow{account: useDesktopIncoming, command: useDesktopForward, expect: expect})

			if report.outcome.Kind != claude.SwapApplied || report.outcome.ExitCode() != 0 {
				t.Errorf("swap outcome = %v (exit %d), want applied with exit 0", report.outcome.Kind, report.outcome.ExitCode())
			}
			got := report.desktop
			if got == nil || got.Outcome == nil {
				t.Fatalf("desktop object = %+v, want outcome %s", got, desktopConsentStale)
			}
			t.Logf("desktop: outcome %s, detail %s", *got.Outcome, got.Detail)
			if *got.Outcome != desktopConsentStale {
				t.Errorf("outcome = %q, want %q", *got.Outcome, desktopConsentStale)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("detail %q does not contain %q", got.Detail, tt.wantDetail)
			}
			if !strings.Contains(got.Detail, "nothing was quit or moved") {
				t.Errorf("detail %q does not say nothing was quit or moved", got.Detail)
			}
			if diff := gocmp.Diff(tt.wantBefore, got.Before); diff != "" {
				t.Errorf("before (-want +got):\n%s", diff)
			}
			if got.After != nil {
				t.Errorf("after = %+v, want null: no result row was written", got.After)
			}
			wantWarning := "the desktop app switch ended " + desktopConsentStale + ": " + got.Detail
			if diff := gocmp.Diff([]string{wantWarning}, report.warnings); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
			if stderr.String() != "warning: "+wantWarning+"\n" {
				t.Errorf("stderr = %q", stderr.String())
			}
			if _, err := os.Lstat(calls); !errors.Is(err, os.ErrNotExist) {
				body, _ := os.ReadFile(calls)
				t.Errorf("osascript or open ran: %q", body)
			}
			if body, err := os.ReadFile(layout.Journal); err == nil && strings.Contains(string(body), desktop.TypeIntent) {
				t.Errorf("an intent was written:\n%s", body)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			afterID, err := desktop.StatID(layout.DefaultData)
			if err != nil || afterID != beforeID {
				t.Errorf("the data path changed: before %s, after %s (%v)", beforeID, afterID, err)
			}
			if diff := gocmp.Diff(beforeSig, useDesktopTreeSig(t, layout.DefaultData)); diff != "" {
				t.Errorf("the profile at the data path changed (-before +after):\n%s", diff)
			}
			if entries, err := os.ReadDir(layout.ProfilesDir); err == nil && len(entries) > 0 {
				t.Errorf("profiles were stored: %v", entries)
			}
		})
	}
}

// TestDesktopConsentGuardAllowsTheDescribedSwitch is the guard's control:
// when nothing changed between the question and the lock, the guard lets
// the switch run and reports what it read.
func TestDesktopConsentGuardAllowsTheDescribedSwitch(t *testing.T) {
	tests := map[string]struct {
		data   string // config.json at the data path; "-" for none
		stored bool
		expect *desktopExpectation
	}{
		"success: the owner still leaves for a new profile": {
			data:   useDesktopConfig(useDesktopOwner),
			expect: &desktopExpectation{kind: expectSwitch, leaving: &desktop.IdentityRecord{State: desktop.StateUUID, UUID: useDesktopOwner}, target: useDesktopIncoming},
		},
		"success: an empty data path and a stored target, unchanged": {
			data: "-", stored: true,
			expect: &desktopExpectation{kind: expectSwitch, target: useDesktopIncoming, targetExisted: true},
		},
		"success: the app still runs on the account": {
			data:   useDesktopConfig(useDesktopIncoming),
			expect: &desktopExpectation{kind: expectNoop},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			layout := desktop.Paths(t.TempDir())
			if tt.data != "-" {
				useDesktopProfile(t, layout.DefaultData, tt.data)
			}
			if tt.stored {
				useDesktopProfile(t, layout.Profile(useDesktopIncoming), useDesktopConfig(useDesktopIncoming))
			}
			var before *desktop.IdentityRecord
			if err := desktopConsentGuard(tt.expect, useDesktopIncoming, layout, &before)(nil); err != nil {
				t.Fatalf("guard = %v, want the switch allowed", err)
			}
			var want *desktop.IdentityRecord
			if tt.data != "-" {
				want = new(desktop.RecordOf(desktop.ReadDesktopIdentity(layout.DefaultData)))
			}
			if diff := gocmp.Diff(want, before); diff != "" {
				t.Errorf("before (-want +got):\n%s", diff)
			}
		})
	}
}
