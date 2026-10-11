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
	"errors"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/provider/claude/desktop"
)

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
