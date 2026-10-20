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
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
)

// otherBlob names an account that is not the live one, with a full
// identity block.
const otherBlob = `{"claudeAiOauth":{"accessToken":"other-access","refreshToken":"other-refresh","expiresAt":9999999999999,"tokenAccount":{"uuid":"33333333-3333-4333-8333-333333333333","emailAddress":"other@example.com","organizationUuid":"44444444-4444-4444-8444-444444444444"}}}`

// testStore builds a store root and the environment pointing at an
// isolated home.
func testStore(t *testing.T) (*config.Paths, *EnvView) {
	t.Helper()
	home := t.TempDir()
	env := EnvWithHome(home)
	return config.NewPaths(filepath.Join(home, "agentctl-store")), &env
}

// rowByID finds one discovered row, failing the test when it is missing.
func rowByID(t *testing.T, discovery Discovery, id string) AccountRow {
	t.Helper()
	for _, row := range discovery.Rows {
		if row.ID == id {
			return row
		}
	}
	ids := make([]string, 0, len(discovery.Rows))
	for _, row := range discovery.Rows {
		ids = append(ids, row.ID)
	}
	t.Fatalf("no row %q among %v", id, ids)
	return AccountRow{}
}

// ownedRecord builds one owned registry record rooted in paths.
func ownedRecord(t *testing.T, paths *config.Paths, acct, org, exportSHA8 string) config.AccountRecord {
	t.Helper()
	nsDir := paths.NamespaceDir(acct, org)
	record, err := config.NewRecord(acct, org, config.AccountKindOwned(ExportSpelling(nsDir), exportSHA8))
	if err != nil {
		t.Fatalf("build the owned record: %v", err)
	}
	return record
}

// writeCredentialsFile seeds one namespace's credential file.
func writeCredentialsFile(t *testing.T, paths *config.Paths, acct, org, blob string) string {
	t.Helper()
	nsDir := paths.NamespaceDir(acct, org)
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		t.Fatalf("create the namespace: %v", err)
	}
	path := filepath.Join(nsDir, CredentialsFileName)
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatalf("write the credential file: %v", err)
	}
	return path
}

func TestDiscoverOwnedRows(t *testing.T) {
	tests := map[string]struct {
		prepare   func(t *testing.T, paths *config.Paths)
		wantState AccountStateKind
		wantSrc   Source
		wantCreds bool
		wantNote  string
	}{
		"success: a readable file is the row's source": {
			prepare: func(t *testing.T, paths *config.Paths) {
				t.Helper()
				writeCredentialsFile(t, paths, "acct-1", "org-1", otherBlob)
			},
			wantState: StateOK,
			wantSrc:   SourceFile,
			wantCreds: true,
			wantNote:  migrationProbeSkipped,
		},
		"error: an absent file needs a login": {
			prepare:   func(t *testing.T, paths *config.Paths) { t.Helper() },
			wantState: StateNeedsLogin,
			wantSrc:   SourceNone,
			wantNote:  migrationProbeSkipped + " (migration unknown)",
		},
		"error: a symlinked credential path is transient, never followed": {
			prepare: func(t *testing.T, paths *config.Paths) {
				t.Helper()
				nsDir := paths.NamespaceDir("acct-1", "org-1")
				if err := os.MkdirAll(nsDir, 0o700); err != nil {
					t.Fatalf("create the namespace: %v", err)
				}
				if err := os.Symlink(filepath.Join(nsDir, "elsewhere"), filepath.Join(nsDir, CredentialsFileName)); err != nil {
					t.Fatalf("plant the symlink: %v", err)
				}
			},
			wantState: StateError,
			wantSrc:   SourceNone,
			wantNote:  migrationProbeSkipped + " (migration unknown)",
		},
		"error: a peer lock artefact reports the session with the file still read": {
			prepare: func(t *testing.T, paths *config.Paths) {
				t.Helper()
				nsDir := paths.NamespaceDir("acct-1", "org-1")
				writeCredentialsFile(t, paths, "acct-1", "org-1", otherBlob)
				if err := os.WriteFile(filepath.Join(nsDir, refreshLockName), nil, 0o600); err != nil {
					t.Fatalf("write the lock artefact: %v", err)
				}
			},
			wantState: StateClaudeSessionDetected,
			wantSrc:   SourceFile,
			wantCreds: true,
			wantNote:  migrationProbeSkipped,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, env := testStore(t)
			tt.prepare(t, paths)
			registry := &config.Registry{Version: config.RegistryVersion, Accounts: []config.AccountRecord{ownedRecord(t, paths, "acct-1", "org-1", "aaaaaaaa")}}

			// The disabled reader's preflight is unavailable, so the
			// migration probe cannot run and every owned row carries the
			// note saying so; the file stays authoritative regardless.
			discovery := Discover(t.Context(), registry, paths, secret.DisabledReader{}, env)
			owned := rowByID(t, discovery, "acct-1")
			if owned.State.Kind != tt.wantState {
				t.Fatalf("state = %q, want %q (label %q)", owned.State.Kind, tt.wantState, owned.State.Label())
			}
			if owned.Source != tt.wantSrc {
				t.Errorf("source = %q, want %q", owned.Source, tt.wantSrc)
			}
			if got := owned.Credentials != nil; got != tt.wantCreds {
				t.Errorf("credentials present = %v, want %v", got, tt.wantCreds)
			}
			if owned.Note != tt.wantNote {
				t.Errorf("note = %q, want %q", owned.Note, tt.wantNote)
			}
			if tt.wantState == StateClaudeSessionDetected && owned.State.Lock != refreshLockName {
				t.Errorf("lock = %q, want %q", owned.State.Lock, refreshLockName)
			}
		})
	}
}

func TestDiscoverRegistryRecords(t *testing.T) {
	foreign, err := config.NewRecord("acct-1", "org-1", config.AccountKindForeign("claude-switcher"))
	if err != nil {
		t.Fatalf("build the foreign record: %v", err)
	}
	hidden := foreign
	hidden.Forgotten = true

	tests := map[string]struct {
		record      config.AccountRecord
		wantState   AccountStateKind
		wantVisible bool
		wantNote    string
	}{
		"success: a foreign record says whose it is": {
			record:      foreign,
			wantState:   StateForeign,
			wantVisible: true,
			wantNote:    "belongs to claude-switcher",
		},
		"success: a forgotten record is hidden": {
			record:      hidden,
			wantState:   StateForgotten,
			wantVisible: false,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, env := testStore(t)
			registry := &config.Registry{Version: config.RegistryVersion, Accounts: []config.AccountRecord{tt.record}}
			discovery := Discover(t.Context(), registry, paths, secret.DisabledReader{}, env)

			row := rowByID(t, discovery, "acct-1")
			if row.State.Kind != tt.wantState {
				t.Fatalf("state = %q, want %q", row.State.Kind, tt.wantState)
			}
			if row.VisibleByDefault != tt.wantVisible {
				t.Errorf("visible = %v, want %v", row.VisibleByDefault, tt.wantVisible)
			}
			if row.Note != tt.wantNote {
				t.Errorf("note = %q, want %q", row.Note, tt.wantNote)
			}
			if row.State.IsFailure() {
				t.Errorf("state %q must not drive the exit status", row.State.Kind)
			}
		})
	}
}

func TestDiscoverEnvTokenRow(t *testing.T) {
	paths, env := testStore(t)
	env.OAuthTokenSet = true

	discovery := Discover(t.Context(), &config.Registry{Version: config.RegistryVersion}, paths, secret.DisabledReader{}, env)
	row := rowByID(t, discovery, "env")
	if row.State.Kind != StateEnvToken {
		t.Fatalf("state = %q, want %q", row.State.Kind, StateEnvToken)
	}
	if row.Source != SourceEnv {
		t.Errorf("source = %q, want %q", row.Source, SourceEnv)
	}
	if !row.VisibleByDefault {
		t.Error("the env row is shown by default")
	}
	if row.State.IsFailure() {
		t.Error("an env token is information, not a failure")
	}
	if row.Credentials != nil {
		t.Error("the env row carries no credential")
	}
	if !strings.Contains(row.Note, OAuthTokenEnv) {
		t.Errorf("note = %q, want it to name %s", row.Note, OAuthTokenEnv)
	}
}

func TestDiscoverCancelledPassReturnsWhatItHasSoFar(t *testing.T) {
	paths, env := testStore(t)
	record, err := config.NewRecord("acct-1", "org-1", config.AccountKindForeign("x"))
	if err != nil {
		t.Fatalf("build the record: %v", err)
	}
	registry := &config.Registry{Version: config.RegistryVersion, Accounts: []config.AccountRecord{record}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	discovery := Discover(ctx, registry, paths, secret.DisabledReader{}, env)
	if len(discovery.Rows) != 1 {
		t.Fatalf("rows = %d, want only the live row before the stop", len(discovery.Rows))
	}
}

func TestClaudeJSONIdentity(t *testing.T) {
	tests := map[string]struct {
		document string
		want     *Identity
	}{
		"success: keys outside oauthAccount are ignored rather than parsed": {
			document: `{"projects":{"/a":{"history":[1,2,3]}},"oauthAccount":{"accountUuid":"55555555-5555-4555-8555-555555555555","emailAddress":"json@example.com","unexpected":{"nested":true}},"tipsHistory":{}}`,
			want: &Identity{
				AccountUUID: "55555555-5555-4555-8555-555555555555",
				Email:       new("json@example.com"),
			},
		},
		"error: an oauthAccount without a uuid is no identity": {
			document: `{"oauthAccount":{"emailAddress":"nameless@example.com"}}`,
			want:     nil,
		},
		"error: a document that is not JSON is no identity": {
			document: `{"oauthAccount":`,
			want:     nil,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".claude.json")
			if err := os.WriteFile(path, []byte(tt.document), 0o600); err != nil {
				t.Fatalf("write the document: %v", err)
			}
			got := claudeJSONIdentity(path)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("identity mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClaudeJSONIdentityFollowsASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-config", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatalf("create the real directory: %v", err)
	}
	if err := os.WriteFile(real, []byte(`{"oauthAccount":{"accountUuid":"99999999-9999-4999-8999-999999999999"}}`), 0o600); err != nil {
		t.Fatalf("write the document: %v", err)
	}
	link := filepath.Join(root, ".claude.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	identity := claudeJSONIdentity(link)
	if identity == nil || identity.AccountUUID != "99999999-9999-4999-8999-999999999999" {
		t.Fatalf("identity = %+v, want the one behind the link", identity)
	}
}

func TestClaudeJSONIdentityRefusesAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the file: %v", err)
	}
	if _, err := file.WriteString(`{"oauthAccount":{"accountUuid":"55555555-5555-4555-8555-555555555555"}}`); err != nil {
		t.Fatalf("write the head: %v", err)
	}
	// A sparse tail keeps the fixture fast while still breaking the cap.
	if err := file.Truncate(MaxClaudeJSONBytes + 1); err != nil {
		t.Fatalf("grow the file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close the file: %v", err)
	}

	if identity := claudeJSONIdentity(path); identity != nil {
		t.Fatalf("identity = %+v, want nil past the size cap", identity)
	}
}

func TestClaudeJSONIdentityMemo(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte(`{"oauthAccount":{"accountUuid":"55555555-5555-4555-8555-555555555555"}}`), 0o600); err != nil {
		t.Fatalf("write the document: %v", err)
	}

	first := claudeJSONIdentity(path)
	if first == nil || first.AccountUUID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("identity = %+v, want the seeded account", first)
	}

	// Poison the memo with an answer the file does not contain: getting it
	// back proves the bytes were not re-read.
	claudeJSONMemo.Lock()
	if !claudeJSONMemo.valid || claudeJSONMemo.path != path {
		claudeJSONMemo.Unlock()
		t.Fatal("the first read did not populate the memo")
	}
	claudeJSONMemo.identity = &Identity{AccountUUID: "memo-hit"}
	claudeJSONMemo.Unlock()

	if got := claudeJSONIdentity(path); got == nil || got.AccountUUID != "memo-hit" {
		t.Fatalf("identity = %+v, want the memo hit: the file was parsed again", got)
	}

	// A change to the file invalidates it: same length, new mtime and a
	// new inode, which is what a rewrite by the peer looks like.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the file: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"oauthAccount":{"accountUuid":"77777777-7777-4777-8777-777777777777"}}`), 0o600); err != nil {
		t.Fatalf("rewrite the file: %v", err)
	}
	if got := claudeJSONIdentity(path); got == nil || got.AccountUUID != "77777777-7777-4777-8777-777777777777" {
		t.Fatalf("identity = %+v, want the rewritten account", got)
	}
}

// ownedSiblingBlob is otherBlob with a top-level mcpOAuth member beside the
// account object, the shape Claude Code's plaintext fallback writes.
const ownedSiblingBlob = `{"claudeAiOauth":{"accessToken":"other-access","refreshToken":"other-refresh","expiresAt":9999999999999,"tokenAccount":{"uuid":"33333333-3333-4333-8333-333333333333","emailAddress":"other@example.com","organizationUuid":"44444444-4444-4444-8444-444444444444"}},"mcpOAuth":{"srv|0123456789abcdef":{"accessToken":"mcp-token"}}}`

func TestReadOwnedCredentialsParsesTheFileAndDigestsItsBytes(t *testing.T) {
	type summary struct {
		Present      bool
		AccountUUID  string
		ExpiresAt    int64
		Siblings     []memberShape
		Digest       [sha256.Size]byte
		NamespaceDir string
	}
	tests := map[string]struct {
		// blob is the file's content; empty writes no file.
		blob string
		// noDir leaves the namespace directory itself absent.
		noDir bool
		want  func(t *testing.T) summary
	}{
		"success: a present file is parsed and digested as read": {
			blob: otherBlob,
			want: func(t *testing.T) summary {
				t.Helper()
				return summary{Present: true, AccountUUID: "33333333-3333-4333-8333-333333333333", ExpiresAt: 9999999999999, Siblings: []memberShape{}, Digest: sha256.Sum256([]byte(otherBlob))}
			},
		},
		"success: a top-level mcpOAuth member is kept in Siblings": {
			blob: ownedSiblingBlob,
			want: func(t *testing.T) summary {
				t.Helper()
				siblings := memberShapes(t, []ExtraMember{{Name: "mcpOAuth", Value: jsontext.Value(`{"srv|0123456789abcdef":{"accessToken":"mcp-token"}}`)}})
				return summary{Present: true, AccountUUID: "33333333-3333-4333-8333-333333333333", ExpiresAt: 9999999999999, Siblings: siblings, Digest: sha256.Sum256([]byte(ownedSiblingBlob))}
			},
		},
		"success: an absent file is nothing, with a zero digest": {
			want: func(t *testing.T) summary {
				t.Helper()
				return summary{}
			},
		},
		"success: an absent namespace directory is nothing, with a zero digest": {
			noDir: true,
			want: func(t *testing.T) summary {
				t.Helper()
				return summary{}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _ := testStore(t)
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatalf("create the store: %v", err)
			}
			nsDir := paths.NamespaceDir("account", "organization")
			switch {
			case tt.blob != "":
				writeCredentialsFile(t, paths, "account", "organization", tt.blob)
			case !tt.noDir:
				if err := os.MkdirAll(nsDir, 0o700); err != nil {
					t.Fatalf("create the namespace: %v", err)
				}
			}

			credentials, digest, err := ReadOwnedCredentials(paths, nsDir)
			if err != nil {
				t.Fatalf("ReadOwnedCredentials(%s) = %v, want no error", nsDir, err)
			}
			got := summary{Digest: digest}
			if credentials != nil {
				t.Cleanup(credentials.Wipe)
				got.Present = true
				got.ExpiresAt = credentials.ExpiresAtMillis
				if identity := credentials.TokenAccount; identity != nil && identity.UUID != nil {
					got.AccountUUID = *identity.UUID
				}
				got.Siblings = memberShapes(t, credentials.Siblings)
			}
			if diff := gocmp.Diff(tt.want(t), got); diff != "" {
				t.Fatalf("read of %s (-want +got):\n%s", nsDir, diff)
			}
		})
	}
}

func TestReadOwnedCredentialsDigestCoversTheWholeDocument(t *testing.T) {
	// The digest is what tells a later read that the file changed. A
	// change confined to a sibling or to the expiry leaves the account's
	// tokens equal, so a digest over the tokens alone would miss it.
	expiryChanged := strings.Replace(otherBlob, `"expiresAt":9999999999999`, `"expiresAt":9999999999998`, 1)
	siblingChanged := strings.Replace(ownedSiblingBlob, `"mcp-token"`, `"mcp-tokeN"`, 1)
	tests := map[string]struct {
		first, second string
		wantEqual     bool
	}{
		"success: the same bytes give the same digest":      {first: ownedSiblingBlob, second: ownedSiblingBlob, wantEqual: true},
		"success: files differing only in a sibling member": {first: ownedSiblingBlob, second: siblingChanged},
		"success: files differing only in expiresAt":        {first: otherBlob, second: expiryChanged},
		"success: files differing only by an added sibling": {first: otherBlob, second: ownedSiblingBlob},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			read := func(blob string) [sha256.Size]byte {
				t.Helper()
				paths, _ := testStore(t)
				path := writeCredentialsFile(t, paths, "account", "organization", blob)
				credentials, digest, err := ReadOwnedCredentials(paths, filepath.Dir(path))
				if err != nil || credentials == nil {
					t.Fatalf("ReadOwnedCredentials = (present %t, %v), want a credential", credentials != nil, err)
				}
				credentials.Wipe()
				if want := sha256.Sum256([]byte(blob)); digest != want {
					t.Fatalf("digest = %x, want the sha256 of the file's bytes %x", digest, want)
				}
				return digest
			}
			first, second := read(tt.first), read(tt.second)
			if got := first == second; got != tt.wantEqual {
				t.Fatalf("digests equal = %t, want %t (%x vs %x)", got, tt.wantEqual, first, second)
			}
		})
	}
}

func TestReadOwnedCredentialsRefusesWhatDiscoveryRefuses(t *testing.T) {
	const canary = "OWNED-FILE-CANARY-7d3e"
	tests := map[string]struct {
		// prepare plants the case and returns the directory to read.
		prepare func(t *testing.T, paths *config.Paths) string
		// wantRead says whether the file's bytes were read before the
		// refusal, so the wipe of that buffer can be checked.
		wantRead    bool
		wantOutside bool
	}{
		"error: a symbolic link at the file is not followed": {
			prepare: func(t *testing.T, paths *config.Paths) string {
				t.Helper()
				path := writeCredentialsFile(t, paths, "account", "organization", otherBlob)
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Fatal(err)
				}
				return filepath.Dir(path)
			},
		},
		"error: a symbolic link at the directory is not followed": {
			prepare: func(t *testing.T, paths *config.Paths) string {
				t.Helper()
				dir := filepath.Dir(writeCredentialsFile(t, paths, "account", "organization", otherBlob))
				if err := os.Rename(dir, dir+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+".saved", dir); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		"error: a directory outside the namespace root is refused": {
			prepare: func(t *testing.T, paths *config.Paths) string {
				t.Helper()
				writeCredentialsFile(t, paths, "account", "organization", otherBlob)
				outside := filepath.Join(t.TempDir(), "elsewhere")
				if err := os.MkdirAll(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(outside, CredentialsFileName), []byte(otherBlob), 0o600); err != nil {
					t.Fatal(err)
				}
				return outside
			},
			wantOutside: true,
		},
		"error: a file that is not JSON is refused and its bytes wiped": {
			prepare: func(t *testing.T, paths *config.Paths) string {
				t.Helper()
				return filepath.Dir(writeCredentialsFile(t, paths, "account", "organization", `{"claudeAiOauth":{"accessToken":"`+canary+`"`))
			},
			wantRead: true,
		},
		"error: a file without an access token is refused and its bytes wiped": {
			prepare: func(t *testing.T, paths *config.Paths) string {
				t.Helper()
				return filepath.Dir(writeCredentialsFile(t, paths, "account", "organization", `{"claudeAiOauth":{"refreshToken":"`+canary+`","expiresAt":5},"mcpOAuth":{"a":{"accessToken":"`+canary+`"}}}`))
			},
			wantRead: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _ := testStore(t)
			nsDir := tt.prepare(t, paths)
			var buffers [][]byte
			observe := func(read []byte) { buffers = append(buffers, read) }

			credentials, digest, err := readOwnedCredentials(paths, nsDir, observe)
			if err == nil || credentials != nil {
				t.Fatalf("readOwnedCredentials(%s) = (present %t, %v), want a refusal", nsDir, credentials != nil, err)
			}
			if digest != ([sha256.Size]byte{}) {
				t.Errorf("a refused read returned the digest %x, want zero", digest)
			}
			var outside *secret.OutsideRootError
			if got := errors.As(err, &outside); got != tt.wantOutside {
				t.Errorf("refusal %v is an outside-root refusal: %t, want %t", err, got, tt.wantOutside)
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("the refusal repeats the file's content: %v", err)
			}
			if got := len(buffers) == 1 && len(buffers[0]) > 0; got != tt.wantRead {
				t.Fatalf("the file's bytes were read: %t (%d buffers), want %t", got, len(buffers), tt.wantRead)
			}
			if n := nonZeroBytes(buffers...); n != 0 {
				t.Fatalf("%d non-zero bytes left in the read buffer after the refusal", n)
			}
		})
	}
}

func TestReadOwnedCredentialsWipesTheReadBufferOnSuccess(t *testing.T) {
	paths, _ := testStore(t)
	path := writeCredentialsFile(t, paths, "account", "organization", ownedSiblingBlob)
	var buffers [][]byte
	credentials, _, err := readOwnedCredentials(paths, filepath.Dir(path), func(read []byte) { buffers = append(buffers, read) })
	if err != nil || credentials == nil {
		t.Fatalf("readOwnedCredentials = (present %t, %v), want a credential", credentials != nil, err)
	}
	credentials.Wipe()
	if len(buffers) != 1 || len(buffers[0]) != len(ownedSiblingBlob) {
		t.Fatalf("the read should hand over one %d-byte buffer, got %d buffers", len(ownedSiblingBlob), len(buffers))
	}
	if n := nonZeroBytes(buffers...); n != 0 {
		t.Fatalf("%d non-zero bytes left in the read buffer once the credential was returned", n)
	}
}
