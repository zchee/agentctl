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

//go:build agentctl_testing

package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

// oldBlob carries no tokenAccount, as older credential documents did.
const oldBlob = `{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":9999999999999}}`

// liveBlob is the live item's credential, with a full identity block.
const liveBlob = `{"claudeAiOauth":{"accessToken":"live-access","refreshToken":"live-refresh","expiresAt":9999999999999,"tokenAccount":{"uuid":"11111111-1111-4111-8111-111111111111","emailAddress":"live@example.com","organizationUuid":"22222222-2222-4222-8222-222222222222"}}}`

// liveUUID is the account liveBlob names.
const liveUUID = "11111111-1111-4111-8111-111111111111"

// otherUUID is the account otherBlob names.
const otherUUID = "33333333-3333-4333-8333-333333333333"

// keychainWorld is a fixture with the fake security(1) installed, its seam
// variables mirrored into the process environment — the in-process reader
// factory reads them there, because the subprocess-facing Environ is for
// commands and discovery is being driven directly.
//
// The reader itself is built by [keychainReader], separately and last,
// because the factory captures the stand-in's knobs at construction: a
// knob set after the reader exists would never reach the child.
func keychainWorld(t *testing.T) *testutil.Fixture {
	t.Helper()
	fixture := testutil.New(t).WithKeychain()
	for _, key := range []string{"AGENTCTL_SECURITY_BIN", "AGCTL_FAKE_SECURITY_LOG", "AGCTL_FAKE_SECURITY_ITEMS", "AGCTL_FAKE_SECURITY_DUMP", "USER", "LOGNAME"} {
		value, ok := fixture.Lookup(key)
		if !ok {
			t.Fatalf("the fixture sets no %s", key)
		}
		t.Setenv(key, value)
	}
	t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
	return fixture
}

// keychainReader builds the reader after every knob is in place.
func keychainReader(t *testing.T) secret.Reader {
	t.Helper()
	return secret.NewReader()
}

// fixtureEnv is the environment view pointing at the fixture's fake home.
func fixtureEnv(fixture *testutil.Fixture) *EnvView {
	env := EnvWithHome(fixture.Home())
	return &env
}

// fixturePaths roots a store inside the fixture.
func fixturePaths(fixture *testutil.Fixture) *config.Paths {
	return config.NewPaths(fixture.ConfigDir())
}

// emptyRegistry is a registry that knows nothing.
func emptyRegistry() *config.Registry {
	return &config.Registry{Version: config.RegistryVersion}
}

// assertNeverRead fails when the argv log shows an item read for service.
func assertNeverRead(t *testing.T, fixture *testutil.Fixture, service string) {
	t.Helper()
	for _, line := range fixture.SecurityLog() {
		if strings.HasPrefix(line, "find-generic-password") && strings.Contains(line, service) {
			t.Fatalf("`%s` was read: %s", service, line)
		}
	}
}

func TestDiscoverLiveItemBecomesOneRowWithItsIdentity(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	fixture.Dump(testutil.LiveService)
	fixture.KeychainItem(testutil.LiveService, liveBlob)

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	if discovery.Preflight.State != secret.KeychainStateUnlocked {
		t.Fatalf("preflight = %v, want unlocked", discovery.Preflight)
	}
	if len(discovery.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(discovery.Rows))
	}

	live := discovery.Rows[0]
	if live.ID != liveUUID {
		t.Errorf("id = %q, want %q", live.ID, liveUUID)
	}
	if live.State.Kind != StateOK {
		t.Errorf("state = %q, want ok", live.State.Kind)
	}
	if live.Source != SourceKeychain {
		t.Errorf("source = %q, want keychain", live.Source)
	}
	if !live.VisibleByDefault {
		t.Error("the live row is shown by default")
	}
	if live.Credentials == nil {
		t.Error("the live credentials were read")
	}
	if live.Record.Email == nil || *live.Record.Email != "live@example.com" {
		t.Errorf("email = %v, want live@example.com", live.Record.Email)
	}
	if live.Record.OrganizationUUID != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("organization = %q", live.Record.OrganizationUUID)
	}
	if !strings.Contains(live.Note, testutil.LiveService) {
		t.Errorf("note = %q, want it to name the service", live.Note)
	}
	fixture.AssertKeychainReadOnly()
}

// linkedLiveStore makes the fixture's live store directory a symlink and
// returns the sibling service named after where it resolves.
func linkedLiveStore(t *testing.T, fixture *testutil.Fixture) string {
	t.Helper()
	real := filepath.Join(fixture.Home(), "agent-claude")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("create the real directory: %v", err)
	}
	if err := os.Symlink(real, filepath.Join(fixture.Home(), ".claude")); err != nil {
		t.Fatalf("link the live store: %v", err)
	}
	return testutil.CanonicalMigrationService(t, real)
}

func TestDiscoverSiblingOfTheLiveDirectoryIsHiddenAndNeverFolded(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	sibling := linkedLiveStore(t, fixture)
	fixture.Dump(testutil.LiveService, sibling)
	fixture.KeychainItem(testutil.LiveService, liveBlob)
	fixture.KeychainItem(sibling, otherBlob)

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	if len(discovery.Rows) != 2 {
		t.Fatalf("rows = %d, want the live row and the sibling", len(discovery.Rows))
	}
	row := rowByID(t, discovery, otherUUID)
	if row.State.Kind != StateStaleSiblingOfLive {
		t.Fatalf("state = %q, want stale_sibling_of_live", row.State.Kind)
	}
	if row.VisibleByDefault {
		t.Error("a stale sibling is hidden without the show-everything flag")
	}
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverIdenticalBlobUnderASecondNameFoldsIntoTheLiveRow(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	sibling := linkedLiveStore(t, fixture)
	fixture.Dump(testutil.LiveService, sibling)
	fixture.KeychainItem(testutil.LiveService, liveBlob)
	fixture.KeychainItem(sibling, liveBlob)

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	if len(discovery.Rows) != 1 {
		t.Fatalf("rows = %d, want one: same digests are one account", len(discovery.Rows))
	}
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverUnclaimedItemIsShownAndALegacyKeyIsIgnored(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	fixture.Dump(testutil.LiveService, "Claude Code-credentials-6cdd6b98", "Claude Code-86c75be7")
	fixture.KeychainItem(testutil.LiveService, liveBlob)
	fixture.KeychainItem("Claude Code-credentials-6cdd6b98", otherBlob)

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	if len(discovery.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(discovery.Rows))
	}
	unclaimed := rowByID(t, discovery, otherUUID)
	if unclaimed.State.Kind != StateUnclaimed {
		t.Fatalf("state = %q, want unclaimed", unclaimed.State.Kind)
	}
	if !unclaimed.VisibleByDefault {
		t.Error("an unclaimed item is shown")
	}
	for _, row := range discovery.Rows {
		if strings.Contains(row.ID, "86c75be7") {
			t.Errorf("the legacy API-key item became a row: %q", row.ID)
		}
	}
	assertNeverRead(t, fixture, "Claude Code-86c75be7")
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverOldBlobInAForeignDirectoryIsIdentityUnknownAndShown(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	fixture.Dump(testutil.LiveService, "Claude Code-credentials-6cdd6b98")
	fixture.KeychainItem(testutil.LiveService, liveBlob)
	fixture.KeychainItem("Claude Code-credentials-6cdd6b98", oldBlob)

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	unknown := rowByID(t, discovery, "Claude Code-credentials-6cdd6b98")
	if unknown.State.Kind != StateIdentityUnknown {
		t.Fatalf("state = %q, want identity_unknown", unknown.State.Kind)
	}
	if !unknown.VisibleByDefault {
		t.Error("the row stays visible: the fix is actionable")
	}
	if unknown.Record.AccountUUID != "" {
		t.Errorf("account = %q, want no identity invented from the path", unknown.Record.AccountUUID)
	}
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverSwitcherItemIsListedHiddenAndNeverRead(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	fixture.Dump(testutil.LiveService, "claude-switcher:alice@example.com")
	fixture.KeychainItem(testutil.LiveService, liveBlob)

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	foreign := rowByID(t, discovery, "claude-switcher:alice@example.com")
	if foreign.State.Name() != "foreign" {
		t.Fatalf("state = %q: a switcher item is not unclaimed, nothing here is adoptable", foreign.State.Name())
	}
	if foreign.VisibleByDefault {
		t.Error("a switcher item is hidden by default")
	}
	if foreign.Source != SourceNone {
		t.Errorf("source = %q, want none", foreign.Source)
	}
	if foreign.Record.Email == nil || *foreign.Record.Email != "alice@example.com" {
		t.Errorf("email = %v, want alice@example.com", foreign.Record.Email)
	}
	if !strings.Contains(foreign.Note, "claude-account-switcher") {
		t.Errorf("note = %q, want it to name the owner", foreign.Note)
	}
	assertNeverRead(t, fixture, "claude-switcher:")
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverLiveRowFallsBackToClaudeJSONForItsIdentity(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	fixture.Dump(testutil.LiveService)
	fixture.KeychainItem(testutil.LiveService, oldBlob)
	document := `{"oauthAccount":{"accountUuid":"55555555-5555-4555-8555-555555555555","emailAddress":"from-json@example.com","organizationUuid":"66666666-6666-4666-8666-666666666666","organizationName":"JSON Org"}}`
	if err := os.WriteFile(filepath.Join(fixture.Home(), ".claude.json"), []byte(document), 0o600); err != nil {
		t.Fatalf("write .claude.json: %v", err)
	}

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	live := discovery.Rows[0]
	if live.ID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("id = %q, want the identity from .claude.json", live.ID)
	}
	if live.Record.Email == nil || *live.Record.Email != "from-json@example.com" {
		t.Errorf("email = %v, want from-json@example.com", live.Record.Email)
	}
	if live.Record.OrgName == nil || *live.Record.OrgName != "JSON Org" {
		t.Errorf("org name = %v, want JSON Org", live.Record.OrgName)
	}
	if live.State.Kind != StateOK {
		t.Errorf("state = %q: the identity was found, so the row is fine", live.State.Kind)
	}
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverClaudeJSONIsNotConsultedForAnyOtherRow(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	fixture.Dump(testutil.LiveService, "Claude Code-credentials-6cdd6b98")
	fixture.KeychainItem(testutil.LiveService, liveBlob)
	fixture.KeychainItem("Claude Code-credentials-6cdd6b98", oldBlob)
	if err := os.WriteFile(filepath.Join(fixture.Home(), ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"55555555-5555-4555-8555-555555555555"}}`), 0o600); err != nil {
		t.Fatalf("write .claude.json: %v", err)
	}

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	foreign := rowByID(t, discovery, "Claude Code-credentials-6cdd6b98")
	if foreign.State.Kind != StateIdentityUnknown {
		t.Fatalf("state = %q, want identity_unknown", foreign.State.Kind)
	}
	if foreign.Record.AccountUUID == "55555555-5555-4555-8555-555555555555" {
		t.Error(".claude.json was borrowed for a row that is not the live one")
	}
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverLockedKeychainLocksKeychainRowsAndReadsNothing(t *testing.T) {
	fixture := keychainWorld(t)
	t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT", "36")
	reader := keychainReader(t)
	fixture.Dump(testutil.LiveService)
	fixture.KeychainItem(testutil.LiveService, liveBlob)

	discovery := Discover(t.Context(), emptyRegistry(), fixturePaths(fixture), reader, fixtureEnv(fixture))
	if discovery.Preflight.State != secret.KeychainStateLocked {
		t.Fatalf("preflight = %v, want locked", discovery.Preflight)
	}
	live := discovery.Rows[0]
	if live.State.Kind != StateKeychainLocked {
		t.Fatalf("state = %q, want keychain_locked", live.State.Kind)
	}
	if !live.State.IsFailure() {
		t.Error("a locked keychain degrades the run")
	}
	if live.State.AllowsNetwork() {
		t.Error("no request for a row with no token")
	}
	if len(discovery.Listing) != 0 {
		t.Errorf("listing = %d entries, want none from a locked keychain", len(discovery.Listing))
	}
	assertNeverRead(t, fixture, testutil.LiveService)
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverConfigDirRecords(t *testing.T) {
	const service = "Claude Code-credentials-6cdd6b98"
	tests := map[string]struct {
		sharesLiveDir bool
		findExit      string
		wantState     AccountStateKind
		wantVisible   bool
	}{
		"success: a config dir record claims its service so it is not also unclaimed": {
			wantState:   StateOK,
			wantVisible: true,
		},
		"success: a record sharing the live directory is a hidden stale sibling": {
			sharesLiveDir: true,
			wantState:     StateStaleSiblingOfLive,
			wantVisible:   false,
		},
		"error: a failed item read under an unlocked preflight reads as needs login": {
			findExit:    "36",
			wantState:   StateNeedsLogin,
			wantVisible: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := keychainWorld(t)
			if tt.findExit != "" {
				t.Setenv("AGCTL_FAKE_SECURITY_FIND_EXIT", tt.findExit)
			}
			reader := keychainReader(t)
			fixture.Dump(testutil.LiveService, service)
			fixture.KeychainItem(testutil.LiveService, liveBlob)
			fixture.KeychainItem(service, otherBlob)

			record, err := config.NewRecord("acct-9", "org-9", config.AccountKindConfigDirReadOnly("/elsewhere", service, tt.sharesLiveDir))
			if err != nil {
				t.Fatalf("build the record: %v", err)
			}
			registry := &config.Registry{Version: config.RegistryVersion, Accounts: []config.AccountRecord{record}}

			discovery := Discover(t.Context(), registry, fixturePaths(fixture), reader, fixtureEnv(fixture))
			if len(discovery.Rows) != 2 {
				t.Fatalf("rows = %d, want the live row and the claimed one", len(discovery.Rows))
			}
			claimed := rowByID(t, discovery, "acct-9")
			if claimed.State.Kind != tt.wantState {
				t.Fatalf("state = %q, want %q", claimed.State.Kind, tt.wantState)
			}
			if claimed.VisibleByDefault != tt.wantVisible {
				t.Errorf("visible = %v, want %v", claimed.VisibleByDefault, tt.wantVisible)
			}
			fixture.AssertKeychainReadOnly()
		})
	}
}

func TestDiscoverMigratedOwnedNamespaceIsDisplayedFromTheKeychain(t *testing.T) {
	fixture := keychainWorld(t)
	reader := keychainReader(t)
	paths := fixturePaths(fixture)
	fixture.WriteCredentials("acct-1", "org-1", oldBlob)

	nsDir := paths.NamespaceDir("acct-1", "org-1")
	exportSHA8 := testutil.Sha8(testutil.ExportSpelling(nsDir))
	service := testutil.MigrationService(nsDir)
	registry := &config.Registry{Version: config.RegistryVersion, Accounts: []config.AccountRecord{ownedRecord(t, paths, "acct-1", "org-1", exportSHA8)}}

	fixture.Dump(testutil.LiveService, service)
	fixture.KeychainItem(testutil.LiveService, liveBlob)
	fixture.KeychainItem(service, otherBlob)

	discovery := Discover(t.Context(), registry, paths, reader, fixtureEnv(fixture))
	owned := rowByID(t, discovery, "acct-1")
	if owned.State.Kind != StateMigratedToKeychain || owned.State.Service != service {
		t.Fatalf("state = %+v, want migrated to %q", owned.State, service)
	}
	if owned.Source != SourceKeychain {
		t.Errorf("source = %q, want keychain", owned.Source)
	}
	if owned.State.IsFailure() {
		t.Error("a migrated row still shows numbers")
	}
	if owned.Credentials == nil {
		t.Fatal("the keychain item was read")
	}
	identity := owned.Credentials.Identity()
	if identity == nil || identity.AccountUUID != otherUUID {
		t.Errorf("identity = %+v, want the keychain item's, not the file's", identity)
	}
	fixture.AssertKeychainReadOnly()
}

func TestDiscoverOwnedRowSaysWhenTheMigrationProbeCouldNotRun(t *testing.T) {
	fixture := keychainWorld(t)
	t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT", "36")
	reader := keychainReader(t)
	paths := fixturePaths(fixture)
	fixture.WriteCredentials("acct-1", "org-1", otherBlob)
	registry := &config.Registry{Version: config.RegistryVersion, Accounts: []config.AccountRecord{ownedRecord(t, paths, "acct-1", "org-1", "aaaaaaaa")}}

	discovery := Discover(t.Context(), registry, paths, reader, fixtureEnv(fixture))
	owned := rowByID(t, discovery, "acct-1")
	if owned.State.Kind != StateOK {
		t.Fatalf("state = %q: the file is still authoritative for an owned row", owned.State.Kind)
	}
	if !strings.Contains(owned.Note, "migration probe skipped") {
		t.Errorf("note = %q, want the skipped probe named", owned.Note)
	}
	assertNeverRead(t, fixture, "Claude Code")
	fixture.AssertKeychainReadOnly()
}
