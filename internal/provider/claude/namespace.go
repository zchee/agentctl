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

// Claude Code's keychain naming rule, reproduced exactly.
//
// Everything here answers one question: which keychain item holds the
// credentials for a given configuration directory? Claude Code answers it
// by hashing an environment variable, and agentctl has to answer it the
// same way — not approximately — because the consequences of disagreeing
// are asymmetric. Guess a name that is not in use and an account shows as
// absent. Guess the name of a live item and one account's usage can show
// under another account's row, or a namespace can look free while a
// running session owns it.
//
// The rule:
//
//	service = "Claude Code-credentials" + suffix
//	suffix  = ""                             when the gate is falsy
//	        = "-" + sha256(NFC(raw))[0..8]   otherwise
//
//	gate, raw =
//	  CLAUDE_SECURESTORAGE_CONFIG_DIR present -> (that value,       that value)
//	  otherwise                               -> (CLAUDE_CONFIG_DIR, CLAUDE_CONFIG_DIR ?? ~/.claude)
//
// Three details that are easy to get wrong, each of which would produce a
// plausible-looking wrong answer:
//
//   - The gate is truthiness, not presence. An empty
//     CLAUDE_SECURESTORAGE_CONFIG_DIR yields the unsuffixed live name even
//     when CLAUDE_CONFIG_DIR is set, because an empty string is falsy to
//     the rule's author. Presence only decides which variable gets hashed.
//   - The hash is of the raw environment string, NFC-normalized and
//     nothing else. No symlink resolution, no path cleaning, no
//     trailing-slash fix. Two spellings of one directory are two different
//     items that can hold different credentials.
//   - NFC is applied on both branches: a decomposed path from the shell
//     and a composed one from a configuration file must hash alike.
//
// [Canonical] exists for one narrow purpose — deciding whether two entries
// point at the same physical directory — and is never used for identity:
// identity comes from the credentials, never from a path.

package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// LiveService is the unsuffixed service name: the credentials Claude Code
// is using now.
const LiveService = "Claude Code-credentials"

// SecureStorageEnv is the environment variable Claude Code hashes when it
// is present.
const SecureStorageEnv = "CLAUDE_SECURESTORAGE_CONFIG_DIR"

// ConfigDirEnv is the environment variable Claude Code hashes otherwise.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

// OAuthTokenEnv is the variable whose non-empty value short-circuits
// credential lookup entirely.
const OAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// ServiceKind is what a keychain service name turned out to be.
type ServiceKind struct {
	// Live reports the unsuffixed name: whatever Claude Code is using
	// right now.
	Live bool
	// Suffix is the eight-hex-digit configuration-directory hash when the
	// name is not the live one. The directory itself is unknowable from
	// the name: only the hash of how it was spelled survives.
	Suffix string
}

// EnvView is the set of environment values the naming rules depend on.
//
// Captured into a value rather than read at each use so the rules are
// testable without mutating the process environment, which would race
// every other test in the binary.
type EnvView struct {
	// SecureStorageDir is CLAUDE_SECURESTORAGE_CONFIG_DIR, nil when unset,
	// so an unset variable is distinguishable from an empty one.
	SecureStorageDir *string
	// ConfigDir is CLAUDE_CONFIG_DIR, nil when unset.
	ConfigDir *string
	// Home is the user's home directory.
	Home string
	// OAuthTokenSet reports whether CLAUDE_CODE_OAUTH_TOKEN holds a
	// non-empty value.
	OAuthTokenSet bool
}

// EnvFromProcess reads the current process environment, distinguishing an
// unset variable from an empty one: presence and truthiness play
// different roles in the naming rules.
func EnvFromProcess() EnvView {
	read := func(name string) *string {
		if value, ok := os.LookupEnv(name); ok {
			return &value
		}
		return nil
	}
	oauthToken := read(OAuthTokenEnv)
	return EnvView{
		SecureStorageDir: read(SecureStorageEnv),
		ConfigDir:        read(ConfigDirEnv),
		Home:             os.Getenv("HOME"),
		OAuthTokenSet:    oauthToken != nil && *oauthToken != "",
	}
}

// EnvWithHome returns an environment with nothing set but a home
// directory, for tests and for the paths that only take a --config-dir.
func EnvWithHome(home string) EnvView {
	return EnvView{Home: home}
}

// SHA8 returns the first eight hex digits of the SHA-256 of raw,
// NFC-normalized.
func SHA8(raw string) string {
	digest := sha256.Sum256([]byte(norm.NFC.String(raw)))
	return hex.EncodeToString(digest[:])[:8]
}

// ServiceName returns the keychain service name Claude Code would use in
// this environment.
func ServiceName(env *EnvView) string {
	var unsuffixed bool
	var raw string
	switch {
	case env.SecureStorageDir != nil:
		// Present: this variable is both the gate and the hash input.
		unsuffixed = *env.SecureStorageDir == ""
		raw = *env.SecureStorageDir
	default:
		// Absent: CLAUDE_CONFIG_DIR is the gate, and the same variable —
		// defaulting to ~/.claude — is the hash input.
		unsuffixed = env.ConfigDir == nil || *env.ConfigDir == ""
		raw = configDirOrDefault(env)
	}
	if unsuffixed {
		return LiveService
	}
	return LiveService + "-" + SHA8(raw)
}

// Classify classifies a keychain service name.
//
// It reports false for anything that is not a Claude Code credentials
// item, which deliberately includes the legacy per-directory API-key items
// and items belonging to third-party switcher tools. Both exist on real
// machines; neither is a credential agentctl can read or reason about.
func Classify(service string) (ServiceKind, bool) {
	if service == LiveService {
		return ServiceKind{Live: true}, true
	}
	rest, hasPrefix := strings.CutPrefix(service, LiveService)
	if !hasPrefix {
		return ServiceKind{}, false
	}
	suffix, hasDash := strings.CutPrefix(rest, "-")
	if !hasDash || len(suffix) != 8 {
		return ServiceKind{}, false
	}
	for i := range len(suffix) {
		b := suffix[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return ServiceKind{}, false
		}
	}
	return ServiceKind{Suffix: suffix}, true
}

// Canonical resolves dir through symlinks and normalizes the result.
//
// Used for exactly one question — do two entries name the same physical
// directory? — and never for identity. On a machine where the default
// configuration directory is a symlink, the same directory would otherwise
// look like two unrelated accounts.
func Canonical(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return norm.NFC.String(resolved), nil
}

// ExportSpelling is how a directory is spelled for naming purposes: NFC,
// no trailing slash.
//
// This is what gets recorded in an owned account record and hashed into
// its service suffix, because it is the spelling a Claude Code session
// would be handed — not where it resolves to.
func ExportSpelling(dir string) string {
	text := norm.NFC.String(dir)
	trimmed := strings.TrimRight(text, "/")
	if trimmed == "" {
		return text
	}
	return trimmed
}

// SecureStorageNamespace returns the namespace
// CLAUDE_SECURESTORAGE_CONFIG_DIR points this shell at, and whether it
// points at one at all.
//
// The gate in one place: the variable must be truthy, not merely present,
// so an empty value reports false — it names the live item exactly as an
// unset variable would. Every caller that has to answer "is this shell
// pointed at a namespace?" asks this rather than spelling the emptiness
// check again, because two halves of a swap disagreeing about that
// question is how a namespace ends up locked while the live item is
// written. The value is returned rather than a bare bool because the
// refusals name it, and a refusal that says only "the variable is set"
// leaves the user hunting for which shell set it.
func SecureStorageNamespace(env *EnvView) (string, bool) {
	if env.SecureStorageDir != nil && *env.SecureStorageDir != "" {
		return *env.SecureStorageDir, true
	}
	return "", false
}

// LiveStoreDir returns the directory whose credential store Claude Code
// reads right now: the one that holds the credential file, the refresh
// lock and the storage-write lock.
//
// CLAUDE_SECURESTORAGE_CONFIG_DIR wins when it holds a non-empty value —
// truthiness again, so an empty value falls back to ~/.claude rather than
// to CLAUDE_CONFIG_DIR.
//
// One deliberate divergence from Claude Code: its own fallback keeps an
// empty CLAUDE_CONFIG_DIR, producing a relative credential path in
// whatever directory the process happened to start in. agentctl treats
// that as ~/.claude instead: reading a credential file out of the current
// working directory and presenting it as the live credentials is not a
// behaviour worth reproducing. The divergence cannot affect a service
// name, because an empty CLAUDE_CONFIG_DIR is falsy and the hash input is
// then never consulted.
func LiveStoreDir(env *EnvView) string {
	switch {
	case env.SecureStorageDir != nil && *env.SecureStorageDir != "":
		return norm.NFC.String(*env.SecureStorageDir)
	case env.SecureStorageDir != nil:
		return joinPath(env.Home, ".claude")
	default:
		dir := configDirOrDefault(env)
		if dir == "" {
			return joinPath(env.Home, ".claude")
		}
		return dir
	}
}

// ClaudeJSONPath returns where the configuration document holding
// oauthAccount lives.
//
// Keyed on CLAUDE_CONFIG_DIR or the home directory — not on
// [LiveStoreDir]. The two diverge whenever
// CLAUDE_SECURESTORAGE_CONFIG_DIR is set. Consulted only for the live
// row. Display-side readers use this path; every writer, lock and backup
// derivation uses [GlobalConfigPath], or the lock and the rewrite would
// target a file the peer does not.
func ClaudeJSONPath(env *EnvView) string {
	if env.ConfigDir != nil && *env.ConfigDir != "" {
		return joinPath(norm.NFC.String(*env.ConfigDir), ".claude.json")
	}
	return joinPath(env.Home, ".claude.json")
}

// GlobalConfigPath returns the global configuration file Claude Code
// reads and writes: the config home's .config.json when that path exists
// — following links, the way an existence probe does — and
// [ClaudeJSONPath] otherwise. Every writer of the file, its lock and its
// backups derive from this, never from [ClaudeJSONPath] alone.
//
// The existence check is the one filesystem access in the naming rules.
func GlobalConfigPath(env *EnvView) string {
	preferred := joinPath(configHome(env), ".config.json")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	return ClaudeJSONPath(env)
}

// BackupsDir returns where the peer keeps its configuration backups:
// under the config home, which is not beside the configuration file.
func BackupsDir(env *EnvView) string {
	return joinPath(configHome(env), "backups")
}

// SessionsDir returns the read-only session registry under this
// environment's configuration home.
func SessionsDir(env *EnvView) string {
	return joinPath(configHome(env), "sessions")
}

// ConfigLockName returns the name of the configuration lock directory for
// configPath: the literal file name plus ".lock", beside the path as
// spelled rather than beside what it resolves to, because that is where
// the peer's own locking library puts it. It reports false for a path
// with no file name.
func ConfigLockName(configPath string) (string, bool) {
	base := filepath.Base(configPath)
	if base == "/" || base == "." || base == ".." {
		return "", false
	}
	return base + ".lock", true
}

// configHome is the configuration home as a writer derives it:
// CLAUDE_CONFIG_DIR when it holds a non-empty value, else the home's
// .claude directory, NFC-normalized.
//
// An empty value follows [LiveStoreDir]'s documented divergence, so no
// derivation here is ever a relative path. That is also where it differs
// from configDirOrDefault, which keeps the empty string because the
// naming rule hashes the raw value.
func configHome(env *EnvView) string {
	if env.ConfigDir != nil && *env.ConfigDir != "" {
		return norm.NFC.String(*env.ConfigDir)
	}
	return norm.NFC.String(joinPath(env.Home, ".claude"))
}

// configDirOrDefault is the naming rule's hash input: CLAUDE_CONFIG_DIR
// if present — empty string included — else the home's .claude directory,
// NFC-normalized either way.
func configDirOrDefault(env *EnvView) string {
	if env.ConfigDir != nil {
		return norm.NFC.String(*env.ConfigDir)
	}
	return norm.NFC.String(joinPath(env.Home, ".claude"))
}

// joinPath joins one name onto a directory without cleaning the
// directory's own spelling, because several of these paths are recorded
// and compared as spelled.
func joinPath(dir, name string) string {
	switch {
	case dir == "":
		return name
	case strings.HasSuffix(dir, "/"):
		return dir + name
	default:
		return dir + "/" + name
	}
}
