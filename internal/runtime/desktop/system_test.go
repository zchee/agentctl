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
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWorld is a process table driven by marker files that two small
// programs create or remove, standing in for the app's reaction to the
// quit request and to the launch.
type fakeWorld struct {
	dir   string
	reads atomic.Int32
}

func (w *fakeWorld) has(name string) bool {
	_, err := os.Stat(filepath.Join(w.dir, name))
	return err == nil
}

func (w *fakeWorld) list(_ context.Context, uid uint32) ([]Record, error) {
	n := w.reads.Add(1)
	if w.has("list-fails") {
		return nil, errors.New("kernel said no")
	}
	rows := []Record{row(1, 0, 0, "")}
	if w.has("running") {
		rows = append(rows, row(100, 1, uid, MainPath), row(101, 100, uid, helper))
	}
	if w.has("linger") {
		rows = append(rows, row(102, 1, uid, gpu), row(110, 1, uid, hosted))
	}
	if w.has("othercopy") {
		rows = append(rows, row(300, 1, uid, "/Volumes/X/Claude.app/Contents/MacOS/Claude"))
	}
	if w.has("launched") {
		visibleFrom := int32(0)
		if data, err := os.ReadFile(filepath.Join(w.dir, "launched")); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				visibleFrom = int32(v)
			}
		}
		if n >= visibleFrom {
			rows = append(rows, Record{PID: 200, PPID: 1, UID: uid, Start: t0.Add(time.Hour), Path: MainPath})
		}
	}
	return rows, nil
}

const fakeOsascript = `#!/bin/sh
printf '%s\n' "$*" >> "$(dirname "$0")/osascript.log"
mode=$(cat "$(dirname "$0")/osascript-mode" 2>/dev/null)
case "$mode" in
refuse) echo "execution error: not allowed" >&2; exit 1 ;;
ignore) exit 0 ;;
esac
rm -f "$(dirname "$0")/running"
`

const fakeOpen = `#!/bin/sh
printf '%s\n' "$*" >> "$(dirname "$0")/open.log"
mode=$(cat "$(dirname "$0")/open-mode" 2>/dev/null)
case "$mode" in
fail) echo "LSOpenURLsWithRole() failed" >&2; exit 1 ;;
none) exit 0 ;;
late) cat "$(dirname "$0")/late-at" > "$(dirname "$0")/launched"; exit 0 ;;
esac
: > "$(dirname "$0")/launched"
`

func newFakeSystem(t *testing.T, markers ...string) (*System, *fakeWorld) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"osascript": fakeOsascript, "open": fakeOpen} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range markers {
		name, body, _ := strings.Cut(m, "=")
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := &fakeWorld{dir: dir}
	return &System{
		Observer:     Observer{List: w.list, UID: me, DataDir: dataDir},
		Osascript:    filepath.Join(dir, "osascript"),
		Open:         filepath.Join(dir, "open"),
		Bundle:       "/fixture/Claude.app",
		PollInterval: time.Millisecond,
		QuitPolls:    5,
		LaunchPolls:  4,
	}, w
}

func logLines(t *testing.T, w *fakeWorld, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestQuit(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		markers     []string
		wantSkipped bool
		// wantErr checks the error; nil means success.
		wantErr func(error) bool
		// wantRequests is the number of quit requests sent.
		wantRequests int
	}{
		"success: nothing runs, so no request is sent": {
			wantSkipped: true,
		},
		"success: the app quits and every helper goes with it": {
			markers: []string{"running"}, wantRequests: 1,
		},
		"error: the quit request is refused": {
			markers: []string{"running", "osascript-mode=refuse"}, wantRequests: 1,
			wantErr: func(err error) bool { var e *QuitRefusedError; return errors.As(err, &e) },
		},
		"error: the app ignores the request until the budget ends": {
			markers: []string{"running", "osascript-mode=ignore"}, wantRequests: 1,
			wantErr: func(err error) bool {
				var e *QuitTimeoutError
				return errors.As(err, &e) && e.Mains == 1 && e.Family == 1
			},
		},
		"error: the main goes but helpers and a hosted child linger": {
			markers: []string{"running", "linger"}, wantRequests: 1,
			wantErr: func(err error) bool {
				var e *QuitTimeoutError
				return errors.As(err, &e) && e.Mains == 0 && e.Family == 2
			},
		},
		"error: only lingering processes and no main still get a request and time out": {
			markers: []string{"linger"}, wantRequests: 1,
			wantErr: func(err error) bool { var e *QuitTimeoutError; return errors.As(err, &e) && e.Family == 2 },
		},
		"error: another copy runs, nothing is requested": {
			markers: []string{"running", "othercopy"},
			wantErr: func(err error) bool { var e *OtherCopyError; return errors.As(err, &e) && e.Procs[0].PID == 300 },
		},
		"error: the listing fails, nothing is requested": {
			markers: []string{"running", "list-fails"},
			wantErr: func(err error) bool { return err != nil },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sys, w := newFakeSystem(t, tt.markers...)
			report, err := sys.Quit(t.Context())
			t.Logf("report skipped=%v err=%v", report.Skipped, err)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("Quit: %v", err)
			}
			if tt.wantErr != nil && !tt.wantErr(err) {
				t.Fatalf("Quit error %v does not match", err)
			}
			if report.Skipped != tt.wantSkipped {
				t.Fatalf("Skipped = %v, want %v", report.Skipped, tt.wantSkipped)
			}
			requests := logLines(t, w, "osascript.log")
			if len(requests) != tt.wantRequests {
				t.Fatalf("quit requests %q, want %d", requests, tt.wantRequests)
			}
			for _, r := range requests {
				if r != "-e "+quitScript {
					t.Fatalf("quit argv %q", r)
				}
			}
		})
	}
}

func TestQuitStopsWhenAnotherCopyAppearsDuringTheWait(t *testing.T) {
	t.Parallel()
	sys, w := newFakeSystem(t, "running", "osascript-mode=ignore")
	inner := sys.Observer.List
	sys.Observer.List = func(ctx context.Context, uid uint32) ([]Record, error) {
		if w.reads.Load() == 2 {
			if err := os.WriteFile(filepath.Join(w.dir, "othercopy"), nil, 0o600); err != nil {
				return nil, err
			}
		}
		return inner(ctx, uid)
	}
	_, err := sys.Quit(t.Context())
	if _, ok := errors.AsType[*OtherCopyError](err); !ok {
		t.Fatalf("Quit = %v, want another copy refusal", err)
	}
}

func TestLaunch(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		markers   []string
		before    []Proc
		wantReady bool
		wantOpen  bool
		// wantLateMain is whether one more observation after the wait
		// finds a new main.
		wantLateMain bool
	}{
		"success: a new main appears": {
			wantReady: true,
		},
		"error: the launcher fails": {
			markers: []string{"open-mode=fail"}, wantOpen: true,
		},
		"error: the launcher succeeds but no main appears": {
			markers: []string{"open-mode=none"},
		},
		"error: a main that was already running is not a new one": {
			markers: []string{"running", "open-mode=none"},
			before:  []Proc{{PID: 100, PPID: 1, Start: t0.Add(100 * time.Millisecond), Path: MainPath}},
		},
		"success: a main appears only after the wait, seen by the late check": {
			// Reads 1 to 4 are the wait; the late check is read 5.
			markers: []string{"open-mode=late", "late-at=5"}, wantLateMain: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sys, w := newFakeSystem(t, tt.markers...)
			report := sys.Launch(t.Context(), tt.before)
			t.Logf("report %+v reads %d", report, w.reads.Load())
			if report.Ready != tt.wantReady {
				t.Fatalf("Ready = %v, want %v", report.Ready, tt.wantReady)
			}
			if (report.OpenErr != nil) != tt.wantOpen {
				t.Fatalf("OpenErr = %v", report.OpenErr)
			}
			if tt.wantReady && (report.Main == nil || report.Main.PID != 200) {
				t.Fatalf("Main = %+v, want pid 200", report.Main)
			}
			if got := logLines(t, w, "open.log"); len(got) != 1 || got[0] != "-n /fixture/Claude.app" {
				t.Fatalf("open argv %q", got)
			}
			if !tt.wantReady && !tt.wantOpen {
				if got := w.reads.Load(); got != int32(sys.LaunchPolls) {
					t.Fatalf("the wait observed %d times, want exactly %d", got, sys.LaunchPolls)
				}
			}
			late, err := sys.Observer.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got := NewMain(late, tt.before) != nil; !tt.wantReady && got != tt.wantLateMain {
				t.Fatalf("late check found a new main = %v, want %v", got, tt.wantLateMain)
			}
		})
	}
}

func TestDefault(t *testing.T) {
	sys, err := Default(dataDir)
	t.Logf("Default on %s: %+v, %v", runtime.GOOS, sys, err)
	switch {
	case runtime.GOOS != "darwin":
		if !errors.Is(err, ErrUnsupportedPlatform) || sys != nil {
			t.Fatalf("Default off macOS = %v, %v; want a refusal before any read", sys, err)
		}
	case testWorld != nil:
		// The testing build with no test world configured must refuse
		// rather than reach the real app.
		t.Setenv("AGENTCTL_TEST_PROCESS_TABLE", "")
		if _, err := Default(dataDir); err == nil {
			t.Fatal("the testing build produced a system without its test world")
		}
	default:
		if err != nil {
			t.Fatalf("Default: %v", err)
		}
		if sys.Osascript != "/usr/bin/osascript" || sys.Open != "/usr/bin/open" || sys.Bundle != AppBundle {
			t.Fatalf("Default programs %s %s %s", sys.Osascript, sys.Open, sys.Bundle)
		}
		if sys.QuitPolls*int(sys.PollInterval/time.Millisecond) != 20000 || sys.LaunchPolls*int(sys.PollInterval/time.Millisecond) != 15000 {
			t.Fatalf("budgets %d x %v and %d x %v are not 20 s and 15 s", sys.QuitPolls, sys.PollInterval, sys.LaunchPolls, sys.PollInterval)
		}
	}
}
