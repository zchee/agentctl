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
			if test.unknown {
				wrapper := fixture.Scratch("unknown-security.sh")
				marker := fixture.Scratch("write-done")
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = find-generic-password ] && [ -f %q ]; then\n printf '%%s\\n' \"$*\" >> \"$AGCTL_FAKE_SECURITY_LOG\"\n exit 36\nfi\n%q \"$@\"\nstatus=$?\nif [ \"$1\" = -i ]; then touch %q; fi\nexit $status\n", marker, fixture.SecurityBin(), marker)
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
			line, err := incoming.ToKeychainStdinLine(testutil.KeychainAccount, subject.service)
			if err != nil {
				t.Fatal(err)
			}
			to8, _ := secret.Digest8(after.AccessSHA256)
			input := useWritePhase{paths: paths, env: &env, subject: subject, before: before, after: after, toDigest8: to8, direction: secret.DirectionForward, shadowingStore: test.shadow}
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
			report := useWrite(t.Context(), input, line)
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
