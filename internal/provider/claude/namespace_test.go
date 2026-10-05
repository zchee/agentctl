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
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// envView builds the environment the naming vectors share. The sha8
// vectors are literal absolute paths verified against an independent
// SHA-256 implementation; they are the load-bearing assertions here,
// because a subtly wrong naming rule produces plausible service names
// that name nothing while every downstream test still passes.
func envView(securestorage, config *string) *EnvView {
	return &EnvView{
		SecureStorageDir: securestorage,
		ConfigDir:        config,
		Home:             "/Users/zchee",
	}
}

func TestSHA8MatchesTheVerifiedVectors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw  string
		want string
	}{
		"success: the default configuration directory": {
			raw:  "/Users/zchee/.claude",
			want: "95313c21",
		},
		"success: a project configuration directory": {
			raw:  "/Users/zchee/src/github.com/zchee/agent/claude",
			want: "5cdc535f",
		},
		"success: an XDG-style configuration directory": {
			raw:  "/Users/zchee/.config/claude-code",
			want: "86c75be7",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := SHA8(tt.raw); got != tt.want {
				t.Fatalf("SHA8(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSHA8NormalizesToNFC(t *testing.T) {
	t.Parallel()

	// A composed and a decomposed spelling must hash alike: the shell
	// hands over decomposed paths on macOS while a configuration file
	// usually carries composed ones, and they name the same directory.
	composed := "/Users/zchee/café"
	decomposed := "/Users/zchee/café"
	if composed == decomposed {
		t.Fatal("the two spellings must differ as bytes")
	}
	if SHA8(composed) != SHA8(decomposed) {
		t.Fatal("NFC must make the two spellings hash alike")
	}
}

func TestServiceNameGateIsTruthinessNotPresence(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		env  *EnvView
		want string
	}{
		"success: nothing set names the live item": {
			env:  envView(nil, nil),
			want: LiveService,
		},
		"success: empty securestorage wins over a set config dir": {
			env:  envView(new(""), new("/x")),
			want: LiveService,
		},
		"success: empty config dir alone names the live item": {
			env:  envView(nil, new("")),
			want: LiveService,
		},
		"success: securestorage set is hashed": {
			env:  envView(new("/Users/zchee/.config/claude-code"), new("/x")),
			want: LiveService + "-86c75be7",
		},
		"success: config dir set with securestorage absent is hashed": {
			env:  envView(nil, new("/Users/zchee/src/github.com/zchee/agent/claude")),
			want: LiveService + "-5cdc535f",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ServiceName(tt.env); got != tt.want {
				t.Fatalf("ServiceName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServiceNameHashesTheRawStringNotTheResolvedPath(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("the target directory should be creatable: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("the symlink should be creatable: %v", err)
	}

	throughLink := envView(nil, &link)
	throughReal := envView(nil, &real)
	if ServiceName(throughLink) == ServiceName(throughReal) {
		t.Fatal("two spellings of one directory are two different keychain items")
	}

	linkCanonical, err := Canonical(link)
	if err != nil {
		t.Fatalf("the link resolves: %v", err)
	}
	realCanonical, err := Canonical(real)
	if err != nil {
		t.Fatalf("the target resolves: %v", err)
	}
	if linkCanonical != realCanonical {
		t.Fatalf("but they canonicalize to the same physical directory: %q vs %q", linkCanonical, realCanonical)
	}
}

func TestServiceNameAppliesNFCOnBothBranches(t *testing.T) {
	t.Parallel()

	decomposed := "/Users/zchee/café"
	composed := "/Users/zchee/café"
	if ServiceName(envView(&decomposed, nil)) != ServiceName(envView(&composed, nil)) {
		t.Fatal("the securestorage branch must normalize")
	}
	if ServiceName(envView(nil, &decomposed)) != ServiceName(envView(nil, &composed)) {
		t.Fatal("the config-dir branch must normalize")
	}
}

func TestClassifyRecognisesOnlyCredentialItems(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		service string
		want    ServiceKind
		ok      bool
	}{
		"success: the live item": {
			service: "Claude Code-credentials",
			want:    ServiceKind{Live: true},
			ok:      true,
		},
		"success: a configuration-directory item": {
			service: "Claude Code-credentials-5cdc535f",
			want:    ServiceKind{Suffix: "5cdc535f"},
			ok:      true,
		},
		"error: the legacy API-key item is the same product but not a credential": {
			service: "Claude Code-86c75be7",
		},
		"error: a third-party switcher item": {
			service: "claude-switcher:user@example.com",
		},
		"error: an empty suffix": {
			service: "Claude Code-credentials-",
		},
		"error: an uppercase suffix": {
			service: "Claude Code-credentials-5CDC535F",
		},
		"error: a seven-digit suffix": {
			service: "Claude Code-credentials-5cdc535",
		},
		"error: a nine-digit suffix": {
			service: "Claude Code-credentials-5cdc535ff",
		},
		"error: a non-hex suffix": {
			service: "Claude Code-credentials-zzzzzzzz",
		},
		"error: an unrelated product": {
			service: "Chrome Safe Storage",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := Classify(tt.service)
			if ok != tt.ok {
				t.Fatalf("Classify(%q) ok = %v, want %v", tt.service, ok, tt.ok)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("Classify(%q) mismatch (-want +got):\n%s", tt.service, diff)
			}
		})
	}
}

func TestLiveStoreDirFollowsTheStoreRule(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		env  *EnvView
		want string
	}{
		"success: nothing set": {
			env:  envView(nil, nil),
			want: "/Users/zchee/.claude",
		},
		"success: securestorage set": {
			env:  envView(new("/store"), new("/config")),
			want: "/store",
		},
		// Truthiness again: an empty securestorage falls back to the home
		// store, not to CLAUDE_CONFIG_DIR.
		"success: empty securestorage": {
			env:  envView(new(""), new("/config")),
			want: "/Users/zchee/.claude",
		},
		"success: config dir only": {
			env:  envView(nil, new("/config")),
			want: "/config",
		},
		"success: empty config dir": {
			env:  envView(nil, new("")),
			want: "/Users/zchee/.claude",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := LiveStoreDir(tt.env); got != tt.want {
				t.Fatalf("LiveStoreDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClaudeJSONIsKeyedOnTheConfigDirNotTheStoreDir(t *testing.T) {
	t.Parallel()

	view := envView(new("/store"), new("/config"))
	if got := LiveStoreDir(view); got != "/store" {
		t.Fatalf("LiveStoreDir = %q", got)
	}
	if got := ClaudeJSONPath(view); got != "/config/.claude.json" {
		t.Fatalf("ClaudeJSONPath = %q", got)
	}

	noConfig := envView(new("/store"), nil)
	if got := ClaudeJSONPath(noConfig); got != "/Users/zchee/.claude.json" {
		t.Fatalf("ClaudeJSONPath without a config dir = %q", got)
	}
}

func TestGlobalConfigPathPrefersConfigJSONUnderTheConfigDir(t *testing.T) {
	t.Parallel()

	// Every row runs against a temporary home, so the one existence check
	// this function makes never looks at the developer's own store.
	home := t.TempDir()
	other := t.TempDir()
	atHome := func(configDir *string) *EnvView {
		return &EnvView{ConfigDir: configDir, Home: home}
	}

	// Neither .config.json exists yet: the display path's answer.
	preCreation := map[string]struct {
		env  *EnvView
		want string
	}{
		"success: nothing set, no .config.json": {
			env:  atHome(nil),
			want: filepath.Join(home, ".claude.json"),
		},
		"success: CLAUDE_CONFIG_DIR set, no .config.json": {
			env:  atHome(&other),
			want: filepath.Join(other, ".claude.json"),
		},
		"success: CLAUDE_CONFIG_DIR empty, no .config.json": {
			env:  atHome(new("")),
			want: filepath.Join(home, ".claude.json"),
		},
	}
	for name, tt := range preCreation {
		if got := GlobalConfigPath(tt.env); got != tt.want {
			t.Fatalf("%s: GlobalConfigPath = %q, want %q", name, got, tt.want)
		}
		if got := GlobalConfigPath(tt.env); got != ClaudeJSONPath(tt.env) {
			t.Fatalf("%s: must fall back to the display path", name)
		}
	}

	// The home config home's .config.json present: it wins for an unset
	// and for an empty CLAUDE_CONFIG_DIR — the empty value is never a
	// relative path.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("the config home is creatable: %v", err)
	}
	preferred := filepath.Join(home, ".claude", ".config.json")
	if err := os.WriteFile(preferred, []byte("{}"), 0o644); err != nil {
		t.Fatalf("a .config.json: %v", err)
	}
	if got := GlobalConfigPath(atHome(nil)); got != preferred {
		t.Fatalf("with the home .config.json present: %q, want %q", got, preferred)
	}
	if got := GlobalConfigPath(atHome(new(""))); got != preferred {
		t.Fatalf("an empty CLAUDE_CONFIG_DIR takes the home rule: %q, want %q", got, preferred)
	}
	if !filepath.IsAbs(GlobalConfigPath(atHome(new("")))) {
		t.Fatal("never a relative path")
	}
	// ...but not for a CLAUDE_CONFIG_DIR naming somewhere else, whose own
	// .config.json is absent.
	if got := GlobalConfigPath(atHome(&other)); got != filepath.Join(other, ".claude.json") {
		t.Fatalf("a set CLAUDE_CONFIG_DIR looks only under itself: %q", got)
	}

	// That directory's own .config.json present: it.
	if err := os.WriteFile(filepath.Join(other, ".config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("a .config.json under the other dir: %v", err)
	}
	if got := GlobalConfigPath(atHome(&other)); got != filepath.Join(other, ".config.json") {
		t.Fatalf("the other dir's .config.json must win: %q", got)
	}

	// Followed like an existence probe: a link named .config.json counts
	// when its target exists, and a dangling one does not.
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(linked, "missing"), filepath.Join(linked, ".config.json")); err != nil {
		t.Fatalf("a dangling link: %v", err)
	}
	if got := GlobalConfigPath(atHome(&linked)); got != filepath.Join(linked, ".claude.json") {
		t.Fatalf("a dangling .config.json link does not exist to the probe: %q", got)
	}
}

func TestBackupsDirIsUnderTheConfigHomeNotBesideTheConfigFile(t *testing.T) {
	t.Parallel()

	view := envView(nil, nil)
	if got := BackupsDir(view); got != "/Users/zchee/.claude/backups" {
		t.Fatalf("BackupsDir = %q", got)
	}
	if filepath.Dir(BackupsDir(view)) == filepath.Dir(ClaudeJSONPath(view)) {
		t.Fatal("the backups are not beside the configuration file")
	}
	if got := BackupsDir(envView(nil, new("/x"))); got != "/x/backups" {
		t.Fatalf("BackupsDir under a config dir = %q", got)
	}
	if got := BackupsDir(envView(nil, new(""))); got != "/Users/zchee/.claude/backups" {
		t.Fatalf("an empty CLAUDE_CONFIG_DIR is the home config home, never relative: %q", got)
	}
	// The securestorage variable names a credential namespace, not the
	// configuration home.
	if got := BackupsDir(envView(new("/store"), nil)); got != "/Users/zchee/.claude/backups" {
		t.Fatalf("securestorage must not move the backups: %q", got)
	}
}

func TestConfigLockNameIsTheLiteralFileNamePlusLock(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
		want string
		ok   bool
	}{
		"success: the home configuration file": {
			path: "/Users/zchee/.claude.json",
			want: ".claude.json.lock",
			ok:   true,
		},
		"success: a config home's own file": {
			path: "/x/.config.json",
			want: ".config.json.lock",
			ok:   true,
		},
		"error: the root has no file name": {
			path: "/",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := ConfigLockName(tt.path)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ConfigLockName(%q) = %q, %v; want %q, %v", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestSessionsDirFollowsTheConfigHomeNotTheCredentialStore(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config *string
		want   string
	}{
		"success: a set config dir": {
			config: new("/config"),
			want:   "/config/sessions",
		},
		"success: an empty config dir": {
			config: new(""),
			want:   "/Users/zchee/.claude/sessions",
		},
		"success: an unset config dir": {
			want: "/Users/zchee/.claude/sessions",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := SessionsDir(envView(new("/store"), tt.config)); got != tt.want {
				t.Fatalf("SessionsDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExportSpellingTrimsTrailingSlashesAndNormalizes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dir  string
		want string
	}{
		"success: one trailing slash is trimmed":  {dir: "/a/b/", want: "/a/b"},
		"success: no trailing slash is unchanged": {dir: "/a/b", want: "/a/b"},
		"success: the root keeps its spelling":    {dir: "/", want: "/"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ExportSpelling(tt.dir); got != tt.want {
				t.Fatalf("ExportSpelling(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
	if ExportSpelling("/café") != ExportSpelling("/café") {
		t.Fatal("the two spellings must normalize alike")
	}
}

func TestTheEnvironmentVariableNamesAreTheOnesClaudeCodeReads(t *testing.T) {
	t.Parallel()

	if SecureStorageEnv != "CLAUDE_SECURESTORAGE_CONFIG_DIR" {
		t.Fatalf("SecureStorageEnv = %q", SecureStorageEnv)
	}
	if ConfigDirEnv != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("ConfigDirEnv = %q", ConfigDirEnv)
	}
	if OAuthTokenEnv != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("OAuthTokenEnv = %q", OAuthTokenEnv)
	}
}

func TestEnvFromProcessReadsTheEnvironment(t *testing.T) {
	t.Setenv(SecureStorageEnv, "/store")
	t.Setenv(ConfigDirEnv, "")
	t.Setenv(OAuthTokenEnv, "a-token")
	t.Setenv("HOME", "/Users/zchee")

	view := EnvFromProcess()
	if view.SecureStorageDir == nil || *view.SecureStorageDir != "/store" {
		t.Fatalf("SecureStorageDir = %v", view.SecureStorageDir)
	}
	if view.ConfigDir == nil || *view.ConfigDir != "" {
		t.Fatal("a set-but-empty variable is present, not unset")
	}
	if !view.OAuthTokenSet {
		t.Fatal("a non-empty token is set")
	}
	if view.Home != "/Users/zchee" {
		t.Fatalf("Home = %q", view.Home)
	}

	service := ServiceName(&view)
	if !strings.HasPrefix(service, LiveService) {
		t.Fatalf("a service name starts with the live name: %q", service)
	}
	if len(service) != len(LiveService) && len(service) != len(LiveService)+9 {
		t.Fatalf("a service name is unsuffixed or carries a nine-character suffix: %q", service)
	}
}
