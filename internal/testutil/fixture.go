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

package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ClosedEndpoint is an unroutable base URL: port 9 on loopback, which
// nothing listens on. A test that means to talk to a server overrides the
// endpoint variables; a test that does not, cannot reach anything.
const ClosedEndpoint = "http://127.0.0.1:9"

// KeychainAccount is the account attribute the fake keychain items carry.
//
// It is also the USER every command runs with, so the account the binary's
// own reads match on is the same string the items are stored under and the
// dump-keychain listing advertises. A reader account that differed from the
// items' account would model a divergence it could never detect.
const KeychainAccount = "example"

// envPair is one environment variable a fixture sets on every command.
type envPair struct {
	key   string
	value string
}

// Fixture is a temporary store, a fake home, and the environment that
// points one process at both and at nothing else.
type Fixture struct {
	tb   testing.TB
	root string
	env  []envPair
}

// New builds an isolated store with the keychain disabled outright and
// every endpoint closed.
func New(tb testing.TB) *Fixture {
	tb.Helper()
	root := tb.TempDir()
	for _, relative := range []string{"config", "home", "bin", "keychain-items"} {
		if err := os.MkdirAll(filepath.Join(root, relative), 0o755); err != nil {
			tb.Fatalf("create fixture directory %q: %v", relative, err)
		}
	}

	f := &Fixture{tb: tb, root: root}
	// Unroutable by default, for both providers. The swap's profile GET
	// carries a live bearer token, so it gets the same closed default: a
	// test that forgot its mock must never reach a real endpoint.
	f.Set("AGENTCTL_CLAUDE_USAGE_URL", ClosedEndpoint)
	f.Set("AGENTCTL_CLAUDE_TOKEN_URL", ClosedEndpoint+"/token")
	f.Set("AGENTCTL_CLAUDE_AUTHORIZE_URL", ClosedEndpoint+"/authorize")
	f.Set("AGENTCTL_CLAUDE_PROFILE_URL", ClosedEndpoint+ProfilePath)
	f.Set("AGENTCTL_CODEX_TOKEN_URL", ClosedEndpoint+"/oauth/token")
	f.Set("AGENTCTL_CODEX_USAGE_URL", ClosedEndpoint)
	// login prints the URL either way; opening a window on the developer's
	// desktop from a test run is not acceptable.
	f.Set("AGENTCTL_NO_BROWSER", "1")
	f.Set("AGENTCTL_KEYCHAIN_BACKEND", "none")
	// Pinned rather than inherited: the current account is read from USER
	// with LOGNAME as the fallback, and both must name the account the fake
	// keychain's items are filed under, or a read matches nothing on one
	// machine and everything on another.
	f.Set("USER", KeychainAccount)
	f.Set("LOGNAME", KeychainAccount)
	f.Set("HOME", f.Home())
	return f
}

// Root returns the fixture's own root directory, which everything the
// fixture creates lives under.
func (f *Fixture) Root() string {
	return f.root
}

// ConfigDir returns the configuration directory the command under test is
// pointed at.
func (f *Fixture) ConfigDir() string {
	return filepath.Join(f.root, "config")
}

// Home returns the fake HOME.
func (f *Fixture) Home() string {
	return filepath.Join(f.root, "home")
}

// BinDir returns the owned directory the fake executables are installed
// into.
func (f *Fixture) BinDir() string {
	return filepath.Join(f.root, "bin")
}

// Scratch returns a path inside the fixture, for resume files and the like.
func (f *Fixture) Scratch(name string) string {
	return filepath.Join(f.root, name)
}

// ConfigFile returns the registry file path.
func (f *Fixture) ConfigFile() string {
	return filepath.Join(f.ConfigDir(), "config.json")
}

// NamespaceDir returns one account's namespace directory.
func (f *Fixture) NamespaceDir(acct, org string) string {
	return filepath.Join(f.ConfigDir(), "claude", acct, org)
}

// CredentialsPath returns one account's credential file path.
func (f *Fixture) CredentialsPath(acct, org string) string {
	return filepath.Join(f.NamespaceDir(acct, org), ".credentials.json")
}

// LockPath returns one account's namespace lock file path.
func (f *Fixture) LockPath(acct, org string) string {
	return filepath.Join(f.ConfigDir(), "claude", ".locks", acct+"."+org+".lock")
}

// Set records one environment variable for every command this fixture
// builds, replacing any earlier value for the same key.
func (f *Fixture) Set(key, value string) *Fixture {
	f.env = append(deleteEnv(f.env, key), envPair{key: key, value: value})
	return f
}

// Unset removes one environment variable from the fixture's own settings,
// leaving the scrub of inherited values in place.
func (f *Fixture) Unset(key string) *Fixture {
	f.env = deleteEnv(f.env, key)
	return f
}

// Lookup returns the value this fixture would set for key.
func (f *Fixture) Lookup(key string) (string, bool) {
	for _, pair := range f.env {
		if pair.key == key {
			return pair.value, true
		}
	}
	return "", false
}

// deleteEnv removes every pair whose key is key.
func deleteEnv(env []envPair, key string) []envPair {
	kept := env[:0]
	for _, pair := range env {
		if pair.key != key {
			kept = append(kept, pair)
		}
	}
	return kept
}

// Endpoints points the usage, token, authorize and profile endpoints at a
// mock server.
func (f *Fixture) Endpoints(baseURL string) *Fixture {
	f.Set("AGENTCTL_CLAUDE_USAGE_URL", baseURL)
	f.Set("AGENTCTL_CLAUDE_TOKEN_URL", baseURL+TokenPath)
	f.Set("AGENTCTL_CLAUDE_AUTHORIZE_URL", baseURL+"/oauth/authorize")
	f.Set("AGENTCTL_CLAUDE_PROFILE_URL", baseURL+ProfilePath)
	return f
}

// CodexEndpoints points the token and usage endpoints at a mock server.
func (f *Fixture) CodexEndpoints(baseURL string) *Fixture {
	f.Set("AGENTCTL_CODEX_TOKEN_URL", baseURL+"/oauth/token")
	f.Set("AGENTCTL_CODEX_USAGE_URL", baseURL)
	return f
}

// Fault turns on fault injection.
func (f *Fixture) Fault(names string) *Fixture {
	return f.Set("AGENTCTL_FAULT", names)
}

// scrubbedExact lists the inherited variables every command starts
// without. A value for any of these would point the binary back at the
// real machine - the live store, the developer's own configuration - or
// quietly change what a test is measuring. HOME, USER and LOGNAME are
// removed here and re-set by the fixture's own settings.
var scrubbedExact = []string{
	"CLAUDE_CONFIG_DIR",
	"CLAUDE_SECURESTORAGE_CONFIG_DIR",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CODEX_HOME",
	"HOME",
	"USER",
	"LOGNAME",
}

// scrubbedPrefixes removes every production and testing variable of this
// binary and every scripting knob of the fake executables, so an inherited
// value loses to the fixture rather than to an enumeration that has to be
// kept complete by hand.
var scrubbedPrefixes = []string{
	"AGENTCTL_",
	"AGCTL_FAKE_",
}

// scrubEnviron returns environ without every scrubbed variable.
func scrubEnviron(environ []string) []string {
	kept := make([]string, 0, len(environ))
next:
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		for _, exact := range scrubbedExact {
			if key == exact {
				continue next
			}
		}
		for _, prefix := range scrubbedPrefixes {
			if strings.HasPrefix(key, prefix) {
				continue next
			}
		}
		kept = append(kept, entry)
	}
	return kept
}

// assertKeychainSeam refuses to build an environment that could reach the
// real security(1).
//
// Every fixture must be in one of exactly two states: the fake is wired
// (AGENTCTL_SECURITY_BIN), or the keychain is switched off outright
// (AGENTCTL_KEYCHAIN_BACKEND=none). A fixture in neither would hand the
// child a reader and a write transport with nothing behind them, and this
// layer decides whether a test could touch the developer's own keychain.
func (f *Fixture) assertKeychainSeam() {
	f.tb.Helper()
	_, securityWired := f.Lookup("AGENTCTL_SECURITY_BIN")
	_, backendWired := f.Lookup("AGENTCTL_KEYCHAIN_BACKEND")
	if !securityWired && !backendWired {
		f.tb.Fatalf("this fixture wires neither the fake security nor the disabled backend, so the child would have no keychain seam at all; call WithKeychain if the test needs a keychain, and leave New's default alone if it does not")
	}
}

// Environ returns the isolated environment: the process environment with
// every scrubbed variable removed, then this fixture's own settings.
// Removals run first, so an inherited value loses to the fixture rather
// than to the scrub list.
func (f *Fixture) Environ() []string {
	f.tb.Helper()
	f.assertKeychainSeam()
	env := scrubEnviron(os.Environ())
	for _, pair := range f.env {
		env = append(env, pair.key+"="+pair.value)
	}
	return env
}

// Apply sets cmd's environment to this fixture's isolated environment.
func (f *Fixture) Apply(cmd *exec.Cmd) *exec.Cmd {
	f.tb.Helper()
	cmd.Env = f.Environ()
	return cmd
}
