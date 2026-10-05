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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestUseSharedEngineOrderingAndContainment(t *testing.T) {
	tests := map[string]struct {
		live               bool
		absent             bool
		empty              bool
		expired            bool
		decline            bool
		envToken           bool
		auditRefused       bool
		mismatch           bool
		profileUnavailable bool
		same               bool
		newer              bool
		busy               bool
		kind               claude.SwapOutcomeKind
		refusal            claude.SwapRefusalKind
		posts              int32
		profiles           int32
		writes             int
	}{
		"success: live parks displaced grant outside live tree":                {live: true, kind: claude.SwapApplied, profiles: 2, writes: 1},
		"success: namespace first write adopts before removing shadow":         {absent: true, kind: claude.SwapApplied, writes: 1},
		"success: empty namespace first write has absent item baseline":        {absent: true, empty: true, kind: claude.SwapApplied, writes: 1},
		"success: expired incoming refreshes and saves after consent":          {expired: true, kind: claude.SwapApplied, posts: 1, writes: 1},
		"success: identical unmigrated namespace never migrates":               {absent: true, same: true, kind: claude.SwapAlreadyActive},
		"success: newer incoming-account live copy never installs stale grant": {live: true, newer: true, kind: claude.SwapAlreadyActive, profiles: 1},
		"error: absent live item never falls back to plaintext":                {live: true, absent: true, kind: claude.SwapRefused, refusal: claude.SwapLiveItemAbsent},
		"error: json plan is not consent and never rotates expired grant":      {expired: true, decline: true, kind: claude.SwapCancelled},
		"error: environment override wins before keychain reads":               {live: true, envToken: true, kind: claude.SwapRefused, refusal: claude.SwapEnvToken},
		"error: live audit gate wins before consent or refresh":                {live: true, auditRefused: true, expired: true, kind: claude.SwapRefused, refusal: claude.SwapAuditRefused, profiles: 1},
		"error: installed profile mismatch refuses before adoption":            {live: true, mismatch: true, kind: claude.SwapRefused, refusal: claude.SwapCannotAdopt, profiles: 2},
		"success: unavailable installed profile does not block owned write":    {live: true, profileUnavailable: true, kind: claude.SwapApplied, profiles: 2, writes: 1},
		"error: peer busy occurs only after outgoing copy is preserved":        {live: true, busy: true, kind: claude.SwapBusy, profiles: 2},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
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
			if test.expired {
				expires = testutil.ExpiredAt()
			}
			incomingBlob := fixture.IdentifiedBlob("incoming-access", "incoming-refresh", expires, "incoming", "org")
			fixture.WriteCredentials("incoming", "org", incomingBlob)
			outgoing := fixture.IdentifiedBlob("outgoing-access", "outgoing-refresh", testutil.FreshAt(), "owner", "org")
			if test.same {
				outgoing = incomingBlob
			}
			if test.newer {
				outgoing = fixture.IdentifiedBlob("outgoing-access", "outgoing-refresh", testutil.FreshAt()+60000, "incoming", "org")
			}
			ownerDir := fixture.NamespaceDir("owner", "org")
			if !test.empty {
				fixture.WriteCredentials("owner", "org", outgoing)
			}
			env := claude.EnvWithHome(fixture.Home())
			env.OAuthTokenSet = test.envToken
			if test.live {
				if err := os.Mkdir(claude.LiveStoreDir(&env), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			subject, refused := useBuildSubject(paths, &env, test.live, owner, claude.ExportSpelling(ownerDir))
			if refused != nil {
				t.Fatalf("subject refused: %+v", refused)
			}
			if !test.absent {
				fixture.KeychainItem(subject.service, outgoing)
			}
			fixture.AllowWrite(subject.service)
			for _, entry := range fixture.Environ() {
				key, value, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
					t.Setenv(key, value)
				}
			}
			t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
			var posts, profiles atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					if _, err := fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":28800,"scope":"user:inference user:profile"}`); err != nil {
						t.Error(err)
					}
					return
				}
				profiles.Add(1)
				account := "owner"
				if strings.Contains(r.Header.Get("Authorization"), "incoming-access") || strings.Contains(r.Header.Get("Authorization"), "rotated-access") || test.newer {
					account = "incoming"
				}
				if account == "incoming" && test.profileUnavailable {
					w.WriteHeader(500)
					return
				}
				if account == "incoming" && test.mismatch {
					account = "wrong-account"
				}
				if _, err := fmt.Fprintf(w, `{"account":{"uuid":%q,"email":"test@example.invalid"},"organization":{"uuid":"org"}}`, account); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			t.Setenv(claude.TokenURLEnv, server.URL+"/token")
			t.Setenv(claude.ProfileURLEnv, server.URL+"/profile")
			if test.auditRefused {
				if err := os.WriteFile(secret.AuditLogPath(paths), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if test.busy {
				if err := os.Mkdir(filepath.Join(subject.storeDir, secret.RefreshLockName), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			process := SessionProcess{In: strings.NewReader("yes\n"), Out: &stdout, Err: &stderr}
			swap := useLiveSwap{paths: paths, config: registry, env: &env, live: test.live, inherited: claude.ExportSpelling(ownerDir), process: process}
			store := owner
			if test.live {
				store = nil
			}
			report := swap.swapIn(t.Context(), useIncoming{record: incoming, direction: secret.DirectionForward, source: useSource{kind: useSourceOwn}}, store, cli.ClaudeUseOptions{Yes: !test.decline, JSON: test.decline})
			if diff := gocmp.Diff(test.kind, report.outcome.Kind); diff != "" {
				t.Fatalf("outcome (-want +got): %s; report=%+v; stderr=%s", diff, report, stderr.String())
			}
			if report.outcome.Refusal.Kind != test.refusal {
				t.Fatalf("refusal=%v want=%v; note=%v", report.outcome.Refusal.Kind, test.refusal, report.note)
			}
			if posts.Load() != test.posts || profiles.Load() != test.profiles {
				t.Fatalf("network: posts=%d/%d profiles=%d/%d", posts.Load(), test.posts, profiles.Load(), test.profiles)
			}
			writes := 0
			for _, call := range fixture.SecurityLog() {
				if call == "-i" {
					writes++
				}
				if strings.Contains(call, "access") || strings.Contains(call, "refresh") || strings.HasPrefix(call, "delete-generic-password") {
					t.Fatalf("unsafe invocation: %s", call)
				}
			}
			if writes != test.writes {
				t.Fatalf("writes=%d want=%d; log=%v", writes, test.writes, fixture.SecurityLog())
			}
			if test.decline {
				if !strings.Contains(stdout.String(), `"kind": "plan"`) {
					t.Fatalf("missing JSON plan: %s", stdout.String())
				}
				if _, err := os.Stat(filepath.Join(ownerDir, secret.AdoptedFile)); !os.IsNotExist(err) {
					t.Fatalf("adopted before consent: %v", err)
				}
			}
			if test.live {
				if _, err := os.Stat(filepath.Join(subject.storeDir, secret.AdoptedFile)); !os.IsNotExist(err) {
					t.Fatalf("wrote adopted in live tree: %v", err)
				}
				if report.outcome.Kind == claude.SwapApplied && report.config == nil {
					t.Fatal("live write has no config report")
				}
			}
			if test.expired && report.outcome.Kind == claude.SwapApplied {
				saved, err := useReadStored(fixture.NamespaceDir("incoming", "org"), useSourceOwn)
				if err != nil || saved == nil {
					t.Fatalf("refresh save absent: %v", err)
				}
				got, err := saved.AccessToken.Digest()
				if err != nil {
					t.Fatal(err)
				}
				minted, err := secret.NewSecret([]byte("rotated-access"))
				if err != nil {
					t.Fatal(err)
				}
				want, err := minted.Digest()
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatal(diff)
				}
			}
		})
	}
}
