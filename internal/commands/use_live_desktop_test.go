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
			err := desktopUndoGuard(useDesktopSwapID, useDesktopOwner, data, &leftover)(entries)
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
	}{
		"success: an account profile leaves and a new one is created": {
			data: useDesktopConfig(useDesktopOwner), list: running, account: useDesktopIncoming,
			want: ". Then quit the Claude desktop app (2 running process(es) close), the profile at the data path (account " + useDesktopOwner + ") moves to " + useDesktopOwner + ", a new profile is created at the next start (sign in once), and start the app for account " + useDesktopIncoming + tail,
		},
		"success: the stored profile moves in for an undo": {
			data: useDesktopConfig(useDesktopIncoming), stored: true, account: useDesktopOwner, undo: true,
			want: ". Then quit the Claude desktop app (0 running process(es) close), the profile at the data path (account " + useDesktopIncoming + ") moves to " + useDesktopIncoming + ", the stored profile " + useDesktopOwner + " moves in, and start the app for account " + useDesktopOwner + ", if the desktop switch recorded for the swap being undone is still in place" + tail,
		},
		"success: a profile nobody signed in to gets a name drawn by the switch": {
			data: "", account: useDesktopIncoming,
			wantPart: "the profile at the data path (fresh) moves to fresh-<operation id>",
		},
		"success: no data path yet": {
			data: "-", account: useDesktopIncoming,
			wantPart: "no profile is at the data path, a new profile is created",
		},
		"success: the account already in place": {
			data: useDesktopConfig(useDesktopIncoming), account: useDesktopIncoming,
			want: ". The desktop app already runs on that account's profile, so the desktop switch quits and moves nothing",
		},
		"error: another platform": {
			data: "-", sysErr: rdesktop.ErrUnsupportedPlatform, account: useDesktopIncoming,
			want: ". The desktop app switch will be refused (desktop_unsupported_platform: the desktop app switch is supported only on macOS); the swap is not affected",
		},
		"error: the processes cannot be listed": {
			data: "-", account: useDesktopIncoming,
			list:     func(context.Context, uint32) ([]rdesktop.Record, error) { return nil, errors.New("EIO") },
			wantPart: ". The desktop app switch will be refused (desktop_observe_failed: could not observe the app's processes: ",
		},
		"error: the profile at the data path cannot be read": {
			data: "[1]", account: useDesktopIncoming,
			wantPart: ". The desktop app switch will be refused (the profile at the data path cannot be read: ",
		},
		"error: an account without a UUID the app could record": {
			data: "-", account: "not-a-uuid",
			want: ". The desktop app switch will be refused (the account has no account UUID the desktop app could record); the swap is not affected",
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
			got := p.desktopConsentClause(t.Context(), tt.account, tt.undo)
			t.Logf("clause: %s", got)
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
