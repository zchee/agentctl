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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/secret"
)

// DoctorCanRemoveStale reports whether peer visibility permits explicit lock recovery.
const DoctorCanRemoveStale = runtime.GOOS == "darwin"

// DoctorStaleRemovalUnsupported explains why no removal is attempted on other platforms.
const DoctorStaleRemovalUnsupported = "stale lock removal unsupported on this platform: peer visibility is unproved"

// RemoveStale removes only an empty, old lock with an unchanged heartbeat and a permitted path.
// Explicit confirmation is required even on an interactive terminal.
func (d *Doctor) RemoveStale(ctx context.Context, path string, yes bool) error {
	if !DoctorCanRemoveStale {
		return errs.NewConfig(DoctorStaleRemovalUnsupported)
	}
	root, record, err := d.removalPermit(ctx, path)
	if err != nil {
		return err
	}
	if filepath.Dir(path) == d.Paths.LocksDir() {
		return errs.NewConfig(fmt.Sprintf("`%s` is one of agentctl's own namespace locks. Those are never unlinked: `flock` locks an inode, and a recreated lock file is a second inode two processes could hold at once", path))
	}
	name := filepath.Base(path)
	if name == secret.LegacyStorageWriteArtefact {
		return errs.NewConfig(fmt.Sprintf("`%s` is a legacy agentctl artefact, not a Claude Code mutex; removal and migration are unsupported", path))
	}
	if name != secret.RefreshLockName && name != secret.StorageWriteLockName && (len(name) <= len(".lock") || !strings.HasSuffix(name, ".lock")) {
		return errs.NewConfig(fmt.Sprintf("`%s` is not a Claude Code lock artefact; only `%s`, `%s` and a legacy `<namespace>.lock` can be removed", path, secret.RefreshLockName, secret.StorageWriteLockName))
	}
	meta, err := os.Lstat(path)
	if err != nil {
		return errs.NewConfig(fmt.Sprintf("`%s` cannot be examined: %s", path, err))
	}
	if meta.Mode()&os.ModeSymlink != 0 {
		return errs.NewConfig(fmt.Sprintf("`%s` is a symbolic link; agentctl will not delete through one", path))
	}
	if !meta.IsDir() {
		if meta.Mode().IsRegular() {
			return errs.NewConfig(fmt.Sprintf("`%s` is %s: every Claude Code lock artefact is a directory made by `mkdir`, so a regular file at that name was written by something else and agentctl will not remove it", path, doctorAnomalousFile))
		}
		return errs.NewConfig(fmt.Sprintf("`%s` is not a directory, and every Claude Code lock artefact is one", path))
	}
	first, ok := doctorSample(path)
	if !ok {
		return errs.NewConfig(fmt.Sprintf("`%s` went away while it was being examined", path))
	}
	if first.age < DoctorStaleMinAge {
		return errs.NewConfig(fmt.Sprintf("`%s` was written %ds ago, less than the %ds staleness threshold: a session that is starting up looks exactly like this", path, int64(first.age/time.Second), int64(DoctorStaleMinAge/time.Second)))
	}
	if record != "" {
		if err := tell(d.Out, fmt.Sprintf("`%s` is outside `%s`. The held-lock record `%s` names it and the agentctl process that wrote it is gone — that record is the only reason this removal is allowed.", path, d.Paths.NamespaceRoot(), record)); err != nil {
			return err
		}
	}
	if err := tell(d.Out, fmt.Sprintf("About to remove `%s`.\nThis is Claude Code's lock, not agentctl's. If a session is holding it and its heartbeat is merely slow, removing it lets two processes write that store at once, which ends with one of them holding a refresh token the server has already rotated — and a login lost.\nChecking for a heartbeat: two samples %ds apart.", path, int64(d.interval()/time.Second))); err != nil {
		return err
	}
	if !yes {
		return errs.NewConfig(fmt.Sprintf("`--yes` is required to remove `%s`; nothing was removed", path))
	}
	if doctorHolderAlive(ctx, first, d.interval()) {
		return errs.NewConfig(fmt.Sprintf("`%s` was rewritten between the two samples, so something is holding it", path))
	}
	if err := secret.RemoveDirUnder(root, path); err != nil {
		if refused, ok := errors.AsType[*secret.SymlinkRefusedError](err); ok {
			return errs.NewConfig(fmt.Sprintf("`%s` is reached through a symbolic link; agentctl will not delete through one", refused.Path))
		}
		if outside, ok := errors.AsType[*secret.OutsideRootError](err); ok {
			return errs.NewConfig(fmt.Sprintf("`%s` does not resolve to a location inside `%s`", outside.Path, root))
		}
		if occupied, ok := errors.AsType[*secret.NotEmptyError](err); ok {
			return errs.NewConfig(fmt.Sprintf("`%s` has something in it, so it is not the empty directory a lapsed lock leaves behind; agentctl removes one directory and never a tree", occupied.Path))
		}
		if changed, ok := errors.AsType[*secret.NotRegularError](err); ok {
			return errs.NewConfig(fmt.Sprintf("`%s` is no longer the directory it was a moment ago", changed.Path))
		}
		return errs.NewIO(fmt.Sprintf("could not remove `%s`", path), err)
	}
	return tell(d.Out, fmt.Sprintf("Removed `%s`.", path))
}

func (d *Doctor) removalPermit(ctx context.Context, path string) (string, string, error) {
	if d.Paths.IsUnderNamespaceRoot(path) {
		return d.Paths.NamespaceRoot(), "", nil
	}
	var first *secret.HeldLockFile
	for _, held := range secret.ReadAllHeldLockRecords(d.Paths) {
		if !held.Record.Attests(path) {
			continue
		}
		if first == nil {
			first = new(held)
		}
		if held.Record.WriterIsGone(ctx) {
			if anchor, ok := held.Record.Anchor(); ok {
				return anchor, held.File, nil
			}
		}
	}
	if first != nil {
		return "", "", errs.NewConfig(fmt.Sprintf("`%s` is named by the held-lock record `%s`, but agentctl process %d is %s — that lock is being held, not leaked", path, first.File, first.Record.WriterPID, holderLabel(ctx, first.Record.WriterPID)))
	}
	return "", "", errs.NewConfig(fmt.Sprintf("`%s` is not inside `%s`; agentctl removes lock artefacts only inside its own namespace root", path, d.Paths.NamespaceRoot()))
}
