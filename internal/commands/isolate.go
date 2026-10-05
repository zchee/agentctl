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
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// SessionOptions selects an isolated directory and which live resources it shares.
type SessionOptions struct {
	ConfigDir    string
	FreshContext bool
	NoMCP        bool
}

// SessionDir describes a prepared session, without carrying any credentials.
type SessionDir struct {
	Path          string
	MCPConfig     string
	Linked        []string
	Missing       []string
	AlreadyLinked []string
}

// SessionTierOne lists resources shared regardless of conversation history.
func SessionTierOne() []string { return []string{"settings.json", "CLAUDE.md", "skills"} }

// SessionTierTwo lists history directories omitted for a fresh context.
func SessionTierTwo() []string {
	return []string{"projects", "shell-snapshots", "file-history", "sessions", "session-env"}
}

// NeverLinked lists live files deliberately excluded from session sharing.
func NeverLinked() []string { return []string{"history.jsonl"} }

// NeverSeeded lists account-scoped keys excluded from session configuration.
func NeverSeeded() []string {
	return []string{"oauthAccount", "userID", "machineID", "cachedUsageUtilization", "overageCreditGrantCache", "passesEligibilityCache", "s1mAccessCache", "s1mNonSubscriberAccessCache", "customApiKeyResponses", "mcpServers", "projects", "modelAccessCache", "orgModelDefaultCache", "cachedExtraUsageDisabledReason", "additionalModelOptionsCache", "additionalModelOptionsAnsweredAt", "additionalModelCostsCache", "autoCompactWindowsCache", "clientDataCacheSlots"}
}

// SessionSeedKeys lists the account-independent preferences copied into a new session.
func SessionSeedKeys() []string {
	return []string{"hasCompletedOnboarding", "lastOnboardingVersion", "lastReleaseNotesSeen", "hasSeenAutoDefaultNotice", "hasCompletedClaudeInChromeOnboarding", "theme", "preferredNotifChannel", "editorMode", "autoUpdates", "autoUpdatesProtectedForNative", "installMethod", "bypassPermissionsModeAccepted", "hasAcknowledgedCostThreshold", "shiftEnterKeyBindingInstalled", "verbose"}
}

// EnsureSession creates an owned account's session, refusing foreign occupants.
// Existing correct links and regular seed files are left unchanged. The live
// configuration is read under its lock, or twice when the lock is unavailable.
func EnsureSession(ctx context.Context, paths *config.Paths, rec *config.AccountRecord, opts SessionOptions, env *claude.EnvView) (SessionDir, error) {
	if rec.Kind.Owned == nil {
		return SessionDir{}, errs.NewConfig(fmt.Sprintf("only an account agentctl owns can be isolated into a session; `%s` is `%s`, whose credentials live outside agentctl's own store", rec.AccountUUID, rec.Kind.Name()))
	}
	path, err := sessionPath(paths, rec, opts.ConfigDir, env)
	if err != nil {
		return SessionDir{}, err
	}
	if err := createSessionDir(ctx, path); err != nil {
		return SessionDir{}, err
	}
	session := SessionDir{Path: path, Linked: []string{}, Missing: []string{}, AlreadyLinked: []string{}}
	names := SessionTierOne()
	if !opts.FreshContext {
		names = append(names, SessionTierTwo()...)
	}
	for i, name := range names {
		live := filepath.Join(claude.LiveStoreDir(env), name)
		target, err := claude.Canonical(live)
		if errors.Is(err, fs.ErrNotExist) {
			session.Missing = append(session.Missing, name)
			continue
		}
		if err != nil {
			return SessionDir{}, errs.NewIO(fmt.Sprintf("could not resolve `%s`", live), err)
		}
		if i >= len(SessionTierOne()) {
			info, err := os.Stat(target)
			if err != nil || !info.IsDir() {
				session.Missing = append(session.Missing, name)
				continue
			}
		}
		link := filepath.Join(path, name)
		created, err := sessionLink(ctx, link, target)
		if err != nil {
			return SessionDir{}, err
		}
		if created {
			session.Linked = append(session.Linked, name)
		} else {
			session.AlreadyLinked = append(session.AlreadyLinked, link)
		}
	}
	if !opts.NoMCP {
		live := claude.ClaudeJSONPath(env)
		target, err := claude.Canonical(live)
		if err != nil {
			return SessionDir{}, errs.NewIO(fmt.Sprintf("could not resolve `%s`", live), err)
		}
		session.MCPConfig = filepath.Join(path, "mcp.json")
		if _, err := sessionLink(ctx, session.MCPConfig, target); err != nil {
			return SessionDir{}, err
		}
	}
	if err := seedSession(ctx, path, env); err != nil {
		return SessionDir{}, err
	}
	return session, nil
}

func sessionPath(paths *config.Paths, rec *config.AccountRecord, override string, env *claude.EnvView) (string, error) {
	if override == "" {
		path := paths.SessionDir(rec.AccountUUID, rec.OrganizationUUID)
		if !paths.IsUnderSessionRoot(path) {
			return "", errs.NewConfig(fmt.Sprintf("`%s` is not under `%s`; refusing to use it as a session directory", path, paths.SessionRoot()))
		}
		return path, nil
	}
	if !filepath.IsAbs(override) {
		return "", errs.NewConfig(fmt.Sprintf("--claude-config-dir must be an absolute path; `%s` is not", override))
	}
	for part := range strings.SplitSeq(override, "/") {
		if part == "." || part == ".." {
			return "", errs.NewConfig(fmt.Sprintf("--claude-config-dir `%s` must be an absolute, normalized path", override))
		}
	}
	live := claude.LiveStoreDir(env)
	a, aErr := claude.Canonical(override)
	b, bErr := claude.Canonical(live)
	same := a == b
	if aErr != nil || bErr != nil {
		same = claude.ExportSpelling(filepath.Clean(override)) == claude.ExportSpelling(filepath.Clean(live))
	}
	if same {
		return "", errs.NewConfig(fmt.Sprintf("--claude-config-dir `%s` is the live Claude Code configuration directory; an isolated session cannot be the very thing it isolates from", override))
	}
	return override, nil
}

func createSessionDir(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return sessionOccupant(path, "plain directory agentctl would create")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return errs.NewIO(fmt.Sprintf("could not inspect `%s`", path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), config.DirMode); err != nil {
		return errs.NewIO(fmt.Sprintf("could not create `%s`", filepath.Dir(path)), err)
	}
	if err := os.Mkdir(path, config.DirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			info, statErr := os.Lstat(path)
			if statErr == nil && info.IsDir() {
				return nil
			}
			if statErr == nil {
				return sessionOccupant(path, "plain directory agentctl would create")
			}
		}
		return errs.NewIO(fmt.Sprintf("could not create the session directory `%s`", path), err)
	}
	return nil
}

func sessionOccupant(path, wanted string) error {
	return errs.NewConfig(fmt.Sprintf("`%s` already exists and is not the %s there; move or remove it before starting this session", path, wanted))
}

func sessionLink(ctx context.Context, link, target string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := os.Lstat(link)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		existing, err := os.Readlink(link)
		if err != nil {
			return false, errs.NewIO(fmt.Sprintf("could not read the existing symlink `%s`", link), err)
		}
		if existing == target {
			return false, nil
		}
		return false, errs.NewConfig(fmt.Sprintf("`%s` already exists and points at `%s`, not `%s`; agentctl will not replace a symlink it did not place there", link, existing, target))
	case err == nil:
		return false, sessionOccupant(link, "symlink agentctl would place")
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Symlink(target, link); err != nil {
			return false, errs.NewIO(fmt.Sprintf("could not create the symlink `%s`", link), err)
		}
		return true, nil
	default:
		return false, errs.NewIO(fmt.Sprintf("could not inspect `%s`", link), err)
	}
}

func seedSession(ctx context.Context, dir string, env *claude.EnvView) error {
	path := filepath.Join(dir, ".claude.json")
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode().IsRegular() {
			return nil
		}
		return sessionOccupant(path, "plain file agentctl would seed")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return errs.NewIO(fmt.Sprintf("could not inspect `%s`", path), err)
	}
	live := claude.GlobalConfigPath(env)
	data, err := readSessionSeed(ctx, live)
	if err != nil {
		return err
	}
	defer clear(data)
	values := map[string]jsontext.Value{}
	defer func() {
		for _, value := range values {
			clear(value)
		}
	}()
	if data != nil {
		if jsontext.Value(data).Kind() != '{' {
			return errs.NewConfig(fmt.Sprintf("`%s` is not a JSON object at its top level, so it cannot be seeded from", live))
		}
		if err := json.Unmarshal(data, &values); err != nil {
			return errs.NewConfig(fmt.Sprintf("`%s` is not valid JSON, so it cannot be seeded from", live))
		}
		if values == nil {
			return errs.NewConfig(fmt.Sprintf("`%s` is not a JSON object at its top level, so it cannot be seeded from", live))
		}
	}
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out, jsontext.WithIndent("  "), jsontext.EscapeForHTML(false))
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, key := range SessionSeedKeys() {
		value, ok := values[key]
		if !ok {
			continue
		}
		if err := enc.WriteToken(jsontext.String(key)); err != nil {
			return err
		}
		if err := enc.WriteValue(value); err != nil {
			return err
		}
	}
	if _, ok := values["hasCompletedOnboarding"]; !ok {
		if err := enc.WriteToken(jsontext.String("hasCompletedOnboarding")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.True); err != nil {
			return err
		}
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return err
	}
	rendered, err := claude.RenderConfig(out.Bytes())
	if err != nil {
		return errs.NewConfig("could not render the session seed as JSON")
	}
	return writeSessionSeed(ctx, path, rendered)
}

func readSessionSeed(ctx context.Context, path string) ([]byte, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	read := func() ([]byte, error) {
		result, err := secret.ReadFileFollowing(path, claude.MaxClaudeJSONBytes)
		if err != nil {
			return nil, errs.NewIO(fmt.Sprintf("could not read `%s`", path), err)
		}
		return result.Bytes, nil
	}
	hold, err := secret.TryConfigLock(ctx, path, secret.RealSeams(secret.SystemClock()))
	if err == nil {
		data, readErr := read()
		elapsed := hold.Elapsed()
		hold.Release()
		if elapsed > secret.ConfigHoldBudget {
			slog.Warn("the session seed's read outlasted the configuration lock's budget", "hold_ms", elapsed.Milliseconds())
		}
		return data, readErr
	}
	for attempt := range 4 {
		if attempt > 0 && ctx.Err() != nil {
			return nil, errs.NewRefused(0, "cancelled while re-reading the live `.claude.json` to seed a session")
		}
		first, err := read()
		if err != nil {
			return nil, err
		}
		second, err := read()
		same := bytes.Equal(first, second)
		clear(second)
		if err != nil {
			clear(first)
			return nil, err
		}
		if same {
			return first, nil
		}
		clear(first)
	}
	return nil, errs.NewConfig(fmt.Sprintf("`%s` changed between two reads, 4 times in a row; a live Claude Code session may be rewriting it continuously; refusing to seed a possibly torn copy — try again", path))
}

func writeSessionSeed(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, config.FileMode)
	if errors.Is(err, fs.ErrExist) {
		info, statErr := os.Lstat(path)
		if statErr == nil && info.Mode().IsRegular() {
			return nil
		}
		if statErr != nil {
			return errs.NewIO(fmt.Sprintf("could not inspect `%s`", path), statErr)
		}
		return sessionOccupant(path, "plain file agentctl would seed")
	}
	if err != nil {
		return errs.NewIO(fmt.Sprintf("could not create `%s`", path), err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return errs.NewIO(fmt.Sprintf("could not write `%s`", path), err)
	}
	return nil
}
