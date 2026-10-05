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
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestUseAdoptionDestinationsAndLiveLineage(t *testing.T) {
	tests := map[string]struct {
		live            bool
		undo            bool
		missingOwner    bool
		ownDuplicate    bool
		parked          string
		parkedAccount   string
		audit           bool
		pending         bool
		incomingAccount bool
		incomingExpiry  int64
		kind            useAdoptionKind
		reason          claude.AdoptionRefusal
	}{
		"success: live forward parks beside independent own grant":           {live: true, kind: useAdoptionCopy},
		"success: live forward duplicate already at home is not parked":      {live: true, ownDuplicate: true, kind: useAdoptionNothing},
		"success: live forward supersedes earlier recorded live parking":     {live: true, parked: "prior-live-grant", audit: true, kind: useAdoptionCopy},
		"error: live forward never overwrites namespace undo source":         {live: true, parked: "namespace-undo-grant", reason: claude.AdoptionOccupiedByAnother},
		"error: recorded parking of another identity is not overwritten":     {live: true, parked: "another-grant", parkedAccount: "another-account", audit: true, reason: claude.AdoptionOccupiedByAnother},
		"error: live forward missing owned account refuses":                  {live: true, missingOwner: true, reason: claude.AdoptionIdentityMismatch},
		"error: pending copy refuses live parking":                           {live: true, pending: true, reason: claude.AdoptionPendingPresent},
		"success: live older incoming-account grant is discarded":            {live: true, incomingAccount: true, incomingExpiry: 200, kind: useAdoptionNothing},
		"error: live newer incoming-account grant is never kept beside live": {live: true, incomingAccount: true, reason: claude.AdoptionNewerCopy},
		"success: namespace own occupant parks in adopted copy":              {kind: useAdoptionCopy},
		"success: namespace undo stages regardless of parked expiry":         {undo: true, parked: "newer-parked-grant", kind: useAdoptionStaged},
		"error: namespace undo pending copy blocks staging":                  {undo: true, pending: true, reason: claude.AdoptionPendingPresent},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			paths := config.NewPaths(fixture.ConfigDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			record := func(account string) *config.AccountRecord {
				spelling := claude.ExportSpelling(paths.NamespaceDir(account, "org"))
				return &config.AccountRecord{AccountUUID: account, OrganizationUUID: "org", Kind: config.AccountKindOwned(spelling, claude.SHA8(spelling))}
			}
			incoming := record("incoming-account")
			owner := record("displaced-account")
			ownerDir := fixture.NamespaceDir(owner.AccountUUID, owner.OrganizationUUID)
			subject := &useSubject{storeDir: ownerDir, service: "unwritten-service", tree: secret.TreeOwn}
			if test.live {
				subject.storeDir = filepath.Join(fixture.Home(), ".claude")
				subject.tree = secret.TreeLive
			}
			displacedAccount := owner.AccountUUID
			if test.incomingAccount {
				displacedAccount = incoming.AccountUUID
			}
			displaced, err := claude.ParseBlob([]byte(fixture.IdentifiedBlob("displaced-access", "displaced-refresh", 100, displacedAccount, "org")))
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := claude.ParseBlob([]byte(fixture.IdentifiedBlob("incoming-access", "incoming-refresh", test.incomingExpiry, incoming.AccountUUID, "org")))
			if err != nil {
				t.Fatal(err)
			}
			ownBlob := fixture.IdentifiedBlob("independent-own-grant", "independent-refresh", 900, owner.AccountUUID, "org")
			if test.ownDuplicate {
				ownBlob = fixture.IdentifiedBlob("displaced-access", "displaced-refresh", 100, owner.AccountUUID, "org")
			}
			fixture.WriteCredentials(owner.AccountUUID, owner.OrganizationUUID, ownBlob)
			fixture.WriteCredentials(incoming.AccountUUID, incoming.OrganizationUUID, fixture.IdentifiedBlob("incoming-access", "incoming-refresh", test.incomingExpiry, incoming.AccountUUID, "org"))
			beforeOwn, err := os.ReadFile(filepath.Join(ownerDir, secret.CredentialsFile))
			if err != nil {
				t.Fatal(err)
			}
			if test.parked != "" {
				account := owner.AccountUUID
				if test.parkedAccount != "" {
					account = test.parkedAccount
				}
				parked, err := claude.ParseBlob([]byte(fixture.IdentifiedBlob(test.parked, "parked-refresh", 1000, account, "org")))
				if err != nil {
					t.Fatal(err)
				}
				sealed, err := useSealedBlob(parked)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := secret.WriteAdopted(t.Context(), paths, ownerDir, sealed); err != nil {
					t.Fatal(err)
				}
				if test.audit {
					digest, err := parked.Digests()
					if err != nil {
						t.Fatal(err)
					}
					digest8, _ := secret.Digest8(digest.AccessSHA256)
					event := &secret.WriteEvent{Target: secret.TargetLive, Direction: secret.DirectionForward, FromDigest8: &digest8, ToDigest8: "aabbccdd", Outcome: secret.WriteUnknown}
					if _, err := secret.AuditAppend(t.Context(), paths, secret.NewAuditEntry(event)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.pending {
				if err := os.WriteFile(filepath.Join(ownerDir, secret.PendingFile), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			parties := useAdoptionParties{paths: paths, store: owner, subject: subject, incoming: incoming, incomingCredentials: replacement, third: owner, identity: displaced.Identity(), displaced: displaced, live: test.live, direction: secret.DirectionForward}
			if test.live {
				parties.store = nil
			}
			if test.undo {
				parties.direction = secret.DirectionUndo
			}
			if test.missingOwner {
				parties.third = nil
			}
			plan, refused := useDecideAdoption(t.Context(), parties)
			if test.reason != "" {
				if refused == nil || refused.outcome.Refusal.Adoption != test.reason {
					t.Fatalf("refused=%+v want=%s", refused, test.reason)
				}
				return
			}
			if refused != nil {
				t.Fatalf("unexpected refusal: %+v", refused)
			}
			if diff := gocmp.Diff(test.kind, plan.kind); diff != "" {
				t.Fatal(diff)
			}
			to, staged, refusal := usePerformAdoption(t.Context(), paths, plan, displaced)
			if refusal != "" {
				t.Fatal(refusal)
			}
			if staged != nil {
				staged.Discard()
				staged.Discard()
				if to != nil {
					t.Fatal("staging prematurely reported completed adoption")
				}
			}
			afterOwn, err := os.ReadFile(filepath.Join(ownerDir, secret.CredentialsFile))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(beforeOwn, afterOwn); diff != "" {
				t.Fatalf("independent own grant was overwritten: %s", diff)
			}
			if plan.kind == useAdoptionCopy {
				parked, err := useReadStored(plan.dir, useSourceAdopted)
				if err != nil || parked == nil {
					t.Fatalf("parked copy missing: %v", err)
				}
				want, err := displaced.Digests()
				if err != nil {
					t.Fatal(err)
				}
				got, err := parked.Digests()
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatal(diff)
				}
				if to == nil || *to != secret.AdoptedFile || !paths.IsUnderNamespaceRoot(plan.dir) {
					t.Fatalf("unsafe adoption destination: %v %s", to, plan.dir)
				}
			}
			if test.live {
				if _, err := os.Stat(subject.storeDir); !os.IsNotExist(err) {
					t.Fatalf("adoption wrote or created live tree: %v", err)
				}
			}
		})
	}
}

func TestUseThirdStoreAdoptionComparesDigestsNotExpiry(t *testing.T) {
	tests := map[string]struct {
		initial   bool
		changed   bool
		malformed bool
		reason    claude.AdoptionRefusal
	}{
		"success: absent third store receives displaced pair":   {},
		"success: unchanged third store is replaced":            {initial: true},
		"error: equal-expiry different pair is not overwritten": {initial: true, changed: true, reason: claude.AdoptionChanged},
		"error: newly-created file is not overwritten":          {changed: true, reason: claude.AdoptionChanged},
		"error: malformed replacement is unreadable":            {initial: true, malformed: true, reason: claude.AdoptionUnreadable},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			paths := config.NewPaths(fixture.ConfigDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			dir := fixture.NamespaceDir("third", "org")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			original := fixture.Blob("original", "original-refresh", 100)
			if test.initial {
				fixture.WriteCredentials("third", "org", original)
			}
			displaced, err := claude.ParseBlob([]byte(fixture.Blob("displaced", "displaced-refresh", 200)))
			if err != nil {
				t.Fatal(err)
			}
			digest, err := displaced.Digests()
			if err != nil {
				t.Fatal(err)
			}
			_, prior, _ := useInspectStored(dir, useSourceOwn, digest)
			if test.changed {
				fixture.WriteCredentials("third", "org", fixture.Blob("changed", "changed-refresh", 100))
			}
			if test.malformed {
				fixture.WriteCredentials("third", "org", "malformed")
			}
			_, staged, refusal := usePerformAdoption(t.Context(), paths, useAdoptionPlan{kind: useAdoptionThirdStore, dir: dir, prior: prior}, displaced)
			if staged != nil {
				staged.Discard()
				t.Fatal("third store adoption unexpectedly staged")
			}
			if diff := gocmp.Diff(test.reason, refusal); diff != "" {
				t.Fatal(diff)
			}
			if refusal != "" {
				read, err := os.ReadFile(filepath.Join(dir, secret.CredentialsFile))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(read), "displaced-refresh") {
					t.Fatal("refused adoption wrote displaced grant")
				}
				return
			}
			stored, err := useReadStored(dir, useSourceOwn)
			if err != nil || stored == nil {
				t.Fatalf("stored grant missing: %v", err)
			}
			got, err := stored.Digests()
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(digest, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUseThirdNamespaceResolution(t *testing.T) {
	tests := map[string]struct {
		live      bool
		ambiguous bool
		want      bool
	}{
		"success: namespace resolves first matching account": {want: true},
		"success: live resolves matching owned organization": {live: true, want: true},
		"error: ambiguous owned live organizations refuse":   {live: true, ambiguous: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			incoming := &config.AccountRecord{AccountUUID: "incoming", OrganizationUUID: "org"}
			identity := &claude.Identity{AccountUUID: "third", OrganizationUUID: new("org")}
			registry := &config.Registry{Accounts: []config.AccountRecord{{AccountUUID: "third", OrganizationUUID: "org", Kind: config.AccountKindOwned("/namespace", "aabbccdd")}}}
			if test.ambiguous {
				registry.Accounts = append(registry.Accounts, registry.Accounts[0])
				identity.OrganizationUUID = nil
			}
			found, ambiguity := useThirdNamespace(registry, nil, incoming, identity, test.live)
			if (found != nil) != test.want || (ambiguity != nil) != test.ambiguous {
				t.Fatalf("found=%+v ambiguity=%+v", found, ambiguity)
			}
			same, ambiguity := useThirdNamespace(registry, found, incoming, incomingIdentityForTest(incoming), test.live)
			if same != nil || ambiguity != nil {
				t.Fatal("incoming identity unexpectedly resolved third namespace")
			}
		})
	}
}

func incomingIdentityForTest(record *config.AccountRecord) *claude.Identity {
	return &claude.Identity{AccountUUID: record.AccountUUID, OrganizationUUID: new(record.OrganizationUUID)}
}
