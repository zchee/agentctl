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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider/claude/desktop"
)

func TestRepairFailedMoveKeepsTheIntentOpen(t *testing.T) {
	tests := map[string]struct {
		sourceExists bool
	}{
		"error: the source disappeared":      {},
		"error: the destination is occupied": {sourceExists: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e, in := desktopLaunchFixture(t, scriptedListing(0))
			if err := e.appendRecord(in, false); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(e.layout.Journal)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(e.layout.ProfilesDir, "incoming")
			if tt.sourceExists {
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			_, moveErr := desktop.Move(source, e.layout.DefaultData, nil)
			if moveErr == nil {
				t.Fatal("move unexpectedly succeeded despite an absent source or occupied destination")
			}
			rep := &desktopReport{}
			err = e.moveFailed(rep, moveErr)
			refusal, ok := errors.AsType[*desktopRefusal](err)
			if !ok || refusal.Word != desktopRepairInconsistent || !strings.Contains(refusal.Detail, moveErr.Error()) {
				t.Fatalf("moveFailed = %v, want inconsistent refusal retaining move error %v", err, moveErr)
			}
			if rep.Journal != journalOpen || rep.Outcome != "" || e.recorded != nil {
				t.Fatalf("failed move claimed a result: report=%+v recorded=%+v", rep, e.recorded)
			}
			after, err := os.ReadFile(e.layout.Journal)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(before, after); diff != "" {
				t.Fatalf("failed move changed journal (-want +got):\n%s", diff)
			}
			entries, err := desktop.ReadJournal(e.layout.Journal)
			if err != nil || len(desktop.OpenIntents(entries)) != 1 {
				t.Fatalf("failed move closed intent: entries=%+v error=%v", entries, err)
			}
		})
	}
}

func TestRepairQuestionNamesTheChosenAction(t *testing.T) {
	tests := map[string]struct {
		action repairAction
		verb   string
	}{
		"success: an implicit action finishes": {actionNone, "finish"},
		"success: complete finishes":           {actionComplete, "finish"},
		"success: revert undoes":               {actionRevert, "undo"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := &desktop.Intent{OpID: "0123456789abcdef", RequestedAccount: "account-123"}
			want := "Operation 0123456789abcdef stopped at outgoing-moved. Quit the Claude desktop app if it runs, " + tt.verb + " the switch to account account-123 by moving the profiles, and start the app?"
			if diff := gocmp.Diff(want, repairQuestion(stateOutgoingMoved, tt.action, in)); diff != "" {
				t.Fatalf("question (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRepairDeclinedConsentLeavesProfilesAndJournalUnchanged(t *testing.T) {
	tests := map[string]struct{ action repairAction }{
		"error: complete was not confirmed": {actionComplete},
		"error: revert was not confirmed":   {actionRevert},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e, in := desktopLaunchFixture(t, scriptedListing(0))
			in.RequestedAccount = "account-123"
			in.Outgoing.Destination = filepath.Join(e.layout.ProfilesDir, "outgoing")
			if err := os.Rename(e.layout.DefaultData, in.Outgoing.Destination); err != nil {
				t.Fatal(err)
			}
			if err := e.appendRecord(in, false); err != nil {
				t.Fatal(err)
			}
			before, err := e.observeLayout(in)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := os.ReadFile(e.layout.Journal)
			if err != nil {
				t.Fatal(err)
			}
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
			if _, err := master.WriteString("no\n"); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			e.prompt = TerminalPrompt{In: slave, Out: &out}
			rep := &desktopReport{}
			err = e.repair(t.Context(), DesktopProcess{}, rep, tt.action, false)
			if err == nil || !strings.Contains(err.Error(), "the repair was not confirmed; nothing was moved") {
				t.Fatalf("declined repair = %v, want consent refusal", err)
			}
			if diff := gocmp.Diff(repairQuestion(stateOutgoingMoved, tt.action, in)+" [y/N] ", out.String()); diff != "" {
				t.Errorf("confirmation output (-want +got):\n%s", diff)
			}
			after, err := e.observeLayout(in)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(before.rows, after.rows); diff != "" {
				t.Fatalf("declined repair moved profiles (-want +got):\n%s", diff)
			}
			afterJournal, err := os.ReadFile(e.layout.Journal)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(journal, afterJournal); diff != "" {
				t.Fatalf("declined repair changed journal (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRepairLaunchSaysOnlyWhatItObserved pins the refusal after a repair
// launch that was not ready: the launch wait decides the word, exit and
// verification, while the text must not claim the app did not start. It
// says the start was not confirmed, names a main the last listing found,
// and keeps the listing error when the processes could not be listed.
func TestRepairLaunchSaysOnlyWhatItObserved(t *testing.T) {
	tests := map[string]struct {
		listing     []int32
		wantIn      []string
		wantPID     int32
		wantRunning *bool
	}{
		"success: a main found after the launch wait is named": {
			listing:     []int32{0, 900},
			wantIn:      []string{"the profiles are in place (applied) but the app's start was not confirmed within the launch wait; a main was observed afterwards (pid 900)"},
			wantPID:     900,
			wantRunning: new(true),
		},
		"error: a listing that keeps failing leaves the state unknown and names the error": {
			listing: []int32{-1},
			wantIn:  []string{"the app's start was not confirmed within the launch wait", "the processes could not be listed: listing processes: EIO"},
		},
		"error: a listing without the app records it stopped": {
			listing:     []int32{0},
			wantIn:      []string{"the app's start was not confirmed within the launch wait"},
			wantRunning: new(false),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e, in := desktopLaunchFixture(t, scriptedListing(tt.listing...))
			syntheticOpener(t, e.sys)
			rep := &desktopReport{}
			err := e.repairLaunch(t.Context(), rep, in, desktop.OutcomeApplied, nil)
			t.Logf("refusal: %v", err)
			refusal, ok := errors.AsType[*desktopRefusal](err)
			if !ok {
				t.Fatalf("want a refusal, got %v", err)
			}
			if refusal.Word != wordRepairedNotRunning {
				t.Errorf("word = %q, want %q", refusal.Word, wordRepairedNotRunning)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(refusal.Detail, want) {
					t.Errorf("refusal %q does not contain %q", refusal.Detail, want)
				}
			}
			if strings.Contains(refusal.Detail, "did not start") {
				t.Errorf("refusal %q claims the app did not start", refusal.Detail)
			}
			if rep.Outcome != wordRepairedNotRunning || rep.Journal != string(desktop.OutcomeApplied) {
				t.Errorf("report outcome/journal = %s/%s, want %s/%s", rep.Outcome, rep.Journal, wordRepairedNotRunning, desktop.OutcomeApplied)
			}
			res := lastResult(t, e)
			if res.Outcome != desktop.OutcomeApplied || res.Word != wordRepairedNotRunning || res.Verified {
				t.Errorf("row outcome/word/verified = %s/%s/%v, want %s/%s/false", res.Outcome, res.Word, res.Verified, desktop.OutcomeApplied, wordRepairedNotRunning)
			}
			var gotPID int32
			if res.Main != nil {
				gotPID = res.Main.PID
			}
			if gotPID != tt.wantPID {
				t.Errorf("row main pid = %d, want %d", gotPID, tt.wantPID)
			}
			if diff := gocmp.Diff(tt.wantRunning, res.AppRunning); diff != "" {
				t.Errorf("row app_running (-want +got):\n%s", diff)
			}
		})
	}
}
