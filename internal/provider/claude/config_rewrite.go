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

package claude

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/secret"
)

// ConfigCatchUpCheck retains a read-only preflight, without open descriptors.
type ConfigCatchUpCheck struct {
	path   string
	report ConfigReport
}

// CheckConfigCatchUp reads and validates the file without creating anything.
// A non-nil report ends the step; otherwise the check may be written after consent.
func CheckConfigCatchUp(env *EnvView, profile *Profile) (*ConfigCatchUpCheck, *ConfigReport) {
	return configPreflight(env, profile, true)
}

// RewriteConfig rewrites the live account while preserving the literal symlink.
// Callers hold no peer locks and have already obtained the account profile.
func RewriteConfig(ctx context.Context, env *EnvView, profile *Profile, seams *secret.Seams) ConfigReport {
	check, stopped := configPreflight(env, profile, false)
	if stopped != nil {
		return *stopped
	}
	return configResolveWrite(ctx, check, configWriteInput{env: env, profile: profile, seams: seams}, configHoldHooks{})
}

// WriteConfigCatchUp performs the confirmed check using the normal locked write.
// It rechecks the account under the lock in case a session has caught up first.
func WriteConfigCatchUp(ctx context.Context, check *ConfigCatchUpCheck, env *EnvView, profile *Profile, seams *secret.Seams) ConfigReport {
	return configResolveWrite(ctx, check, configWriteInput{env: env, profile: profile, seams: seams, catchUp: true}, configHoldHooks{})
}

func configPreflight(env *EnvView, profile *Profile, catchUp bool) (*ConfigCatchUpCheck, *ConfigReport) {
	report := ConfigReport{Outcome: secret.ConfigApplied, Account: &secret.IncomingIdentity{AccountUUID: profile.AccountUUID, OrganizationUUID: new(profile.OrganizationUUID)}}
	path := GlobalConfigPath(env)
	read, err := secret.ReadFileFollowing(path, MaxClaudeJSONBytes)
	if err != nil {
		return nil, configStopped(report, secret.ConfigRefused, secret.ConfigReasonUnreadable)
	}
	if !read.Present {
		return nil, configStopped(report, secret.ConfigSkipped, secret.ConfigReasonAbsent)
	}
	report.FromSHA8 = configSHA8(read.Bytes)
	// On the read-only catch-up path the identity check precedes reproduction.
	if catchUp && configAlreadyCurrent(read.Bytes, report.Account) {
		return nil, configStopped(report, secret.ConfigSkipped, secret.ConfigReasonAlreadyCurrent)
	}
	if err := Reproduce(read.Bytes); err != nil {
		return nil, configStopped(report, secret.ConfigRefused, configParseReason(err))
	}
	return &ConfigCatchUpCheck{path: path, report: report}, nil
}

func configStopped(report ConfigReport, outcome secret.ConfigOutcome, reason secret.ConfigReason) *ConfigReport {
	report.Outcome, report.Reason = outcome, &reason
	return &report
}

func configParseReason(err error) secret.ConfigReason {
	switch {
	case errors.Is(err, ErrNotAnObject):
		return secret.ConfigReasonNotAnObject
	case errors.Is(err, ErrNotReproducible):
		return secret.ConfigReasonNotReproducible
	default:
		return secret.ConfigReasonUnparseable
	}
}

type configWriteInput struct {
	env     *EnvView
	profile *Profile
	seams   *secret.Seams
	catchUp bool
}
type configPrepared struct {
	path, target string
	dir, backups int
	account      jsontext.Value
	report       ConfigReport
	catchUp      bool
}
type configHoldHooks struct {
	afterTemp  func()
	beforeTerm func(int)
}

func configResolveWrite(ctx context.Context, check *ConfigCatchUpCheck, input configWriteInput, hooks configHoldHooks) ConfigReport {
	report := check.report
	target, err := filepath.EvalSymlinks(check.path)
	if err != nil {
		return *configStopped(report, secret.ConfigRefused, secret.ConfigReasonUnreadable)
	}
	dir, err := unix.Open(filepath.Dir(target), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return *configStopped(report, secret.ConfigRefused, secret.ConfigReasonUnreadable)
	}
	defer unix.Close(dir) //nolint:errcheck // Closing a read-only directory has no recoverable failure.
	backups, err := configOpenBackups(BackupsDir(input.env))
	if err != nil {
		return *configStopped(report, secret.ConfigRefused, secret.ConfigReasonBackupUnwritable)
	}
	defer unix.Close(backups) //nolint:errcheck // Closing a read-only directory has no recoverable failure.
	seams := input.seams
	if seams == nil {
		seams = secret.RealSeams(secret.SystemClock())
	}
	p := configPrepared{path: check.path, target: filepath.Base(target), dir: dir, backups: backups, account: BuildOAuthAccount(input.profile, seams.Clock.Wall().UnixMilli()), report: report, catchUp: input.catchUp}
	hold, err := secret.AcquireConfigLock(ctx, check.path, seams)
	if err != nil {
		outcome, reason := secret.ConfigFailed, secret.ConfigReasonIO
		if lockErr, ok := errors.AsType[*secret.ConfigLockError](err); ok {
			switch lockErr.Kind {
			case secret.ConfigLockBusy:
				outcome, reason = secret.ConfigSkipped, secret.ConfigReasonLockBusy
			case secret.ConfigLockStale:
				outcome, reason = secret.ConfigSkipped, secret.ConfigReasonLockStale
			case secret.ConfigLockCancelled:
				outcome, reason = secret.ConfigSkipped, secret.ConfigReasonCancelled
			case secret.ConfigLockCompromised:
				outcome, reason = secret.ConfigAborted, secret.ConfigReasonCompromised
			}
		}
		return *configStopped(report, outcome, reason)
	}
	defer hold.Release()
	report = configUnderLock(&p, hold, seams.Clock, hooks)
	elapsed := hold.Elapsed()
	report.HoldMS = new(uint64(elapsed.Milliseconds()))
	if report.Outcome == secret.ConfigApplied && elapsed > secret.ConfigHoldBudget {
		slog.Warn("the configuration hold outlasted its budget after the rename", "hold_ms", elapsed.Milliseconds(), "budget_ms", secret.ConfigHoldBudget.Milliseconds())
	}
	return report
}

func configOpenBackups(path string) (int, error) {
	parent, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(parent) //nolint:errcheck // The opened leaf owns its descriptor independently.
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(parent, filepath.Base(path), flags, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(parent, filepath.Base(path), 0o700); err != nil {
			return -1, err
		}
		return unix.Openat(parent, filepath.Base(path), flags, 0)
	}
	return fd, err
}

func configUnderLock(p *configPrepared, hold *secret.ConfigHold, clock secret.Clock, hooks configHoldHooks) ConfigReport {
	report := p.report
	gate := func(term int) bool {
		if hooks.beforeTerm != nil {
			hooks.beforeTerm(term)
		}
		budgets := [...]time.Duration{0, 150, 200, 250, 300, 150, 100}
		deadlines := [...]time.Duration{50, 200, 400, 650, 950, 1100, 1200}
		if hold.Elapsed()+budgets[term]*time.Millisecond > deadlines[term]*time.Millisecond {
			report = *configStopped(report, secret.ConfigAborted, secret.ConfigReasonBudget)
			return false
		}
		return true
	}
	if !gate(0) || !gate(1) {
		return report
	}
	before, mode, err := configReadTarget(p)
	if err != nil {
		return *configStopped(report, secret.ConfigRefused, secret.ConfigReasonUnreadable)
	}
	report.FromSHA8 = configSHA8(before)
	if !gate(2) {
		return report
	}
	rewrite, err := Plan(before, p.account)
	if err != nil {
		return *configStopped(report, secret.ConfigRefused, configParseReason(err))
	}
	if p.catchUp && configAlreadyCurrent(before, report.Account) {
		return *configStopped(report, secret.ConfigSkipped, secret.ConfigReasonAlreadyCurrent)
	}
	if !gate(3) {
		return report
	}
	backup, err := configWriteBackup(p, before, clock.Wall().UnixMilli())
	if err != nil {
		return *configStopped(report, secret.ConfigRefused, secret.ConfigReasonBackupUnwritable)
	}
	report.Backup = &backup
	if !gate(4) {
		return report
	}
	var random [6]byte
	_, _ = rand.Read(random[:])
	temp := fmt.Sprintf("%s.tmp.%d.%x", p.target, os.Getpid(), random)
	if err := hold.TrackTemp(p.dir, temp); err != nil {
		return *configStopped(report, secret.ConfigFailed, secret.ConfigReasonIO)
	}
	created, err := configWriteTemp(p.dir, temp, rewrite.Updated(), mode)
	if !created {
		hold.UntrackTemp()
	}
	if err != nil {
		return *configStopped(report, secret.ConfigFailed, secret.ConfigReasonIO)
	}
	if hooks.afterTemp != nil {
		hooks.afterTemp()
	}
	if !gate(5) {
		return report
	}
	current, _, err := configReadTarget(p)
	if err != nil {
		return *configStopped(report, secret.ConfigAborted, secret.ConfigReasonChangedUnderLock)
	}
	if _, err := rewrite.Apply(current); err != nil {
		return *configStopped(report, secret.ConfigAborted, secret.ConfigReasonChangedUnderLock)
	}
	if hold.DriftCheck() != nil {
		return *configStopped(report, secret.ConfigAborted, secret.ConfigReasonCompromised)
	}
	if !gate(6) {
		return report
	}
	if unix.Renameat(p.dir, temp, p.dir, p.target) != nil {
		return *configStopped(report, secret.ConfigFailed, secret.ConfigReasonIO)
	}
	hold.UntrackTemp()
	if unix.Fsync(p.dir) != nil {
		slog.Warn("the configuration file's directory could not be flushed after the rename")
	}
	report.Outcome, report.Reason, report.ToSHA8 = secret.ConfigApplied, nil, configSHA8(rewrite.Updated())
	return report
}

func configReadTarget(p *configPrepared) ([]byte, uint32, error) {
	fd, err := unix.Openat(p.dir, p.target, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(fd), p.target)
	defer file.Close() //nolint:errcheck // Read-only descriptor.
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 0 || stat.Size > MaxClaudeJSONBytes {
		return nil, 0, unix.EINVAL
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxClaudeJSONBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > MaxClaudeJSONBytes {
		return nil, 0, unix.EFBIG
	}
	return data, uint32(stat.Mode) & 0o7777, nil
}

func configWriteBackup(p *configPrepared, before []byte, stamp int64) (string, error) {
	for offset := range 2 {
		name := fmt.Sprintf("%s.backup.%d", filepath.Base(p.path), stamp+int64(offset))
		fd, err := unix.Openat(p.backups, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		file := os.NewFile(uintptr(fd), name)
		_, err = file.Write(before)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		return name, nil
	}
	return "", unix.EEXIST
}

func configWriteTemp(dir int, name string, after []byte, mode uint32) (bool, error) {
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close() //nolint:errcheck // Sync reports persistence failures before rename.
	if _, err := file.Write(after); err != nil {
		return true, err
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return true, err
	}
	return true, file.Sync()
}

func configSHA8(data []byte) *string {
	digest := sha256.Sum256(data)
	return new(fmt.Sprintf("%x", digest[:4]))
}
