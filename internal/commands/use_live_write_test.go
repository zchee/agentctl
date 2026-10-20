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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestUseWriteOutcomesAndRecovery(t *testing.T) {
	tests := map[string]struct {
		live      bool
		failed    bool
		changed   bool
		unknown   bool
		staging   bool
		shadow    bool
		duplicate bool
		busy      bool
		gone      bool
		holdPipes bool
		shutdown  bool
		kind      claude.SwapOutcomeKind
		refusal   claude.SwapRefusalKind
		audited   bool
		reads     int
	}{
		"success: live applied keeps live plaintext":                  {live: true, shadow: true, kind: claude.SwapApplied, audited: true, reads: 2},
		"success: first namespace write removes shadow":               {shadow: true, kind: claude.SwapApplied, audited: true, reads: 2},
		"success: applied reversal commits displaced staging":         {staging: true, kind: claude.SwapApplied, audited: true, reads: 2},
		"success: applied restoration removes only duplicate adopted": {live: true, duplicate: true, kind: claude.SwapApplied, audited: true, reads: 2},
		"error: definite write failure has no verify read":            {failed: true, staging: true, kind: claude.SwapFailed, audited: true, reads: 1},
		"error: changed baseline discards before write":               {changed: true, staging: true, kind: claude.SwapDiscarded, reads: 1},
		"error: unknown drops displaced staging":                      {unknown: true, staging: true, kind: claude.SwapUnknown, audited: true, reads: 2},
		"success: unreleased output is settled by the readback":       {holdPipes: true, staging: true, kind: claude.SwapApplied, audited: true, reads: 2},
		"error: a refused spawn discards without a write":             {shutdown: true, staging: true, kind: claude.SwapDiscarded, reads: 1},
		"error: unknown preserves namespace plaintext":                {unknown: true, shadow: true, kind: claude.SwapUnknown, audited: true, reads: 2},
		"error: busy peer never reads under a hold":                   {busy: true, kind: claude.SwapBusy},
		"error: vanished live store is not a compromised hold":        {live: true, gone: true, kind: claude.SwapRefused, refusal: claude.SwapLiveUnreachable},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t).WithKeychain()
			fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
			paths, record := refreshRecord(t, fixture)
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			nsDir := fixture.NamespaceDir(testutil.Acct, testutil.Org)
			oldBlob := fixture.Blob("outgoing-access", "outgoing-refresh", testutil.FreshAt())
			incomingBlob := fixture.Blob("incoming-access", "incoming-refresh", testutil.FreshAt())
			fixture.WriteCredentials(testutil.Acct, testutil.Org, incomingBlob)
			env := claude.EnvWithHome(fixture.Home())
			if test.live {
				if err := os.Mkdir(claude.LiveStoreDir(&env), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			subject, refused := useBuildSubject(paths, &env, test.live, record, claude.ExportSpelling(nsDir))
			if refused != nil {
				t.Fatalf("subject refused: %+v", refused)
			}
			var before *claude.Digests
			if !test.shadow || test.live {
				fixture.KeychainItem(subject.service, oldBlob)
				credentials, err := claude.ParseBlob([]byte(oldBlob))
				if err != nil {
					t.Fatal(err)
				}
				digest, err := credentials.Digests()
				if err != nil {
					t.Fatal(err)
				}
				before = &digest
			}
			if test.changed {
				fixture.KeychainItem(subject.service, incomingBlob)
			}
			fixture.AllowWrite(subject.service)
			for _, entry := range fixture.Environ() {
				key, value, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
					t.Setenv(key, value)
				}
			}
			t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
			if test.failed {
				t.Setenv("AGCTL_FAKE_SECURITY_WRITE_EXIT", "1")
			}
			if test.holdPipes {
				// The fake stores, then exits 0 while a background sleep keeps
				// its output open: the wait reports exec.ErrWaitDelay.
				t.Setenv("AGCTL_FAKE_SECURITY_HOLD_PIPES", "2")
			}
			if test.unknown {
				wrapper := fixture.Scratch("unknown-security.sh")
				marker := fixture.Scratch("write-done")
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = find-generic-password ] && [ -f %q ]; then\n printf '%%s\\n' \"$*\" >> \"$AGCTL_FAKE_SECURITY_LOG\"\n exit 36\nfi\n%q \"$@\"\nstatus=$?\nif [ \"$1\" = add-generic-password ]; then touch %q; fi\nexit $status\n", marker, fixture.SecurityBin(), marker)
				if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AGENTCTL_SECURITY_BIN", wrapper)
			}
			incoming, err := claude.ParseBlob([]byte(incomingBlob))
			if err != nil {
				t.Fatal(err)
			}
			after, err := incoming.Digests()
			if err != nil {
				t.Fatal(err)
			}
			to8, _ := secret.Digest8(after.AccessSHA256)
			input := useWritePhase{paths: paths, env: &env, subject: subject, before: before, after: after, toDigest8: to8, direction: secret.DirectionForward, shadowingStore: test.shadow, controller: testSignalController(t), account: testutil.KeychainAccount}
			if test.live {
				input.log, err = secret.OpenAuditLog(paths, secret.AuditLogPath(paths))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = input.log.Close() }()
				input.incomingIdentity = &secret.IncomingIdentity{AccountUUID: testutil.Acct, OrganizationUUID: new(testutil.Org)}
			}
			if before != nil {
				digest8, _ := secret.Digest8(before.AccessSHA256)
				input.fromDigest8 = &digest8
			}
			if test.shadow {
				if err := os.WriteFile(filepath.Join(subject.storeDir, secret.CredentialsFile), []byte(oldBlob), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.staging || test.duplicate {
				blob, err := useSealedBlob(incoming)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := secret.WriteAdopted(t.Context(), paths, nsDir, blob); err != nil {
					t.Fatal(err)
				}
			}
			if test.staging {
				displaced, err := claude.ParseBlob([]byte(oldBlob))
				if err != nil {
					t.Fatal(err)
				}
				blob, err := useSealedBlob(displaced)
				if err != nil {
					t.Fatal(err)
				}
				input.staged, err = secret.StageAdopted(t.Context(), paths, nsDir, blob)
				if err != nil {
					t.Fatal(err)
				}
				input.direction = secret.DirectionUndo
			}
			if test.duplicate {
				input.restoredAdopted = nsDir
			}
			if test.busy {
				if err := os.Mkdir(filepath.Join(subject.storeDir, secret.RefreshLockName), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.gone {
				if err := os.Remove(subject.storeDir); err != nil {
					t.Fatal(err)
				}
			}
			if test.shutdown {
				input.controller.Stop()
			}
			report := useWrite(t.Context(), input, incoming)
			if diff := gocmp.Diff(test.kind, report.outcome.Kind); diff != "" {
				t.Fatalf("outcome (-want +got):\n%s; note=%v", diff, report.note)
			}
			if report.outcome.Refusal.Kind != test.refusal {
				t.Fatalf("refusal=%v; want %v", report.outcome.Refusal, test.refusal)
			}
			if report.lock.BudgetMS == nil || *report.lock.BudgetMS != 3000 {
				t.Fatalf("missing protocol budget: %+v", report.lock)
			}
			reads := 0
			for _, call := range fixture.SecurityLog() {
				if strings.HasPrefix(call, "find-generic-password") {
					reads++
				}
				if strings.Contains(call, "incoming-access") || strings.Contains(call, "outgoing-access") || strings.HasPrefix(call, "delete-generic-password") {
					t.Fatalf("unsafe child argv: %s", call)
				}
			}
			if reads != test.reads {
				t.Fatalf("reads=%d want=%d; calls=%v", reads, test.reads, fixture.SecurityLog())
			}
			tail, err := secret.TailAuditLog(paths, 64)
			if err != nil {
				t.Fatal(err)
			}
			if (len(tail.Entries) != 0) != test.audited {
				t.Fatalf("unexpected audits: %+v", tail.Entries)
			}
			if test.audited {
				event, ok := tail.Entries[len(tail.Entries)-1].Event.(*secret.WriteEvent)
				if !ok || string(event.Outcome) != report.outcome.Word() || event.Direction != input.direction {
					t.Fatalf("wrong write audit: %+v", event)
				}
				if test.live && event.IncomingIdentity == nil {
					t.Fatal("live write lost identity provenance")
				}
			}
			if test.staging {
				read, err := secret.ReadAdopted(nsDir)
				if err != nil || !read.Present {
					t.Fatalf("adopted copy missing: %v", err)
				}
				want := incomingBlob
				if test.kind == claude.SwapApplied {
					want = oldBlob
				}
				wantItem, err := claude.ParseBlob([]byte(want))
				if err != nil {
					t.Fatal(err)
				}
				foundItem, err := claude.ParseBlob(read.Bytes)
				if err != nil {
					t.Fatal(err)
				}
				wantDigests, err := wantItem.Digests()
				if err != nil {
					t.Fatal(err)
				}
				foundDigests, err := foundItem.Digests()
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(wantDigests, foundDigests); diff != "" {
					t.Fatalf("wrong adopted credential: %s", diff)
				}
				if temps, err := secret.ListStrayAdoptedTmp(nsDir); err != nil || len(temps) != 0 {
					t.Fatalf("staged temporary leaked: %v", temps)
				}
			}
			if test.shadow {
				_, err := os.Stat(filepath.Join(subject.storeDir, secret.CredentialsFile))
				removed := test.kind == claude.SwapApplied && !test.live
				if os.IsNotExist(err) != removed {
					t.Fatalf("plaintext removal=%v want=%v: %v", os.IsNotExist(err), removed, err)
				}
			}
			if test.duplicate {
				read, err := secret.ReadAdopted(nsDir)
				if err != nil || read.Present {
					t.Fatalf("duplicate adopted copy remains: %v", err)
				}
			}
			for _, lock := range []string{secret.RefreshLockName, secret.StorageWriteLockName} {
				if test.busy && lock == secret.RefreshLockName {
					continue
				}
				if _, err := os.Lstat(filepath.Join(subject.storeDir, lock)); !os.IsNotExist(err) {
					t.Fatalf("peer lock leaked: %s (%v)", lock, err)
				}
			}
		})
	}
}

func TestUseDuplicateAdoptedCleanupRequiresBothHomes(t *testing.T) {
	tests := map[string]struct {
		own     bool
		adopted bool
		removed bool
	}{
		"success: equal own and adopted":            {own: true, adopted: true, removed: true},
		"success: different own keeps sole adopted": {adopted: true},
		"success: different adopted is kept":        {own: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			paths := config.NewPaths(fixture.ConfigDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			a := fixture.Blob("restored", "r", testutil.FreshAt())
			b := fixture.Blob("other", "s", testutil.FreshAt())
			own, adopted := b, b
			if test.own {
				own = a
			}
			if test.adopted {
				adopted = a
			}
			dir := fixture.NamespaceDir(testutil.Acct, testutil.Org)
			fixture.WriteCredentials(testutil.Acct, testutil.Org, own)
			sealed, err := secret.NewSecret([]byte(adopted))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := secret.WriteAdopted(t.Context(), paths, dir, sealed); err != nil {
				t.Fatal(err)
			}
			restored, err := claude.ParseBlob([]byte(a))
			if err != nil {
				t.Fatal(err)
			}
			digest, err := restored.Digests()
			if err != nil {
				t.Fatal(err)
			}
			if err := useRemoveDuplicateAdopted(paths, dir, digest); err != nil {
				t.Fatal(err)
			}
			read, err := secret.ReadAdopted(dir)
			if err != nil || read.Present == test.removed {
				t.Fatalf("removed=%v want=%v: %v", !read.Present, test.removed, err)
			}
		})
	}
}

// testSignalController installs a signal controller for one test, as the
// command's entry point does for a run: the keychain writer refuses to start
// a child the signal path cannot see.
func testSignalController(t *testing.T) *signals.Controller {
	t.Helper()
	_, controller := signals.Install(t.Context())
	t.Cleanup(controller.Stop)
	return controller
}

// withMembers appends top-level members to a credential document, the way
// Claude Code keeps its MCP logins beside the account object.
func withMembers(t *testing.T, blob, members string) string {
	t.Helper()
	trimmed := strings.TrimSpace(blob)
	if !strings.HasSuffix(trimmed, "}") {
		t.Fatalf("the fixture document does not end in an object")
	}
	return strings.TrimSuffix(trimmed, "}") + "," + members + "}"
}

func mustSummary(t *testing.T, document string) []claude.SiblingMeta {
	t.Helper()
	credentials, err := claude.ParseBlob([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := credentials.SiblingSummary()
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestUseWriteCarriesTheItemsOwnMembers(t *testing.T) {
	const canary = "SIBLING-CANARY-9f31"
	itemMembers := `"mcpOAuth":{"one":{"accessToken":"` + canary + `"},"two":{"accessToken":"b"}},"pluginSecrets":"p"`
	tests := map[string]struct {
		itemAbsent  bool
		rewrite     bool
		unreadable  bool
		wantCarried []useCarriedMember
		wantVerdict secret.SiblingsReadback
		wantChanged string
		wantKind    claude.SwapOutcomeKind
	}{
		"success: the item's members are written back and read back equal": {
			wantCarried: []useCarriedMember{{Name: "mcpOAuth", Entries: new(2)}, {Name: "pluginSecrets"}},
			wantVerdict: secret.SiblingsEqual, wantKind: claude.SwapApplied,
		},
		"success: an absent item carries nothing, whatever the file held": {
			itemAbsent: true, wantCarried: []useCarriedMember{}, wantVerdict: secret.SiblingsEqual, wantKind: claude.SwapApplied,
		},
		"success: a rewrite after the write is reported, not prevented": {
			rewrite:     true,
			wantCarried: []useCarriedMember{{Name: "mcpOAuth", Entries: new(2)}, {Name: "pluginSecrets"}},
			wantVerdict: secret.SiblingsDiffer, wantChanged: "mcpOAuth", wantKind: claude.SwapApplied,
		},
		"error: a failed readback knows nothing about the members": {
			unreadable:  true,
			wantCarried: []useCarriedMember{{Name: "mcpOAuth", Entries: new(2)}, {Name: "pluginSecrets"}},
			wantVerdict: secret.SiblingsUnverified, wantKind: claude.SwapUnknown,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t).WithKeychain()
			fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
			paths, record := refreshRecord(t, fixture)
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			nsDir := fixture.NamespaceDir(testutil.Acct, testutil.Org)
			itemBlob := withMembers(t, fixture.Blob("outgoing-access", "outgoing-refresh", testutil.FreshAt()), itemMembers)
			// The incoming credential's file carries a member of its own; a
			// file is never a source of carried members.
			incomingBlob := withMembers(t, fixture.Blob("incoming-access", "incoming-refresh", testutil.FreshAt()), `"fileOnly":{"x":{}}`)
			fixture.WriteCredentials(testutil.Acct, testutil.Org, incomingBlob)
			env := claude.EnvWithHome(fixture.Home())
			subject, refused := useBuildSubject(paths, &env, false, record, claude.ExportSpelling(nsDir))
			if refused != nil {
				t.Fatalf("subject refused: %+v", refused)
			}
			var before *claude.Digests
			if !test.itemAbsent {
				fixture.KeychainItem(subject.service, itemBlob)
				outgoing, err := claude.ParseBlob([]byte(itemBlob))
				if err != nil {
					t.Fatal(err)
				}
				digest, err := outgoing.Digests()
				if err != nil {
					t.Fatal(err)
				}
				before = &digest
			}
			fixture.AllowWrite(subject.service)
			for _, entry := range fixture.Environ() {
				key, value, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "AGCTL_FAKE_SECURITY_") || key == "AGENTCTL_SECURITY_BIN" || key == "USER" {
					t.Setenv(key, value)
				}
			}
			t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
			if test.rewrite || test.unreadable {
				// After the write, another process rewrites the item with a
				// changed MCP login, or the item becomes unreadable.
				rewritten := withMembers(t, fixture.Blob("incoming-access", "incoming-refresh", testutil.FreshAt()), `"mcpOAuth":{"one":{"accessToken":"rotated"},"two":{"accessToken":"b"}},"pluginSecrets":"p"`)
				replacement := fixture.Scratch("rewritten.json")
				if err := os.WriteFile(replacement, []byte(rewritten), 0o600); err != nil {
					t.Fatal(err)
				}
				marker := fixture.Scratch("write-done")
				after := fmt.Sprintf("cp %q %q", replacement, fixture.KeychainItemPath(subject.service))
				if test.unreadable {
					after = "true"
				}
				wrapper := fixture.Scratch("rewriting-security.sh")
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = find-generic-password ] && [ -f %q ] && [ %t = true ]; then exit 36; fi\n%q \"$@\"\nstatus=$?\nif [ \"$1\" = add-generic-password ] && [ $status = 0 ]; then %s; touch %q; fi\nexit $status\n", marker, test.unreadable, fixture.SecurityBin(), after, marker)
				if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AGENTCTL_SECURITY_BIN", wrapper)
			}
			incoming, err := claude.ParseBlob([]byte(incomingBlob))
			if err != nil {
				t.Fatal(err)
			}
			after, err := incoming.Digests()
			if err != nil {
				t.Fatal(err)
			}
			to8, _ := secret.Digest8(after.AccessSHA256)
			input := useWritePhase{paths: paths, env: &env, subject: subject, before: before, after: after, toDigest8: to8, direction: secret.DirectionForward, shadowingStore: test.itemAbsent, controller: testSignalController(t), account: testutil.KeychainAccount}
			if before != nil {
				digest8, _ := secret.Digest8(before.AccessSHA256)
				input.fromDigest8 = &digest8
			}

			report := useWrite(t.Context(), input, incoming)

			if report.outcome.Kind != test.wantKind {
				t.Fatalf("outcome = %s, want %s; note=%s", report.outcome.Kind, test.wantKind, deref(report.note))
			}
			if report.carriedMembers == nil {
				t.Fatal("carried_members is absent although the update was built")
			}
			if diff := gocmp.Diff(test.wantCarried, *report.carriedMembers); diff != "" {
				t.Errorf("carried_members (-want +got):\n%s", diff)
			}
			if report.siblingsReadback == nil || *report.siblingsReadback != test.wantVerdict {
				t.Errorf("siblings_readback = %v, want %s", report.siblingsReadback, test.wantVerdict)
			}
			if report.transport == nil || *report.transport != secret.TransportArgv {
				t.Errorf("transport = %v, want argv", report.transport)
			}
			if test.wantChanged != "" && (report.note == nil || !strings.Contains(*report.note, "members "+test.wantChanged+" did not read back")) {
				t.Errorf("the note must name %s: %v", test.wantChanged, report.note)
			}
			if !test.rewrite && !test.unreadable {
				stored, err := os.ReadFile(fixture.KeychainItemPath(subject.service))
				if err != nil {
					t.Fatal(err)
				}
				want := []claude.SiblingMeta{}
				if !test.itemAbsent {
					want = mustSummary(t, itemBlob)
				}
				if diff := gocmp.Diff(want, mustSummary(t, string(stored))); diff != "" {
					t.Errorf("the stored item's members are not the held read's (-want +got):\n%s", diff)
				}
				if strings.Contains(string(stored), "fileOnly") {
					t.Error("a member of the incoming credential's file reached the keychain item")
				}
			}
			if !test.itemAbsent && !test.rewrite && !test.unreadable {
				if stored, _ := os.ReadFile(fixture.KeychainItemPath(subject.service)); !strings.Contains(string(stored), canary) {
					t.Error("the carried MCP login is missing from the stored item")
				}
			}
			tail, err := secret.TailAuditLog(paths, 8)
			if err != nil || len(tail.Entries) == 0 {
				t.Fatalf("no audit entry: %v", err)
			}
			event, ok := tail.Entries[len(tail.Entries)-1].Event.(*secret.WriteEvent)
			if !ok {
				t.Fatalf("last audit entry is %T", tail.Entries[len(tail.Entries)-1].Event)
			}
			var wantNames []string
			for _, member := range test.wantCarried {
				wantNames = append(wantNames, member.Name)
			}
			if diff := gocmp.Diff(wantNames, event.Carried); diff != "" {
				t.Errorf("audit carried (-want +got):\n%s", diff)
			}
			if event.SiblingsReadback != test.wantVerdict || event.Transport != secret.TransportArgv {
				t.Errorf("audit readback=%q transport=%q", event.SiblingsReadback, event.Transport)
			}
			for _, path := range []string{secret.AuditLogPath(paths), os.Getenv("AGCTL_FAKE_SECURITY_LOG")} {
				if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), canary) {
					t.Errorf("%s carries a carried member's value", filepath.Base(path))
				}
			}
		})
	}
}

func deref(note *string) string {
	if note == nil {
		return "<nil>"
	}
	return *note
}

func TestUseAdmitUpdateCountsTheItemsMembersBeforeAnyMutation(t *testing.T) {
	fixture := testutil.New(t)
	incomingBlob := withMembers(t, fixture.Blob("incoming-access", "incoming-refresh", testutil.FreshAt()), `"fileOnly":{"x":{}}`)
	small := withMembers(t, fixture.Blob("outgoing-access", "outgoing-refresh", testutil.FreshAt()), `"mcpOAuth":{"one":{}}`)
	grown := withMembers(t, fixture.Blob("outgoing-access", "outgoing-refresh", testutil.FreshAt()), `"mcpOAuth":{"one":{"accessToken":"`+strings.Repeat("m", secret.KeychainBlobLimit)+`"}}`)
	tests := map[string]struct {
		item        string
		account     string
		wantRefusal claude.SwapRefusalKind
		wantCarried []string
	}{
		"success: the item's members are carried": {item: small, account: testutil.KeychainAccount, wantCarried: []string{"mcpOAuth"}},
		"success: an absent item carries nothing": {account: testutil.KeychainAccount},
		"error: the item's members push the document over the ceiling": {
			item: grown, account: testutil.KeychainAccount, wantRefusal: claude.SwapCredentialTooLarge,
		},
		"error: an account security would misread refuses as cannot-adopt": {
			item: small, account: "two words", wantRefusal: claude.SwapCannotAdopt,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			credentials, err := claude.ParseBlob([]byte(incomingBlob))
			if err != nil {
				t.Fatal(err)
			}
			var item *claude.Credentials
			if tt.item != "" {
				if item, err = claude.ParseBlob([]byte(tt.item)); err != nil {
					t.Fatal(err)
				}
			}
			report := useAdmitUpdate(credentials, item, tt.account, claude.LiveService)
			if tt.wantRefusal == 0 {
				if report != nil {
					t.Fatalf("refused: %s", deref(report.note))
				}
			} else {
				if report == nil || report.outcome.Kind != claude.SwapRefused || report.outcome.Refusal.Kind != tt.wantRefusal {
					t.Fatalf("report = %+v, want refusal %v", report, tt.wantRefusal)
				}
				if tt.wantRefusal == claude.SwapCredentialTooLarge && (report.outcome.ExitCode() != 31 || report.outcome.Refusal.Reason() != "credential_too_large") {
					t.Fatalf("exit %d reason %q", report.outcome.ExitCode(), report.outcome.Refusal.Reason())
				}
				return
			}
			var names []string
			for _, member := range credentials.Siblings {
				names = append(names, member.Name)
			}
			if diff := gocmp.Diff(tt.wantCarried, names); diff != "" {
				t.Errorf("carried members (-want +got):\n%s", diff)
			}
		})
	}
}
