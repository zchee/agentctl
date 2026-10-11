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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

// Budgets of the two waits, as poll counts at the default interval: the
// quit wait takes up to 20 s and the launch wait up to 15 s. Counting
// polls rather than wall time keeps the number of observations fixed,
// which the testing build relies on when it shortens the interval.
const (
	DefaultPollInterval = 250 * time.Millisecond
	QuitPolls           = 80
	LaunchPolls         = 60
)

// quitScript asks the app, by bundle id, to quit the way its own menu
// would.
const quitScript = `tell application id "` + BundleID + `" to quit`

// maxInfoPlist bounds the read of the bundle's Info.plist.
const maxInfoPlist = 64 << 10

// ErrUnsupportedPlatform reports that the desktop switch exists only on
// macOS.
var ErrUnsupportedPlatform = errors.New("the desktop app switch is supported only on macOS")

// ErrAppNotFound reports a missing main executable at the fixed path.
var ErrAppNotFound = errors.New("the Claude desktop app is not installed at " + AppBundle)

// ErrBundleMismatch reports an app at the fixed path with another bundle
// identifier.
var ErrBundleMismatch = errors.New("the app at " + AppBundle + " is not the Claude desktop app")

// System is everything the desktop feature touches outside its own files:
// the process observer, the two programs it runs and the installed bundle.
type System struct {
	// Observer takes process snapshots.
	Observer Observer
	// Osascript is the absolute path of the AppleScript runner.
	Osascript string
	// Open is the absolute path of the launcher.
	Open string
	// Bundle is the on-disk app bundle that is checked and launched.
	Bundle string
	// PollInterval is the pause between two observations.
	PollInterval time.Duration
	// QuitPolls and LaunchPolls bound the two waits.
	QuitPolls, LaunchPolls int
	// DeviceOf returns the device id of path.
	DeviceOf func(path string) (uint64, error)
	// Log receives one line per lifecycle step.
	Log *slog.Logger

	// checked is the bundle directory's (device, inode) as CheckBundle
	// saw it; Launch refuses a bundle that is no longer that directory.
	checked *bundleID
}

// bundleID is a directory's identity on disk.
type bundleID struct{ dev, ino uint64 }

// bundleDir confirms the bundle path is a real directory, never a link,
// and returns its identity.
func (s *System) bundleDir() (bundleID, error) {
	var st unix.Stat_t
	if err := unix.Lstat(s.Bundle, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return bundleID{}, ErrAppNotFound
		}
		return bundleID{}, fmt.Errorf("checking the app: %w", &fs.PathError{Op: "lstat", Path: s.Bundle, Err: err})
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return bundleID{dev: uint64(st.Dev), ino: st.Ino}, nil
	case unix.S_IFLNK:
		// A link could point the launch at any program; only the
		// directory installed at the fixed path is the app.
		return bundleID{}, fmt.Errorf("%w (%s is a symbolic link)", ErrBundleMismatch, s.Bundle)
	default:
		return bundleID{}, ErrAppNotFound
	}
}

// testWorld replaces the system boundary in the testing build; the
// release build never sets it.
var testWorld func(dataDir string) (*System, error)

// Default returns the system boundary for the app whose data directory is
// dataDir. Elsewhere than macOS it refuses before reading anything.
func Default(dataDir string) (*System, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrUnsupportedPlatform
	}
	if testWorld != nil {
		return testWorld(dataDir)
	}
	return platformDefault(dataDir)
}

func (s *System) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// CheckBundle proves the fixed install is the app: its main executable is
// a regular file and its Info.plist names the expected bundle identifier.
func (s *System) CheckBundle() error {
	id, err := s.bundleDir()
	if err != nil {
		return err
	}
	info, err := os.Stat(s.Bundle + "/Contents/MacOS/Claude")
	if errors.Is(err, fs.ErrNotExist) {
		return ErrAppNotFound
	}
	if err != nil {
		return fmt.Errorf("checking the app: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ErrAppNotFound
	}
	file, err := os.Open(s.Bundle + "/Contents/Info.plist")
	if err != nil {
		return fmt.Errorf("%w: reading Info.plist: %v", ErrBundleMismatch, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxInfoPlist+1))
	if err != nil || len(data) > maxInfoPlist {
		return fmt.Errorf("%w: Info.plist is unreadable or larger than %d bytes", ErrBundleMismatch, maxInfoPlist)
	}
	var meta struct {
		BundleID string `plist:"CFBundleIdentifier"`
	}
	if _, err := plist.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("%w: Info.plist: %v", ErrBundleMismatch, err)
	}
	if meta.BundleID != BundleID {
		return fmt.Errorf("%w (its identifier is %q)", ErrBundleMismatch, meta.BundleID)
	}
	s.checked = &id
	return nil
}

// OtherCopyError refuses an operation while another copy of the app runs:
// the quit request addresses the bundle id and could reach that copy.
type OtherCopyError struct{ Procs []Proc }

// Error names the first other copy.
func (e *OtherCopyError) Error() string {
	p := e.Procs[0]
	return fmt.Sprintf("another copy of the app is running (pid %d, %s)", p.PID, p.Path)
}

// QuitRefusedError reports that the quit request itself failed.
type QuitRefusedError struct{ Err error }

// Error includes the runner's failure.
func (e *QuitRefusedError) Error() string { return "the app refused to quit: " + e.Err.Error() }

// Unwrap returns the runner's failure.
func (e *QuitRefusedError) Unwrap() error { return e.Err }

// QuitTimeoutError reports a quit that did not finish within the budget.
type QuitTimeoutError struct {
	Mains, Family int
	Last          Snapshot
}

// Error names both counts.
func (e *QuitTimeoutError) Error() string {
	return fmt.Sprintf("the app did not finish quitting: %d main and %d other processes still run", e.Mains, e.Family)
}

// QuitReport says what the quit step did.
type QuitReport struct {
	// Skipped is set when nothing of the app ran, so no request was sent:
	// the AppleScript quit would start a stopped app first.
	Skipped bool
	// Before is the snapshot that decided the step.
	Before Snapshot
	// Started and Ended bound the request and the wait.
	Started, Ended time.Time
}

// Quit asks the app to quit and waits until no main and no counted
// process remain. It refuses while another copy runs, before and during
// the wait. Nothing is signalled or killed; a refusal or a timeout leaves
// the processes as they are.
func (s *System) Quit(ctx context.Context) (QuitReport, error) {
	snap, err := s.Observer.Snapshot(ctx)
	if err != nil {
		return QuitReport{}, err
	}
	report := QuitReport{Before: snap}
	if len(snap.OtherCopies) > 0 {
		return report, &OtherCopyError{Procs: snap.OtherCopies}
	}
	if snap.Quiet() {
		report.Skipped = true
		return report, nil
	}
	report.Started = time.Now()
	log := s.logger()
	log.Info("desktop quit requested", "mains", len(snap.Mains), "family", len(snap.Family))
	if err := s.run(ctx, s.Osascript, "-e", quitScript); err != nil {
		return report, &QuitRefusedError{Err: err}
	}
	last := snap
	for poll := range s.QuitPolls {
		if poll > 0 {
			if err := s.pause(ctx); err != nil {
				return report, err
			}
		}
		last, err = s.Observer.Snapshot(ctx)
		if err != nil {
			return report, err
		}
		if len(last.OtherCopies) > 0 {
			return report, &OtherCopyError{Procs: last.OtherCopies}
		}
		if last.Quiet() {
			report.Ended = time.Now()
			log.Info("desktop quit complete", "elapsed_ms", report.Ended.Sub(report.Started).Milliseconds())
			return report, nil
		}
	}
	report.Ended = time.Now()
	return report, &QuitTimeoutError{Mains: len(last.Mains), Family: len(last.Family), Last: last}
}

// LaunchReport says what the launch step observed.
type LaunchReport struct {
	// OpenErr is the launcher's failure, if any.
	OpenErr error
	// Ready is set when a new main appeared within the budget.
	Ready bool
	// Main is that new main.
	Main *Proc
	// ObserveErr is the last observation failure during the wait.
	ObserveErr error
	// Elapsed is the time from the request to the decision.
	Elapsed time.Duration
}

// Launch starts the fixed bundle and waits for a main executable that is
// not in before. Readiness is a process, never a file the app wrote. A
// bundle that fails CheckBundle, or is no longer the directory it saw, is
// not started.
func (s *System) Launch(ctx context.Context, before []Proc) LaunchReport {
	start := time.Now()
	log := s.logger()
	var report LaunchReport
	// The bundle is checked again right before it is started: a caller
	// that never checked it (repair) gets the full check here, and one that
	// did must still find the same real directory, so a bundle swapped
	// since the check is never launched.
	var err error
	if s.checked == nil {
		err = s.CheckBundle()
	} else if id, idErr := s.bundleDir(); idErr != nil {
		err = idErr
	} else if id != *s.checked {
		err = fmt.Errorf("%w (%s was replaced after it was checked)", ErrBundleMismatch, s.Bundle)
	}
	if err != nil {
		report.OpenErr = err
		report.Elapsed = time.Since(start)
		log.Warn("desktop launch refused", "error", err.Error())
		return report
	}
	if err := s.run(ctx, s.Open, "-n", s.Bundle); err != nil {
		report.OpenErr = err
		report.Elapsed = time.Since(start)
		log.Warn("desktop launch failed", "error", err.Error())
		return report
	}
	for range s.LaunchPolls {
		if err := s.pause(ctx); err != nil {
			report.ObserveErr = err
			break
		}
		snap, err := s.Observer.Snapshot(ctx)
		if err != nil {
			report.ObserveErr = err
			continue
		}
		if main := NewMain(snap, before); main != nil {
			report.Ready, report.Main = true, main
			break
		}
	}
	report.Elapsed = time.Since(start)
	log.Info("desktop launch observed", "ready", report.Ready, "elapsed_ms", report.Elapsed.Milliseconds())
	return report
}

// NewMain returns a main of snap whose (pid, start) is not in before.
func NewMain(snap Snapshot, before []Proc) *Proc {
	for _, m := range snap.Mains {
		known := false
		for _, b := range before {
			if b.PID == m.PID && b.Start.Equal(m.Start) {
				known = true
				break
			}
		}
		if !known {
			return &m
		}
	}
	return nil
}

func (s *System) pause(ctx context.Context) error {
	timer := time.NewTimer(s.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// run executes an absolute program with argv, no shell, and keeps a short
// tail of its error output for the message.
func (s *System) run(ctx context.Context, program string, args ...string) error {
	cmd := exec.CommandContext(ctx, program, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{buf: &stderr, max: 1024}
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", program, err, msg)
		}
		return fmt.Errorf("%s: %w", program, err)
	}
	return nil
}

type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
