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
	"encoding/json/jsontext"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
)

const desktopTestRegistry = `{
  "version": 1,
  "accounts": [{
    "account_uuid": "11111111-2222-3333-4444-555555555555",
    "organization_uuid": "66666666-7777-8888-9999-000000000000",
    "email": "a@example.com",
    "org_name": "Acme",
    "label": "a",
    "kind": {"kind": "owned", "export_spelling": "", "export_sha8": ""},
    "forgotten": false,
    "created_at": "2026-09-08T00:00:00Z"
  }]
}
`

// desktopTestSystem builds a system whose bundle is a stand-in under root,
// whose process table is always empty and whose programs never run: the
// consent question comes before any quit or launch.
func desktopTestSystem(t *testing.T, root string) func(string) (*rdesktop.System, error) {
	t.Helper()
	bundle := filepath.Join(root, "Claude.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o700); err != nil {
		t.Fatal(err)
	}
	plistData := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + rdesktop.BundleID + `</string></dict></plist>
`
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plistData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "MacOS", "Claude"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return func(dataDir string) (*rdesktop.System, error) {
		return &rdesktop.System{
			Observer: rdesktop.Observer{
				List:    func(context.Context, uint32) ([]rdesktop.Record, error) { return nil, nil },
				UID:     uint32(os.Getuid()),
				DataDir: dataDir,
			},
			Osascript: filepath.Join(root, "no-osascript"), Open: filepath.Join(root, "no-open"),
			Bundle: bundle, PollInterval: time.Millisecond, QuitPolls: 1, LaunchPolls: 1,
			DeviceOf: func(string) (uint64, error) { return 1, nil },
		}, nil
	}
}

// TestRunUseJSONKeepsTheQuestionOffStdout drives the consent question
// through a terminal while --json is set: a program reading stdout must get
// exactly one JSON document, so the question has to go to the error stream.
func TestRunUseJSONKeepsTheQuestionOffStdout(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, configDir := filepath.Join(root, "home"), filepath.Join(root, "config")
	for _, dir := range []string{home, configDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(desktopTestRegistry), 0o600); err != nil {
		t.Fatal(err)
	}

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	if _, err := master.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	process := DesktopProcess{In: slave, Out: &stdout, Err: &stderr, System: desktopTestSystem(t, root), LockWait: time.Second}
	runErr := process.RunUse(t.Context(), cli.Globals{ConfigDir: configDir}, cli.ClaudeDesktopUseOptions{ID: "a", JSON: true})
	t.Logf("run error: %v\nstdout: %s\nstderr: %s", runErr, stdout.String(), stderr.String())
	if runErr == nil {
		t.Fatal("declining the question must not report success")
	}

	if !strings.Contains(stderr.String(), "[y/N]") {
		t.Errorf("the question did not reach the error stream: %q", stderr.String())
	}
	dec := jsontext.NewDecoder(&stdout)
	if _, err := dec.ReadValue(); err != nil {
		t.Fatalf("stdout does not start with a JSON document: %v", err)
	}
	if _, err := dec.ReadValue(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries more than one JSON document: %v", err)
	}
}

// TestLaunchObservedNeverInfersAStoppedApp pins what a result row records
// after a launch: a ready launch's main, or what one more listing shows.
// A launch that was not ready says nothing about whether the app runs, so
// a listing that fails must leave both the main and the running state
// unknown instead of recording a stopped app.
func TestLaunchObservedNeverInfersAStoppedApp(t *testing.T) {
	start := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	uid := uint32(os.Getuid())
	ready := &rdesktop.Proc{PID: 700, PPID: 1, Start: start, Path: rdesktop.MainPath}
	listFails := func(context.Context, uint32) ([]rdesktop.Record, error) {
		return nil, errors.New("EIO")
	}
	listNothing := func(context.Context, uint32) ([]rdesktop.Record, error) { return nil, nil }
	listMain := func(context.Context, uint32) ([]rdesktop.Record, error) {
		return []rdesktop.Record{{PID: 900, PPID: 1, UID: uid, Start: start, Path: rdesktop.MainPath}}, nil
	}

	tests := map[string]struct {
		launch      rdesktop.LaunchReport
		list        rdesktop.Lister
		wantPID     int32
		wantRunning *bool
	}{
		"success: a ready launch records its main without another listing": {
			launch:      rdesktop.LaunchReport{Ready: true, Main: ready},
			list:        listFails,
			wantPID:     700,
			wantRunning: new(true),
		},
		"success: a launch that was not ready and a listing without the app record it stopped": {
			launch:      rdesktop.LaunchReport{},
			list:        listNothing,
			wantRunning: new(false),
		},
		"success: a main that the last listing finds is recorded running": {
			launch:      rdesktop.LaunchReport{ObserveErr: errors.New("EIO")},
			list:        listMain,
			wantPID:     900,
			wantRunning: new(true),
		},
		"error: a wait that could not list and a failed last listing leave both unknown": {
			launch: rdesktop.LaunchReport{ObserveErr: errors.New("EIO")},
			list:   listFails,
		},
		"error: a failed start and a failed listing leave both unknown": {
			launch: rdesktop.LaunchReport{OpenErr: errors.New("open failed")},
			list:   listFails,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := &desktopEnv{
				sys: &rdesktop.System{Observer: rdesktop.Observer{List: tt.list, UID: uid, DataDir: t.TempDir()}},
				log: slog.New(slog.DiscardHandler),
			}
			main, running := e.launchObserved(t.Context(), tt.launch)
			var gotPID int32
			if main != nil {
				gotPID = main.PID
			}
			if gotPID != tt.wantPID {
				t.Errorf("main pid = %d, want %d", gotPID, tt.wantPID)
			}
			if diff := gocmp.Diff(tt.wantRunning, running); diff != "" {
				t.Errorf("running (-want +got):\n%s", diff)
			}
		})
	}
}

// desktopLaunchFixture builds an environment whose data path holds an
// empty profile, whose opener is missing and whose listing is replaced by
// list, together with an intent whose outgoing profile is that data path.
// A test that needs a launch to go through points the opener at
// syntheticOpener; nothing here can reach the real app.
func desktopLaunchFixture(t *testing.T, list rdesktop.Lister) (*desktopEnv, *desktop.Intent) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := desktop.Paths(filepath.Join(root, "home"))
	for _, dir := range []string{layout.DefaultData, layout.ProfilesDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sys, err := desktopTestSystem(t, root)(layout.DefaultData)
	if err != nil {
		t.Fatal(err)
	}
	sys.Observer.List = list
	id, err := desktop.StatID(layout.DefaultData)
	if err != nil {
		t.Fatal(err)
	}
	in := &desktop.Intent{Type: desktop.TypeIntent, OpID: "0123456789abcdef", Command: "use", Outgoing: &desktop.Outgoing{File: id}}
	return &desktopEnv{layout: layout, sys: sys, log: slog.New(slog.DiscardHandler)}, in
}

// syntheticOpener replaces the system's opener with a script that exits
// successfully without starting anything, so the launch goes on to wait.
func syntheticOpener(t *testing.T, sys *rdesktop.System) {
	t.Helper()
	sys.Open = filepath.Join(filepath.Dir(sys.Bundle), "synthetic-open")
	if err := os.WriteFile(sys.Open, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// scriptedListing answers the n-th process listing with the n-th entry of
// steps and repeats the last entry after that. A nil entry lists no
// process, a positive pid lists one main with that pid, and a negative one
// fails the listing.
func scriptedListing(steps ...int32) rdesktop.Lister {
	calls := 0
	return func(context.Context, uint32) ([]rdesktop.Record, error) {
		step := steps[min(calls, len(steps)-1)]
		calls++
		switch {
		case step < 0:
			return nil, errors.New("EIO")
		case step == 0:
			return nil, nil
		}
		start := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
		return []rdesktop.Record{{PID: step, PPID: 1, UID: uint32(os.Getuid()), Start: start, Path: rdesktop.MainPath}}, nil
	}
}

// lastResult reads the journal back and returns its last result row.
func lastResult(t *testing.T, e *desktopEnv) *desktop.Result {
	t.Helper()
	entries, err := desktop.ReadJournal(e.layout.Journal)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Result != nil {
			return entries[i].Result
		}
	}
	t.Fatal("the journal holds no result row")
	return nil
}

// TestFailedFirstMoveNamesOnlyAnObservedRestart pins the refusal after the
// first move failed: it may say the app was started again only when the
// launch saw its main, and otherwise says a restart was attempted, carries
// the opener's failure and names any main the last listing found. The row
// keeps the aborted direction and records what that listing showed.
func TestFailedFirstMoveNamesOnlyAnObservedRestart(t *testing.T) {
	tests := map[string]struct {
		opener      bool
		listing     []int32
		wantIn      []string
		wantOut     []string
		wantPID     int32
		wantRunning *bool
	}{
		"success: a launch that saw its main says the app was started again": {
			opener:      true,
			listing:     []int32{700},
			wantIn:      []string{"nothing was moved and the app was started again"},
			wantOut:     []string{"attempted"},
			wantPID:     700,
			wantRunning: new(true),
		},
		"error: a missing opener and a listing without the app say a restart was attempted": {
			listing:     []int32{0},
			wantIn:      []string{"a restart was attempted, but the app could not be started (", "no-open"},
			wantOut:     []string{"started again", "observed afterwards"},
			wantRunning: new(false),
		},
		"error: a missing opener and a main found afterwards name that main": {
			listing:     []int32{900},
			wantIn:      []string{"a restart was attempted, but the app could not be started (", "a main was observed afterwards (pid 900)"},
			wantOut:     []string{"started again"},
			wantPID:     900,
			wantRunning: new(true),
		},
		"error: an unconfirmed wait and a failed listing keep the listing error": {
			opener:  true,
			listing: []int32{-1},
			wantIn:  []string{"a restart was attempted, but the app's start was not confirmed within the launch wait", "the processes could not be listed: listing processes: EIO"},
			wantOut: []string{"started again", "observed afterwards"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e, in := desktopLaunchFixture(t, scriptedListing(tt.listing...))
			if tt.opener {
				syntheticOpener(t, e.sys)
			}
			rep := &desktopReport{}
			err := e.failedFirstMove(t.Context(), rep, in, nil, desktop.ErrDestinationExists)
			t.Logf("refusal: %v", err)
			refusal, ok := errors.AsType[*desktopRefusal](err)
			if !ok {
				t.Fatalf("want a refusal, got %v", err)
			}
			if refusal.Word != wordDestinationExists {
				t.Errorf("word = %q, want %q", refusal.Word, wordDestinationExists)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(refusal.Detail, want) {
					t.Errorf("refusal %q does not contain %q", refusal.Detail, want)
				}
			}
			for _, unwanted := range tt.wantOut {
				if strings.Contains(refusal.Detail, unwanted) {
					t.Errorf("refusal %q must not contain %q", refusal.Detail, unwanted)
				}
			}
			if rep.Journal != string(desktop.OutcomeAborted) {
				t.Errorf("journal = %q, want %q", rep.Journal, desktop.OutcomeAborted)
			}
			res := lastResult(t, e)
			if res.Outcome != desktop.OutcomeAborted || res.Word != wordDestinationExists {
				t.Errorf("row outcome/word = %s/%s, want %s/%s", res.Outcome, res.Word, desktop.OutcomeAborted, wordDestinationExists)
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
