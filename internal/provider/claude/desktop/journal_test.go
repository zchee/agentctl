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

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

const (
	testOp1 = "00000000000000a1"
	testOp2 = "00000000000000b2"
)

func sampleIntent(op string) Intent {
	return Intent{
		Type: TypeIntent, OpID: op, Time: "2026-10-11T10:00:00Z", Command: "use",
		RequestedAccount: "552d914f-aaaa-bbbb-cccc-000000000001",
		Outgoing: &Outgoing{
			Name: "4a650b30-aaaa-bbbb-cccc-000000000002", Identity: IdentityRecord{State: StateUUID, UUID: "4a650b30-aaaa-bbbb-cccc-000000000002"},
			Destination: "/p/4a650b30-aaaa-bbbb-cccc-000000000002", File: FileID{Dev: 16777230, Ino: 122371396},
		},
		Target: Target{Identity: IdentityRecord{State: StateFresh}, Park: "/p/fresh-" + op + "-target"},
	}
}

func sampleResult(op string, outcome Outcome) Result {
	return Result{
		Type: TypeResult, OpID: op, Time: "2026-10-11T10:00:05Z", Command: "use", Outcome: outcome, Word: "selected",
		Verified: true, Observed: &IdentityRecord{State: StateFresh}, Installed: &FileID{Dev: 16777230, Ino: 9},
		Main: &Main{PID: 43157, Start: "2026-10-11T10:00:04.5Z"}, AppRunning: true,
	}
}

func TestJournalRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "journal.jsonl")
	in1, res1, in2 := sampleIntent(testOp1), sampleResult(testOp1, OutcomeApplied), sampleIntent(testOp2)
	for _, rec := range []any{in1, res1, in2} {
		if err := Append(path, rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("journal:\n%s", data)
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Fatalf("journal has %d lines, want 3", lines)
	}
	// A null installed pair and a null main are written as null, so a
	// reader can tell "observed absent" from "not recorded".
	nullResult := sampleResult(testOp2, OutcomeApplied)
	nullResult.Installed, nullResult.Main, nullResult.AppRunning = nil, nil, false
	if err := Append(path, nullResult); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	last := strings.Split(strings.TrimSpace(string(data)), "\n")[3]
	for _, member := range []string{`"installed":null`, `"main":null`, `"app_running":false`} {
		if !strings.Contains(last, member) {
			t.Fatalf("result line %s lacks %s", last, member)
		}
	}

	entries, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("read %d entries, want 4", len(entries))
	}
	if diff := gocmp.Diff(in1, *entries[0].Intent); diff != "" {
		t.Fatalf("intent round trip (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(res1, *entries[1].Result); diff != "" {
		t.Fatalf("result round trip (-want +got):\n%s", diff)
	}
	if open := OpenIntents(entries); len(open) != 0 {
		t.Fatalf("OpenIntents = %d, want 0 once both results are written", len(open))
	}
	if applied := LatestApplied(entries); applied == nil || applied.OpID != testOp2 {
		t.Fatalf("LatestApplied = %+v, want %s", applied, testOp2)
	}
	if got := IntentFor(entries, testOp2); got == nil || got.OpID != testOp2 {
		t.Fatalf("IntentFor = %+v", got)
	}
}

func TestReadJournal(t *testing.T) {
	t.Parallel()

	intentLine := `{"type":"desktop_intent","op_id":"` + testOp1 + `","time":"t","command":"use","requested_account":"a","outgoing":null,"target":{"identity":{"state":"fresh"},"target_existed":false}}`
	tests := map[string]struct {
		body     string
		wantOpen []string
		wantErr  bool
	}{
		"success: absent journal is empty": {body: "\x00absent"},
		"success: interrupted operation is open": {
			body: intentLine + "\n", wantOpen: []string{testOp1},
		},
		"success: unknown future members are ignored": {
			body:     strings.Replace(intentLine, `"time":"t"`, `"time":"t","future_member":{"x":[1]}`, 1) + "\n",
			wantOpen: []string{testOp1},
		},
		"success: blank lines are skipped": {
			body: "\n" + intentLine + "\n\n", wantOpen: []string{testOp1},
		},
		"success: a result closes only its own operation": {
			body: intentLine + "\n" + strings.ReplaceAll(intentLine, testOp1, testOp2) + "\n" +
				`{"type":"desktop_result","op_id":"` + testOp1 + `","time":"t","command":"use","outcome":"applied","word":"selected","verified":true,"observed":null,"installed":null,"main":null,"app_running":false}` + "\n",
			wantOpen: []string{testOp2},
		},
		"error: malformed line":      {body: intentLine + "\n{not json\n", wantErr: true},
		"error: unknown record type": {body: `{"type":"desktop_other","op_id":"` + testOp1 + `"}` + "\n", wantErr: true},
		"error: bad operation id":    {body: strings.ReplaceAll(intentLine, testOp1, "../../x") + "\n", wantErr: true},
		"error: line longer than any record": {
			body: `{"type":"desktop_intent","op_id":"` + testOp1 + `","pad":"` + strings.Repeat("x", maxJournalLine) + `"}` + "\n", wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			if tt.body != "\x00absent" {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := ReadJournal(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ReadJournal succeeded with %d entries; want an error", len(entries))
				}
				t.Logf("error: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("ReadJournal: %v", err)
			}
			var open []string
			for _, in := range OpenIntents(entries) {
				open = append(open, in.OpID)
			}
			if diff := gocmp.Diff(tt.wantOpen, open); diff != "" {
				t.Fatalf("open intents (-want +got):\n%s", diff)
			}
		})
	}
}

func TestJournalRefusesSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "journal.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, sampleIntent(testOp1)); err == nil {
		t.Fatal("Append through a symbolic link succeeded")
	}
	if _, err := ReadJournal(path); err == nil {
		t.Fatal("ReadJournal through a symbolic link succeeded")
	}
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Fatalf("the link target was written: %q", data)
	}
}

// TestAppendFailureLikeAFullDisk drives the append failure path the way a
// full disk would report it and proves nothing reaches the journal.
func TestAppendFailureLikeAFullDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	appendHook = func(string) error { return &os.PathError{Op: "write", Path: path, Err: syscall.ENOSPC} }
	t.Cleanup(func() { appendHook = nil })
	err := Append(path, sampleIntent(testOp1))
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Append = %v, want ENOSPC", err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the journal exists after a failed append: %v", statErr)
	}
}
