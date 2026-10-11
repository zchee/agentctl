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
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/lockfile"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
	"github.com/zchee/agentctl/internal/runtime/fault"
)

// DesktopExitRefused is the exit status of every desktop refusal and of
// every desktop outcome that did not end as asked. The desktop commands are
// separate from the swap, so sharing the number with the swap's follow-up
// refusal never makes one run ambiguous.
const DesktopExitRefused = 30

// The refusal words of the desktop commands. Each is printed and carried in
// the JSON report, so a script can branch on it.
const (
	desktopUnsupportedPlatform = "desktop_unsupported_platform"
	desktopAppNotFound         = "desktop_app_not_found"
	desktopBundleMismatch      = "desktop_bundle_mismatch"
	desktopLockBusy            = "desktop_lock_busy"
	desktopOtherCopyRunning    = "desktop_other_copy_running"
	desktopIntentOpen          = "desktop_intent_open"
	desktopCrossDevice         = "desktop_cross_device"
	desktopProfileUnreadable   = "desktop_profile_unreadable"
	desktopProfileMismatch     = "desktop_profile_mismatch"
	desktopJournalFailed       = "desktop_journal_failed"
	desktopQuitRefused         = "desktop_quit_refused"
	desktopQuitTimeout         = "desktop_quit_timeout"
	desktopRepairInconsistent  = "desktop_repair_inconsistent"
	desktopObserveFailed       = "desktop_observe_failed"
	desktopAppRunning          = "desktop_app_running"
	desktopProfileInPlace      = "desktop_profile_in_place"
)

// desktopRefusal is a refusal with its word. It unwraps to the refusal
// that carries the exit status, so the status is decided in one place.
type desktopRefusal struct {
	Word   string
	Detail string
}

// Error names the word and why.
func (e *desktopRefusal) Error() string { return "refused: " + e.Word + ": " + e.Detail }

// Unwrap returns the exit-status carrier.
func (e *desktopRefusal) Unwrap() error {
	return errs.NewRefused(DesktopExitRefused, e.Word+": "+e.Detail)
}

func refuse(word, format string, args ...any) error {
	return &desktopRefusal{Word: word, Detail: fmt.Sprintf(format, args...)}
}

// DesktopProcess runs the desktop commands.
type DesktopProcess struct {
	// In is the terminal the confirmation is read from.
	In *os.File
	// Out receives the report.
	Out io.Writer
	// System returns the process observer and the app's programs.
	System func(dataDir string) (*rdesktop.System, error)
	// LockWait bounds the wait for the per-user desktop lock.
	LockWait time.Duration
}

// NewDesktopProcess returns the process wired to the real system.
func NewDesktopProcess(in *os.File, out io.Writer) DesktopProcess {
	return DesktopProcess{In: in, Out: out, System: rdesktop.Default, LockWait: time.Second}
}

// desktopEnv is one command's view of the desktop store and the system.
type desktopEnv struct {
	layout desktop.Layout
	sys    *rdesktop.System
	out    io.Writer
	prompt Prompter
	log    *slog.Logger
	fault  fault.Fault
}

// newEnv resolves the layout from HOME and the system boundary. Off macOS
// it refuses before anything is read.
func (p DesktopProcess) newEnv() (*desktopEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errs.NewConfig("the desktop switch needs HOME: " + err.Error())
	}
	if !filepath.IsAbs(home) {
		return nil, errs.NewConfig(fmt.Sprintf("HOME `%s` is not an absolute path", home))
	}
	layout := desktop.Paths(home)
	sys, err := p.System(layout.DefaultData)
	if errors.Is(err, rdesktop.ErrUnsupportedPlatform) {
		return nil, refuse(desktopUnsupportedPlatform, "%v", err)
	}
	if err != nil {
		return nil, errs.NewConfig("the desktop switch cannot reach the system: " + err.Error())
	}
	log := slog.Default()
	sys.Log = log
	return &desktopEnv{
		layout: layout, sys: sys, out: p.Out, log: log, fault: fault.Active(),
		prompt: TerminalPrompt{In: p.In, Out: p.Out},
	}, nil
}

// lock takes the per-user desktop lock, which serializes every desktop
// mutation whichever agentctl store the caller uses.
func (e *desktopEnv) lock(ctx context.Context, wait time.Duration) (*lockfile.LockGuard, error) {
	guard, err := lockfile.LockIn(ctx, e.layout.Root, desktop.LockName, time.Now().Add(wait))
	if errors.Is(err, lockfile.ErrLockBusy) {
		return nil, refuse(desktopLockBusy, "another desktop operation holds %s", e.layout.Lock)
	}
	if err != nil {
		return nil, errs.NewIO("could not take the desktop lock", err)
	}
	return guard, nil
}

// checkLayout proves the data path and the store contain no symbolic link
// in their chains and creates the store when missing.
func (e *desktopEnv) checkLayout() error {
	if err := desktop.ParentChainHasNoSymlink(e.layout.DefaultData); err != nil {
		return errs.NewConfig("the desktop data path is not usable: " + err.Error())
	}
	if err := e.layout.EnsureStore(); err != nil {
		return errs.NewConfig("the desktop store is not usable: " + err.Error())
	}
	return nil
}

// checkBundle maps the installed-app check onto its refusal words.
func (e *desktopEnv) checkBundle() error {
	err := e.sys.CheckBundle()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rdesktop.ErrAppNotFound):
		return refuse(desktopAppNotFound, "%v", err)
	case errors.Is(err, rdesktop.ErrBundleMismatch):
		return refuse(desktopBundleMismatch, "%v", err)
	default:
		return errs.NewIO("could not check the installed app", err)
	}
}

// sameDevice refuses when the data path's parent and the profiles
// directory are on two file systems: a rename cannot cross them, and the
// switch never copies a profile.
func (e *desktopEnv) sameDevice() error {
	parent := filepath.Dir(e.layout.DefaultData)
	a, err := e.sys.DeviceOf(parent)
	if err != nil {
		return errs.NewIO("could not read the device of "+parent, err)
	}
	b, err := e.sys.DeviceOf(e.layout.ProfilesDir)
	if err != nil {
		return errs.NewIO("could not read the device of "+e.layout.ProfilesDir, err)
	}
	if a != b {
		return refuse(desktopCrossDevice, "%s (device %d) and %s (device %d) are on different file systems", parent, a, e.layout.ProfilesDir, b)
	}
	return nil
}

// appendRecord writes one journal record, or fails it the way a full disk
// would when the caller's injected fault point is active.
func (e *desktopEnv) appendRecord(record any, injectedFailure bool) error {
	if injectedFailure {
		return errs.NewIO("could not append to the desktop journal", errors.New("no space left on device (injected)"))
	}
	return desktop.Append(e.layout.Journal, record)
}

// crashPoint ends the process when the caller's injected fault point is
// active, the way a crash or a kill would, so recovery from every
// intermediate layout can be tested.
func crashPoint(active bool) {
	if active {
		os.Exit(134)
	}
}

// resolveAccount maps an agentctl account id to its account UUID. An
// unknown id keeps the registry's wording and exits 2.
func resolveAccount(ctx context.Context, globals cli.Globals, id string) (string, error) {
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return "", err
	}
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return "", err
	}
	rec, err := registry.ResolveID(id)
	if err != nil {
		if cfg, ok := errors.AsType[*errs.ConfigError](err); ok {
			return "", errs.NewRefused(0, cfg.Error())
		}
		return "", err
	}
	if !desktop.ValidUUID(rec.AccountUUID) {
		return "", errs.NewConfig(fmt.Sprintf("account `%s` has no account UUID the desktop app could record", id))
	}
	return desktop.AccountName(rec.AccountUUID), nil
}

// registeredAccounts returns the account UUIDs agentctl knows, in lower
// case. A store that cannot be read yields an empty set and the error.
func registeredAccounts(ctx context.Context, globals cli.Globals) (map[string]bool, error) {
	known := map[string]bool{}
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return known, err
	}
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return known, err
	}
	for _, rec := range registry.Accounts {
		known[desktop.AccountName(rec.AccountUUID)] = true
	}
	return known, nil
}

// writeJSON emits one indented document with a trailing newline.
func writeJSON(w io.Writer, doc any) error {
	data, err := json.Marshal(doc, jsontext.WithIndent("  "), jsontext.EscapeForHTML(false), json.Deterministic(true))
	if err != nil {
		return errs.NewIO("could not encode the report", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return errs.NewIO("could not write the report", err)
	}
	return nil
}

// uuidPrefix shortens a UUID for a table cell.
func uuidPrefix(uuid string) string {
	if len(uuid) > 8 {
		return uuid[:8] + "…"
	}
	return uuid
}
