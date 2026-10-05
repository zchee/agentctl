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

// Package config derives every path agentctl is allowed to write to from
// one root, the configuration directory, and holds the account registry
// that lives at that root.
//
// The layout under the root is:
//
//	<config_dir>/                         0700
//	  config.json                         0600
//	  .config.lock                        0600, never unlinked
//	  claude/                             0700   NamespaceRoot
//	    .locks/                           0700   LocksDir
//	      <acct>.<org>.lock               0600, never unlinked
//	    <acct>/<org>/                     0700   NamespaceDir
//	      .credentials.json               0600
//	  claude-sessions/                    0700   SessionRoot
//	  cache/claude/                       0700   CacheDir
//
// A sibling tree holds Codex state; Claude-only operations never create it
// ([Paths.EnsureCodexDirs] is the only creator):
//
//	<config_dir>/
//	  codex/                              0700   CodexRoot
//	    .locks/                           0700   CodexLocksDir
//	      <user>+<acct>.lock              0600, never unlinked
//	      scratch.lock                    0600   CodexScratchLock
//	    .state/                           0700   CodexStateDir
//	      <user>+<acct>.refresh           0600   CodexRefreshStatePath
//	    .scratch/                         0700   CodexScratchRoot
//	    <user>/<acct>/                    0700   CodexNamespaceDir
//	      auth.json                       0600
//	  cache/codex/                        0700   CodexCacheDir
//
// The three dot-directories share the Codex namespace tree with <user>
// directories, which is why a Codex id may not begin with a dot
// ([ValidateCodexSegment]): an id of ".locks" would otherwise name the lock
// directory as a namespace.
//
// The lock files live outside the namespace directories on purpose: flock
// locks an inode, so a lock file inside a directory another tool may delete
// and recreate is a lock two processes can hold at once. Outside the
// namespace, never unlinked, the inode is stable.
package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zchee/agentctl/internal/errs"
)

// UnknownOrg is the organization placeholder used when a login could not
// name one; `accounts relocate` moves such a namespace afterwards.
const UnknownOrg = "_unknown-org"

// SessionRootName is the directory, under the configuration directory,
// where isolated sessions live.
const SessionRootName = "claude-sessions"

// DirMode is the mode of every directory agentctl creates.
const DirMode fs.FileMode = 0o700

// FileMode is the mode of every file agentctl creates.
const FileMode fs.FileMode = 0o600

// ConfigDirEnv is the environment form of --config-dir.
const ConfigDirEnv = "AGENTCTL_CONFIG_DIR"

// Paths is the resolved location of one agentctl store.
//
// Two stores with different roots are independent and have independent
// locks: logging the same account into both makes two holders of one
// refresh chain, which is documented rather than prevented.
type Paths struct {
	configDir string
}

// NewPaths builds a value around an already-chosen configuration directory,
// which is what a store-relative test needs; every command reaches its
// store through [Resolve] instead.
func NewPaths(configDir string) *Paths {
	return &Paths{configDir: configDir}
}

// Resolve resolves the configuration directory.
//
// Precedence: the --config-dir value when non-empty, then
// [ConfigDirEnv], then the XDG configuration directory — XDG_CONFIG_HOME
// when it holds an absolute path, $HOME/.config otherwise — with "agctl"
// appended. The store directory keeps that name for compatibility with
// existing stores. No symbolic link is resolved: a home directory reached
// through a link keeps its spelled path.
//
// It returns a [errs.ConfigError] when no home directory can be determined
// and no override was given; an override never consults the base directory.
func Resolve(cliOverride string) (*Paths, error) {
	if cliOverride != "" {
		return NewPaths(cliOverride), nil
	}
	if env, ok := os.LookupEnv(ConfigDirEnv); ok {
		return NewPaths(env), nil
	}
	if xdg, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok && filepath.IsAbs(xdg) {
		return NewPaths(filepath.Join(xdg, "agctl")), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errs.NewConfig(fmt.Sprintf("could not determine the XDG configuration directory (%v); pass --config-dir or set %s", err, ConfigDirEnv))
	}
	return NewPaths(filepath.Join(home, ".config", "agctl")), nil
}

// ConfigDir returns the root of this store.
func (p *Paths) ConfigDir() string {
	return p.configDir
}

// ConfigFile returns the account registry's path.
func (p *Paths) ConfigFile() string {
	return filepath.Join(p.configDir, "config.json")
}

// ConfigLock returns the lock guarding read-modify-write of the account
// registry. Created once and never unlinked.
func (p *Paths) ConfigLock() string {
	return filepath.Join(p.configDir, ".config.lock")
}

// NamespaceRoot returns the root under which every Claude namespace lives.
func (p *Paths) NamespaceRoot() string {
	return filepath.Join(p.configDir, "claude")
}

// NamespaceDir returns the namespace directory for one (account,
// organization) pair. Callers pass identifiers that have already gone
// through [ValidateSegment].
func (p *Paths) NamespaceDir(acct, org string) string {
	return filepath.Join(p.NamespaceRoot(), acct, org)
}

// LocksDir returns where the Claude namespace locks live — beside the
// namespaces, never inside one.
func (p *Paths) LocksDir() string {
	return filepath.Join(p.NamespaceRoot(), ".locks")
}

// LockPath returns the lock file for one Claude namespace. Created once and
// never unlinked.
func (p *Paths) LockPath(acct, org string) string {
	return filepath.Join(p.LocksDir(), acct+"."+org+".lock")
}

// CacheRoot returns the directory every provider's usage cache lives under.
func (p *Paths) CacheRoot() string {
	return filepath.Join(p.configDir, "cache")
}

// CacheDir returns where Claude usage responses are cached between passes.
func (p *Paths) CacheDir() string {
	return filepath.Join(p.CacheRoot(), "claude")
}

// CodexCacheDir returns where Codex usage responses are cached between
// passes.
func (p *Paths) CodexCacheDir() string {
	return filepath.Join(p.CacheRoot(), "codex")
}

// SessionRoot returns the root of every isolated session directory.
//
// Deliberately outside [Paths.NamespaceRoot]: namespace removal unlinks a
// fixed name list under a namespace directory and then climbs it, so a
// session's copied symlink or seeded configuration sitting inside a
// namespace would make that climb fail silently.
func (p *Paths) SessionRoot() string {
	return filepath.Join(p.configDir, SessionRootName)
}

// SessionDir returns the isolated session directory for one (account,
// organization) pair. Callers pass identifiers that have already gone
// through [ValidateSegment].
func (p *Paths) SessionDir(acct, org string) string {
	return filepath.Join(p.SessionRoot(), acct, org)
}

// IsUnderNamespaceRoot reports whether path spells a path strictly below
// [Paths.NamespaceRoot].
//
// The comparison is lexical: "." and ".." are folded out of both paths and
// no symbolic link is resolved. That guarantees the string a writer hands
// to rename begins with the namespace root and is not the root itself — a
// property of the path, which cannot change under the caller. It does not
// guarantee where the path leads: a symlinked component resolves somewhere
// else entirely, and that escape is closed by the descriptor-relative,
// no-follow traversal the leaf operations use. Both checks are required;
// neither replaces the other.
func (p *Paths) IsUnderNamespaceRoot(path string) bool {
	return isStrictlyUnder(p.NamespaceRoot(), path)
}

// IsUnderSessionRoot reports whether path spells a path strictly below
// [Paths.SessionRoot], lexically, exactly as [Paths.IsUnderNamespaceRoot]
// does.
func (p *Paths) IsUnderSessionRoot(path string) bool {
	return isStrictlyUnder(p.SessionRoot(), path)
}

// EnsureDirs creates the Claude store's directories, each level at mode
// 0700.
//
// A blanket recursive create would apply the process umask's default of
// 0755, so each level is created individually with an explicit mode.
// Levels that already exist are left as they are — including their modes,
// which doctor reports on rather than silently tightening. It never
// creates the Codex tree.
func (p *Paths) EnsureDirs(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, dir := range []string{p.configDir, p.NamespaceRoot(), p.LocksDir(), p.CacheRoot(), p.CacheDir()} {
		if err := createDirMode(dir); err != nil {
			return err
		}
	}
	return nil
}

// CodexRoot returns the root under which every Codex namespace, lock,
// refresh marker and login scratch home lives.
func (p *Paths) CodexRoot() string {
	return filepath.Join(p.configDir, "codex")
}

// CodexNamespaceDir returns the namespace directory for one (ChatGPT user,
// ChatGPT account) pair.
//
// Both ids arrive from a token's claims, so both are validated here rather
// than trusted, before any caller can create the path. It returns a
// [errs.ConfigError] when either id fails [ValidateCodexSegment].
func (p *Paths) CodexNamespaceDir(user, acct string) (string, error) {
	if err := validateCodexPair(user, acct); err != nil {
		return "", err
	}
	return filepath.Join(p.CodexRoot(), user, acct), nil
}

// CodexLocksDir returns where the Codex namespace locks live — beside the
// namespaces, never inside one, for the reason the Claude locks are.
func (p *Paths) CodexLocksDir() string {
	return filepath.Join(p.CodexRoot(), ".locks")
}

// CodexLockPath returns the lock file for one Codex namespace. Created once
// and never unlinked.
//
// The name is <user>+<acct>.lock: "+" is outside the id alphabet, so two
// different pairs cannot spell the same name — ("a.b", "c") and
// ("a", "b.c") would collide under a "." separator. It returns a
// [errs.ConfigError] when either id fails [ValidateCodexSegment].
func (p *Paths) CodexLockPath(user, acct string) (string, error) {
	if err := validateCodexPair(user, acct); err != nil {
		return "", err
	}
	return filepath.Join(p.CodexLocksDir(), user+"+"+acct+".lock"), nil
}

// CodexScratchRoot returns where login scratch homes are created, one per
// login.
func (p *Paths) CodexScratchRoot() string {
	return filepath.Join(p.CodexRoot(), ".scratch")
}

// CodexScratchLock returns the lock that serialises the whole login
// lifecycle: scratch home, child, verification, install and sweep.
//
// Not a namespace lock. A namespace lock is named <user>+<acct>.lock and
// always contains "+", which [ValidateCodexSegment] forbids inside either
// id — so no namespace can ever derive this name. It shares the directory
// and nothing else: holding it says a login is in progress, never that any
// namespace may be written.
func (p *Paths) CodexScratchLock() string {
	return filepath.Join(p.CodexLocksDir(), "scratch.lock")
}

// CodexStateDir returns where the per-namespace refresh markers live.
func (p *Paths) CodexStateDir() string {
	return filepath.Join(p.CodexRoot(), ".state")
}

// CodexRefreshStatePath returns the refresh marker for one Codex namespace,
// <user>+<acct>.refresh. It returns a [errs.ConfigError] when either id
// fails [ValidateCodexSegment].
func (p *Paths) CodexRefreshStatePath(user, acct string) (string, error) {
	if err := validateCodexPair(user, acct); err != nil {
		return "", err
	}
	return filepath.Join(p.CodexStateDir(), user+"+"+acct+".refresh"), nil
}

// IsUnderCodexRoot reports whether path spells a path strictly below
// [Paths.CodexRoot], lexically, exactly as [Paths.IsUnderNamespaceRoot]
// does, with the same limit: it answers what the path says, and the
// no-follow walk answers where it leads.
func (p *Paths) IsUnderCodexRoot(path string) bool {
	return isStrictlyUnder(p.CodexRoot(), path)
}

// EnsureCodexDirs creates the Codex tree, each level at mode 0700.
//
// Only Codex commands call this, so a Claude-only store never grows a
// codex/ directory. It does not create the Claude tree either.
func (p *Paths) EnsureCodexDirs(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, dir := range []string{p.configDir, p.CodexRoot(), p.CodexLocksDir(), p.CodexStateDir(), p.CodexScratchRoot(), p.CacheRoot(), p.CodexCacheDir()} {
		if err := createDirMode(dir); err != nil {
			return err
		}
	}
	return nil
}

// createDirMode creates one directory — and any missing parents — at
// [DirMode], tolerating levels that already exist and leaving their modes
// alone.
func createDirMode(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := createDirMode(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, DirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
				return nil
			}
		}
		return errs.NewIO(fmt.Sprintf("could not create the directory `%s`", dir), err)
	}
	return nil
}

// isStrictlyUnder reports whether target spells a path strictly below root
// after folding "." and ".." out of both, without resolving any symbolic
// link. A ".." that would climb above a relative path's start is kept, so
// an escaping path can never compare equal to — or start with — the root.
func isStrictlyUnder(root, target string) bool {
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	return cleanTarget != cleanRoot && strings.HasPrefix(cleanTarget, cleanRoot+string(filepath.Separator))
}

// IsSingleComponent reports whether name is exactly one plain path
// component: not empty, not "." or "..", and free of separators, including
// a trailing one. Every descriptor-relative call that is handed a name asks
// this first, because openat, renameat and unlinkat resolve ".." and "/"
// inside the name, and a name that is not one component undoes the walk the
// descriptor came from.
func IsSingleComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, filepath.Separator)
}

// ValidateSegment checks that s is usable as a single path segment.
//
// Account and organization identifiers arrive from an OAuth response and
// end up as directory names, so they are validated rather than trusted: no
// separators, no "." or "..", no empty string, and a conservative ASCII
// alphabet. UUIDs and [UnknownOrg] pass; a value carrying "../" does not.
// It returns a [errs.ConfigError] describing which rule the value broke.
func ValidateSegment(s string) error {
	if s == "" {
		return errs.NewConfig("an account or organization id cannot be empty")
	}
	if s == "." || s == ".." {
		return errs.NewConfig(fmt.Sprintf("`%s` cannot be used as a directory name", s))
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return errs.NewConfig(fmt.Sprintf("the id `%s` contains `%c`, which is not allowed in a namespace directory name", s, r))
		}
	}
	return nil
}

// ValidateCodexSegment checks [ValidateSegment]'s rules, plus: a Codex id
// may not begin with a dot.
//
// A Codex namespace directory sits beside .locks, .state and .scratch
// under [Paths.CodexRoot], so an id of ".locks" would name the lock
// directory as somebody's namespace. No ChatGPT user or account id starts
// with a dot, so refusing every such id costs nothing. It returns a
// [errs.ConfigError] describing which rule the value broke.
func ValidateCodexSegment(s string) error {
	if err := ValidateSegment(s); err != nil {
		return err
	}
	if strings.HasPrefix(s, ".") {
		return errs.NewConfig(fmt.Sprintf("the id `%s` begins with `.`, which is reserved for agentctl's own directories", s))
	}
	return nil
}

// validateCodexPair validates the two ids every Codex path derivation
// takes, so a refused derivation creates nothing.
func validateCodexPair(user, acct string) error {
	if err := ValidateCodexSegment(user); err != nil {
		return err
	}
	return ValidateCodexSegment(acct)
}
