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

package commands

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

// wipeMembers are the members every snapshot in these tests carries beside
// the account object, so a wipe that is skipped leaves bytes to find.
const wipeMembers = `"mcpOAuth":{"server|0001":{"accessToken":"` + siblingCanary + `-mcp"}},"pluginSecrets":{"plugin":"` + siblingCanary + `-plugin"}`

// parsedValues keeps aliases of the member values of every credential the
// swap and undo read helpers parse while it is installed.
type parsedValues struct {
	mu          sync.Mutex
	credentials []*claude.Credentials
	values      [][]byte
}

// trackParsed installs a parsedValues for the rest of the test.
func trackParsed(t *testing.T) *parsedValues {
	t.Helper()
	tracked := &parsedValues{}
	observe := tracked.add
	if !useParsedObserver.CompareAndSwap(nil, &observe) {
		t.Fatal("another test is already tracking parsed credentials")
	}
	t.Cleanup(func() { useParsedObserver.Store(nil) })
	return tracked
}

func (p *parsedValues) add(credentials *claude.Credentials) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.credentials = append(p.credentials, credentials)
	for _, members := range [][]claude.ExtraMember{credentials.Siblings, credentials.Extra} {
		for _, member := range members {
			p.values = append(p.values, member.Value)
		}
	}
}

// requireWiped fails unless at least wantCredentials snapshots carrying
// member values were parsed, and every one of them was wiped.
func (p *parsedValues) requireWiped(t *testing.T, wantCredentials int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.credentials) < wantCredentials || len(p.values) < wantCredentials {
		t.Fatalf("parsed %d credentials with %d member values, want at least %d of each: the test proves nothing", len(p.credentials), len(p.values), wantCredentials)
	}
	for i, credentials := range p.credentials {
		if credentials.Siblings != nil || credentials.Extra != nil {
			t.Errorf("parsed credential %d still holds %d siblings and %d unrecognised members", i, len(credentials.Siblings), len(credentials.Extra))
		}
	}
	for i, value := range p.values {
		if nonZero := countNonZero(value); nonZero != 0 {
			t.Errorf("member value %d keeps %d non-zero bytes of %d", i, nonZero, len(value))
		}
	}
}

// capturingRefresher records the credential it is asked to refresh and
// aliases of its member values, then delegates.
type capturingRefresher struct {
	next        claude.TokenRefresher
	credentials *claude.Credentials
	values      [][]byte
}

func (r *capturingRefresher) RefreshAccess(ctx context.Context, credentials *claude.Credentials) (*claude.Credentials, error) {
	r.credentials = credentials
	for _, members := range [][]claude.ExtraMember{credentials.Siblings, credentials.Extra} {
		for _, member := range members {
			r.values = append(r.values, member.Value)
		}
	}
	return r.next.RefreshAccess(ctx, credentials)
}

// wipeSwapWorld is a swap between two owned accounts whose destination
// item carries wipeMembers.
type wipeSwapWorld struct {
	fixture  *testutil.Fixture
	swap     useLiveSwap
	incoming *config.AccountRecord
	store    *config.AccountRecord
	service  string
	posts    *atomic.Int32
}

func newWipeSwapWorld(t *testing.T, live, expired bool, token http.HandlerFunc) *wipeSwapWorld {
	t.Helper()
	fixture := testutil.New(t).WithKeychain()
	fixture.WriteRegistry([]any{fixture.OwnedRecord("owner", "org"), fixture.OwnedRecord("incoming", "org")})
	paths := config.NewPaths(fixture.ConfigDir())
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatal(err)
	}
	registry, err := config.LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := registry.ResolveID("owner")
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := registry.ResolveID("incoming")
	if err != nil {
		t.Fatal(err)
	}
	expires := testutil.FreshAt()
	if expired {
		expires = testutil.ExpiredAt()
	}
	fixture.WriteCredentials("incoming", "org", withMembers(t, fixture.IdentifiedBlob("incoming-access", "incoming-refresh", expires, "incoming", "org"), `"fileMember":{"kept":"`+siblingCanary+`-file"}`))
	outgoing := fixture.IdentifiedBlob("outgoing-access", "outgoing-refresh", testutil.FreshAt(), "owner", "org")
	fixture.WriteCredentials("owner", "org", outgoing)
	ownerDir := fixture.NamespaceDir("owner", "org")
	env := claude.EnvWithHome(fixture.Home())
	if live {
		if err := os.Mkdir(claude.LiveStoreDir(&env), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	subject, refused := useBuildSubject(paths, &env, live, owner, claude.ExportSpelling(ownerDir))
	if refused != nil {
		t.Fatalf("subject refused: %+v", refused)
	}
	fixture.KeychainItem(subject.service, withMembers(t, outgoing, wipeMembers))
	fixture.AllowWrite(subject.service)
	for _, entry := range fixture.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
			t.Setenv(key, value)
		}
	}
	t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
	world := &wipeSwapWorld{fixture: fixture, incoming: incoming, service: subject.service, posts: &atomic.Int32{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			world.posts.Add(1)
			token(w, r)
			return
		}
		account := "owner"
		if strings.Contains(r.Header.Get("Authorization"), "incoming-access") || strings.Contains(r.Header.Get("Authorization"), "rotated-access") {
			account = "incoming"
		}
		if _, err := fmt.Fprintf(w, `{"account":{"uuid":%q,"email":"test@example.invalid"},"organization":{"uuid":"org"}}`, account); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv(claude.TokenURLEnv, server.URL+"/token")
	t.Setenv(claude.ProfileURLEnv, server.URL+"/profile")
	process := SessionProcess{In: strings.NewReader("yes\n"), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Signals: testSignalController(t)}
	world.swap = useLiveSwap{paths: paths, config: registry, env: &env, live: live, inherited: claude.ExportSpelling(ownerDir), process: process}
	if !live {
		world.store = owner
	}
	return world
}

func (w *wipeSwapWorld) run(t *testing.T) *useReport {
	t.Helper()
	return w.swap.swapIn(t.Context(), useIncoming{record: w.incoming, direction: secret.DirectionForward, source: useSource{kind: useSourceOwn}}, w.store, cli.ClaudeUseOptions{Yes: true})
}

func TestUseSwapWipesTheIncomingCredentialWhenItsRefreshFails(t *testing.T) {
	tests := map[string]struct {
		live bool
	}{
		"error: a namespace swap whose refresh is rejected": {},
		"error: a live swap whose refresh is rejected":      {live: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newWipeSwapWorld(t, tt.live, true, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			})
			client, err := claude.NewOAuthClientFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			refresher := &capturingRefresher{next: client}
			world.swap.refresher = refresher
			tracked := trackParsed(t)

			report := world.run(t)

			if report.outcome.Kind != claude.SwapRefused || report.outcome.Refusal.Kind != claude.SwapCannotAdopt {
				t.Fatalf("outcome = %+v, want a cannot-adopt refusal; note=%v", report.outcome, report.note)
			}
			if got := world.posts.Load(); got != 1 {
				t.Fatalf("token requests = %d, want 1", got)
			}
			for _, call := range world.fixture.SecurityLog() {
				if strings.HasPrefix(call, "add-generic-password ") {
					t.Fatalf("a refused swap wrote the item: %s", call)
				}
			}
			if refresher.credentials == nil {
				t.Fatal("the refresh was never asked for")
			}
			// The members carried from the item at the first admission are
			// copies made after the parse, so only the refresher saw them.
			if len(refresher.values) < 2 {
				t.Fatalf("the refreshed credential carried %d member values, want the item's two", len(refresher.values))
			}
			if refresher.credentials.Siblings != nil || refresher.credentials.Extra != nil {
				t.Errorf("the credential whose refresh failed still holds %d siblings and %d unrecognised members", len(refresher.credentials.Siblings), len(refresher.credentials.Extra))
			}
			for i, value := range refresher.values {
				if nonZero := countNonZero(value); nonZero != 0 {
					t.Errorf("carried member value %d keeps %d non-zero bytes of %d", i, nonZero, len(value))
				}
			}
			tracked.requireWiped(t, 2)
		})
	}
}

func TestUseSwapWipesEverySnapshotItParses(t *testing.T) {
	tests := map[string]struct {
		live, expired bool
	}{
		"success: a namespace swap":                      {},
		"success: a namespace swap that refreshes first": {expired: true},
		"success: a live swap that parks the displaced":  {live: true},
		"success: a live swap that refreshes on the way": {live: true, expired: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newWipeSwapWorld(t, tt.live, tt.expired, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":28800,"scope":"user:inference user:profile"}`))
			})
			tracked := trackParsed(t)

			report := world.run(t)

			if report.outcome.Kind != claude.SwapApplied {
				t.Fatalf("outcome = %+v, want applied; note=%v", report.outcome, report.note)
			}
			// The early item read, the incoming file, the held re-read and
			// the readback at least.
			tracked.requireWiped(t, 4)
		})
	}
}

// writeWipeDocument writes a credential file whose document carries
// wipeMembers and an unrecognised account field.
func writeWipeDocument(t *testing.T, path, access, account string) {
	t.Helper()
	inner := map[string]any{"accessToken": access, "refreshToken": "refresh", "expiresAt": int64(4102444800000), "futureField": siblingCanary + "-extra"}
	if account != "" {
		inner["tokenAccount"] = map[string]string{"uuid": account, "organizationUuid": "org"}
	}
	data, err := json.Marshal(map[string]any{"claudeAiOauth": inner})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(withMembers(t, string(data), wipeMembers)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUseHelpersWipeTheSnapshotsTheyRead(t *testing.T) {
	tests := map[string]struct {
		run             func(t *testing.T)
		wantCredentials int
	}{
		"success: a migrated check drops the item it read": {
			run: func(t *testing.T) {
				fixture := testutil.New(t).WithKeychain()
				paths := config.NewPaths(fixture.ConfigDir())
				if err := paths.EnsureDirs(t.Context()); err != nil {
					t.Fatal(err)
				}
				fixture.WriteRegistry([]any{fixture.OwnedRecord("owner", "org")})
				registry, err := config.LoadRegistry(t.Context(), paths)
				if err != nil {
					t.Fatal(err)
				}
				record, err := registry.ResolveID("owner")
				if err != nil {
					t.Fatal(err)
				}
				subject, refused := useBuildSubject(paths, nil, false, record, claude.ExportSpelling(paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)))
				if refused != nil {
					t.Fatalf("subject refused: %+v", refused)
				}
				fixture.KeychainItem(subject.service, withMembers(t, fixture.IdentifiedBlob("owner-access", "owner-refresh", testutil.FreshAt(), "owner", "org"), wipeMembers))
				for _, entry := range fixture.Environ() {
					key, value, _ := strings.Cut(entry, "=")
					if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
						t.Setenv(key, value)
					}
				}
				t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
				if !useMigrated(t.Context(), paths, record) {
					t.Fatal("the item was not found")
				}
			},
			wantCredentials: 1,
		},
		"success: a duplicate adopted copy is compared and removed": {
			run: func(t *testing.T) {
				paths, registry := undoFixture(t)
				dir := paths.NamespaceDir(registry.Accounts[0].AccountUUID, "org")
				writeWipeDocument(t, filepath.Join(dir, secret.CredentialsFile), "restored", "restored")
				writeWipeDocument(t, filepath.Join(dir, secret.AdoptedFile), "restored", "restored")
				reference, err := useReadStored(dir, useSourceAdopted)
				if err != nil {
					t.Fatal(err)
				}
				digests, err := reference.Digests()
				reference.Wipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := useRemoveDuplicateAdopted(paths, dir, digests); err != nil {
					t.Fatal(err)
				}
				if read, err := secret.ReadAdopted(dir); err != nil || read.Present {
					t.Fatalf("the duplicate adopted copy was kept: %v", err)
				}
			},
			wantCredentials: 3,
		},
		"success: a different adopted copy is compared and kept": {
			run: func(t *testing.T) {
				paths, registry := undoFixture(t)
				dir := paths.NamespaceDir(registry.Accounts[0].AccountUUID, "org")
				writeWipeDocument(t, filepath.Join(dir, secret.CredentialsFile), "restored", "restored")
				writeWipeDocument(t, filepath.Join(dir, secret.AdoptedFile), "other", "restored")
				reference, err := useReadStored(dir, useSourceAdopted)
				if err != nil {
					t.Fatal(err)
				}
				digests, err := reference.Digests()
				reference.Wipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := useRemoveDuplicateAdopted(paths, dir, digests); err != nil {
					t.Fatal(err)
				}
				if read, err := secret.ReadAdopted(dir); err != nil || !read.Present {
					t.Fatalf("the sole adopted copy was removed: %v", err)
				}
			},
			wantCredentials: 2,
		},
		"success: a namespace reversal reads the adopted copy": {
			run: func(t *testing.T) {
				paths, registry := undoFixture(t)
				store := registry.Accounts[1]
				entry := auditUndoEntry(t, paths, &secret.WriteEvent{Target: secret.NamespaceTarget(store.Kind.Owned.ExportSHA8), FromDigest8: new(undoPrefix("displaced")), ToDigest8: undoPrefix("incoming"), Outcome: secret.WriteApplied})
				writeWipeDocument(t, filepath.Join(paths.NamespaceDir(store.AccountUUID, "org"), secret.AdoptedFile), "displaced", "restored")
				if _, err := namespacedReversal(t.Context(), paths, registry, entry); err != nil {
					t.Fatal(err)
				}
			},
			wantCredentials: 1,
		},
		"error: a namespace reversal refuses a copy it cannot match": {
			run: func(t *testing.T) {
				paths, registry := undoFixture(t)
				store := registry.Accounts[1]
				entry := auditUndoEntry(t, paths, &secret.WriteEvent{Target: secret.NamespaceTarget(store.Kind.Owned.ExportSHA8), FromDigest8: new(undoPrefix("displaced")), ToDigest8: undoPrefix("incoming"), Outcome: secret.WriteApplied})
				writeWipeDocument(t, filepath.Join(paths.NamespaceDir(store.AccountUUID, "org"), secret.AdoptedFile), "other", "restored")
				if _, err := namespacedReversal(t.Context(), paths, registry, entry); err == nil {
					t.Fatal("a copy the swap did not displace was accepted")
				}
			},
			wantCredentials: 1,
		},
		"success: a live reversal scans every namespace": {
			run: func(t *testing.T) {
				paths, registry := undoFixture(t)
				entry := auditUndoEntry(t, paths, &secret.WriteEvent{Target: secret.TargetLive, FromDigest8: new(undoPrefix("displaced")), ToDigest8: undoPrefix("incoming"), Outcome: secret.WriteApplied, Direction: secret.DirectionForward, IncomingIdentity: &secret.IncomingIdentity{AccountUUID: "installed", OrganizationUUID: new("org")}})
				restored := paths.NamespaceDir("restored", "org")
				writeWipeDocument(t, filepath.Join(restored, secret.AdoptedFile), "displaced", "restored")
				writeWipeDocument(t, filepath.Join(restored, secret.CredentialsFile), "displaced", "restored")
				writeWipeDocument(t, filepath.Join(paths.NamespaceDir("third", "org"), secret.CredentialsFile), "unrelated", "third")
				if _, err := liveReversal(t.Context(), paths, registry, entry); err != nil {
					t.Fatal(err)
				}
			},
			wantCredentials: 3,
		},
		"error: a live reversal refuses an ambiguous copy": {
			run: func(t *testing.T) {
				paths, registry := undoFixture(t)
				entry := auditUndoEntry(t, paths, &secret.WriteEvent{Target: secret.TargetLive, FromDigest8: new(undoPrefix("displaced")), ToDigest8: undoPrefix("incoming"), Outcome: secret.WriteApplied, Direction: secret.DirectionForward, IncomingIdentity: &secret.IncomingIdentity{AccountUUID: "installed", OrganizationUUID: new("org")}})
				writeWipeDocument(t, filepath.Join(paths.NamespaceDir("restored", "org"), secret.AdoptedFile), "displaced", "")
				writeWipeDocument(t, filepath.Join(paths.NamespaceDir("third", "org"), secret.AdoptedFile), "displaced", "")
				if _, err := liveReversal(t.Context(), paths, registry, entry); err == nil {
					t.Fatal("two matching copies were not refused")
				}
			},
			wantCredentials: 2,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tracked := trackParsed(t)
			tt.run(t)
			tracked.requireWiped(t, tt.wantCredentials)
		})
	}
}

func TestUseAdoptionWipesTheSnapshotsItInspects(t *testing.T) {
	type world struct {
		paths              *config.Paths
		displaced          *claude.Credentials
		incoming           *claude.Credentials
		owner, other, peer *config.AccountRecord
	}
	newWorld := func(t *testing.T, displacedAccount string) *world {
		t.Helper()
		paths, registry := undoFixture(t)
		parse := func(access, account string) *claude.Credentials {
			credentials, err := claude.ParseBlob([]byte(`{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"refresh","expiresAt":4102444800000,"tokenAccount":{"uuid":"` + account + `","organizationUuid":"org"}}}`))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(credentials.Wipe)
			return credentials
		}
		return &world{paths: paths, displaced: parse("displaced", displacedAccount), incoming: parse("incoming", "installed"), owner: &registry.Accounts[0], other: &registry.Accounts[1], peer: &registry.Accounts[2]}
	}
	dirOf := func(w *world, record *config.AccountRecord) string {
		return w.paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
	}
	tests := map[string]struct {
		run             func(t *testing.T)
		wantCredentials int
	}{
		"success: a namespace undo inspects the adopted copy it would stage over": {
			run: func(t *testing.T) {
				w := newWorld(t, "restored")
				dir := dirOf(w, w.owner)
				writeWipeDocument(t, filepath.Join(dir, secret.AdoptedFile), "parked", "restored")
				_, _ = useDecideAdoption(t.Context(), useAdoptionParties{paths: w.paths, store: w.owner, subject: &useSubject{storeDir: dir, tree: secret.TreeOwn}, incoming: w.other, incomingCredentials: w.incoming, identity: w.displaced.Identity(), displaced: w.displaced, direction: secret.DirectionUndo})
			},
			wantCredentials: 1,
		},
		"success: a displaced copy of the incoming account inspects its adopted copy": {
			run: func(t *testing.T) {
				w := newWorld(t, "installed")
				dir := dirOf(w, w.owner)
				writeWipeDocument(t, filepath.Join(dir, secret.AdoptedFile), "parked", "installed")
				_, _ = useDecideAdoption(t.Context(), useAdoptionParties{paths: w.paths, store: w.owner, subject: &useSubject{storeDir: dir, tree: secret.TreeOwn}, incoming: w.other, incomingCredentials: w.incoming, identity: w.displaced.Identity(), displaced: w.displaced, direction: secret.DirectionForward})
			},
			wantCredentials: 1,
		},
		"success: a store's own account inspects its adopted copy": {
			run: func(t *testing.T) {
				w := newWorld(t, "restored")
				dir := dirOf(w, w.owner)
				writeWipeDocument(t, filepath.Join(dir, secret.AdoptedFile), "parked", "restored")
				_, _ = useDecideAdoption(t.Context(), useAdoptionParties{paths: w.paths, store: w.owner, subject: &useSubject{storeDir: dir, tree: secret.TreeOwn}, incoming: w.other, incomingCredentials: w.incoming, identity: w.displaced.Identity(), displaced: w.displaced, direction: secret.DirectionForward})
			},
			wantCredentials: 1,
		},
		"error: a live swap inspects both of the third namespace's copies": {
			run: func(t *testing.T) {
				w := newWorld(t, "third")
				dir := dirOf(w, w.peer)
				writeWipeDocument(t, filepath.Join(dir, secret.CredentialsFile), "independent", "third")
				writeWipeDocument(t, filepath.Join(dir, secret.AdoptedFile), "namespace-undo-source", "third")
				_, refused := useDecideAdoption(t.Context(), useAdoptionParties{paths: w.paths, subject: &useSubject{storeDir: t.TempDir(), tree: secret.TreeLive}, incoming: w.other, incomingCredentials: w.incoming, third: w.peer, identity: w.displaced.Identity(), displaced: w.displaced, live: true, direction: secret.DirectionForward})
				if refused == nil {
					t.Fatal("an adopted copy no live swap parked was not refused")
				}
			},
			wantCredentials: 2,
		},
		"success: a namespace swap inspects the third namespace's own copy": {
			run: func(t *testing.T) {
				w := newWorld(t, "third")
				writeWipeDocument(t, filepath.Join(dirOf(w, w.peer), secret.CredentialsFile), "independent", "third")
				_, _ = useDecideAdoption(t.Context(), useAdoptionParties{paths: w.paths, store: w.owner, subject: &useSubject{storeDir: dirOf(w, w.owner), tree: secret.TreeOwn}, incoming: w.other, incomingCredentials: w.incoming, third: w.peer, identity: w.displaced.Identity(), displaced: w.displaced, direction: secret.DirectionForward})
			},
			wantCredentials: 1,
		},
		"success: a third-store adoption rereads the store it replaces": {
			run: func(t *testing.T) {
				w := newWorld(t, "third")
				dir := dirOf(w, w.peer)
				writeWipeDocument(t, filepath.Join(dir, secret.CredentialsFile), "independent", "third")
				prior, err := useReadStored(dir, useSourceOwn)
				if err != nil {
					t.Fatal(err)
				}
				digests, err := prior.Digests()
				prior.Wipe()
				if err != nil {
					t.Fatal(err)
				}
				if _, _, refusal := usePerformAdoption(t.Context(), w.paths, useAdoptionPlan{kind: useAdoptionThirdStore, dir: dir, prior: &digests}, w.displaced); refusal != "" {
					t.Fatalf("refusal = %s", refusal)
				}
			},
			wantCredentials: 2,
		},
		"success: a refreshed undo source is compared with its adopted copy": {
			run: func(t *testing.T) {
				paths, registry := undoFixture(t)
				dir := paths.NamespaceDir(registry.Accounts[0].AccountUUID, "org")
				writeWipeDocument(t, filepath.Join(dir, secret.AdoptedFile), "parked", "restored")
				reference, err := useReadStored(dir, useSourceAdopted)
				if err != nil {
					t.Fatal(err)
				}
				digests, err := reference.Digests()
				reference.Wipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := useAdoptedTakesRefresh(dir, digests); err != nil {
					t.Fatal(err)
				}
			},
			wantCredentials: 2,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tracked := trackParsed(t)
			tt.run(t)
			tracked.requireWiped(t, tt.wantCredentials)
		})
	}
}
