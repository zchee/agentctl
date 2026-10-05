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

package codex

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
)

func writerCredentials(t *testing.T, refresh string) []byte {
	t.Helper()
	id := syntheticJWT(`{"https://api.openai.com/auth":{"chatgpt_user_id":"user-one","chatgpt_account_id":"acct-one","chatgpt_plan_type":"plus"}}`)
	return fmt.Appendf(nil, `{"future":{"ordered":[1,2]},"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":"agctl-test-access","refresh_token":%q,"account_id":"acct-one"},"last_refresh":"2026-09-16T12:00:00Z"}`, id, refresh)
}

func writerNamespace(t *testing.T) (*config.Paths, *OwnedNamespace, *Lock, string) {
	t.Helper()
	paths := config.NewPaths(t.TempDir())
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Fatal(err)
	}
	record := ownedTestRecord("user-one", "acct-one")
	guard, err := AcquireCodex(t.Context(), paths, Owned(&record), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil {
			t.Error(err)
		}
	})
	namespace, err := OpenOwnedNamespace(paths, Owned(&record), guard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := namespace.Close(); err != nil {
			t.Error(err)
		}
	})
	dir, err := paths.CodexNamespaceDir("user-one", "acct-one")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, authFile)
	writeCodexFile(t, path, writerCredentials(t, "agctl-test-refresh-old"))
	return paths, namespace, guard, path
}

func lockedRead(t *testing.T, namespace *OwnedNamespace) *LockedCredentials {
	t.Helper()
	read, err := namespace.Read()
	if err != nil {
		t.Fatal(err)
	}
	if read.Kind != ResolvedCredentials || read.Credentials == nil {
		t.Fatalf("read kind=%s", read.Kind)
	}
	return read.Credentials
}

func mergedCredential(t *testing.T, credential *LockedCredentials) *LockedCredentials {
	t.Helper()
	response, err := ParseRefreshResponse([]byte(`{"access_token":"agctl-test-access-new","refresh_token":"agctl-test-refresh-new"}`))
	if err != nil {
		t.Fatal(err)
	}
	merged, outcome, err := credential.MergeRefresh(response, time.Date(2026, 9, 16, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.RefreshRotated || outcome.IdentityDrift {
		t.Fatalf("merge=%+v", outcome)
	}
	return merged
}

func TestLockedCredentialAuthority(t *testing.T) {
	_, namespace, guard, _ := writerNamespace(t)
	credentials := lockedRead(t, namespace)
	if err := credentials.WithRefreshBody("public-client", func(body []byte) error {
		if diff := gocmp.Diff(`{"client_id":"public-client","grant_type":"refresh_token","refresh_token":"agctl-test-refresh-old"}`, string(body)); diff != "" {
			t.Fatal(diff)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	copy := *credentials
	unbound, err := credentials.IntoCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if unbound.Digests() == nil {
		t.Fatal("unbound lost document")
	}
	if copy.Valid() {
		t.Fatal("copied carrier retained consumed authority")
	}
	called := false
	if err := copy.WithRefreshBody("x", func([]byte) error { called = true; return nil }); err == nil || called {
		t.Fatal("consumed carrier exposed refresh")
	}
	fresh := lockedRead(t, namespace)
	guardCopy := *guard
	if err := guardCopy.Release(); err != nil {
		t.Fatal(err)
	}
	if fresh.Valid() || guard.Valid() {
		t.Fatal("copied release did not invalidate capabilities")
	}
	if _, err := namespace.Read(); err == nil {
		t.Fatal("released namespace read")
	}
}

func TestRefreshMergePreservesOriginalGrant(t *testing.T) {
	_, namespace, _, path := writerNamespace(t)
	before := lockedRead(t, namespace)
	base := before.BaseDigests()
	beforeCopy := *before
	merged := mergedCredential(t, before)
	if beforeCopy.Valid() {
		t.Fatal("merge left original carrier live")
	}
	if diff := gocmp.Diff(base, merged.BaseDigests()); diff != "" {
		t.Fatal(diff)
	}
	output := credentialOutput(t, merged.Credentials())
	if !bytes.Contains(output, []byte(`"future"`)) || !bytes.Contains(output, []byte("2026-09-16T12:01:00Z")) {
		t.Fatal("merge lost unknown field or timestamp")
	}
	snapshot, err := namespace.SnapshotForPost()
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	external := writerCredentials(t, "agctl-test-refresh-external")
	writeCodexFile(t, path, external)
	if _, err := namespace.SnapshotForPost(); err != nil {
		t.Fatal(err)
	}
	result, err := namespace.Write(t.Context(), merged)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != CodexWriteChangedSinceRead || result.Receipt.Kind() != WriteDiscardedExternal {
		t.Fatalf("write=%+v", result)
	}
	if err := result.Receipt.Consume(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(external, got); diff != "" {
		t.Fatal(diff)
	}
}

func TestRefreshMergeIdentityAndTimestamp(t *testing.T) {
	tests := map[string]struct {
		id                string
		drift, unreadable bool
	}{
		"success: unreadable id retains old":     {"not-a-jwt", false, true},
		"success: changed user reports drift":    {syntheticJWT(`{"https://api.openai.com/auth":{"chatgpt_user_id":"user-other","chatgpt_account_id":"acct-one"}}`), true, false},
		"success: changed account reports drift": {syntheticJWT(`{"https://api.openai.com/auth":{"chatgpt_user_id":"user-one","chatgpt_account_id":"acct-other"}}`), true, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, namespace, _, _ := writerNamespace(t)
			body := fmt.Appendf(nil, `{"id_token":%q,"access_token":"agctl-test-rotated"}`, tt.id)
			response, err := ParseRefreshResponse(body)
			if err != nil {
				t.Fatal(err)
			}
			merged, outcome, err := lockedRead(t, namespace).MergeRefresh(response, time.Date(2026, 9, 16, 12, 0, 0, 123000000, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.IdentityDrift != tt.drift || outcome.IDTokenUnreadable != tt.unreadable {
				t.Fatalf("outcome=%+v", outcome)
			}
			if merged.Credentials().Identity().AccountID != "acct-one" {
				t.Fatal("merge replaced stored workspace")
			}
			if !strings.Contains(string(credentialOutput(t, merged.Credentials())), ".123Z") {
				t.Fatal("timestamp not auto precision")
			}
		})
	}
}

func TestOwnedWriterAndPendingReplay(t *testing.T) {
	tests := map[string]struct{ park, torn, missing bool }{"success: applied": {}, "success: missing target": {missing: true}, "success: pending replay": {park: true}, "error: torn pending target keeps pair": {park: true, torn: true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, namespace, _, path := writerNamespace(t)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			merged := mergedCredential(t, lockedRead(t, namespace))
			if tt.missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			var result CodexWrite
			if tt.park {
				result, err = namespace.Park(t.Context(), merged)
			} else {
				result, err = namespace.Write(t.Context(), merged)
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Kind != CodexWriteLanded || result.Outcome.SavedToPending != tt.park {
				t.Fatalf("result=%+v", result)
			}
			copy := *result.Receipt
			if err := result.Receipt.Consume(); err != nil {
				t.Fatal(err)
			}
			if copy.Consume() == nil {
				t.Fatal("receipt copied into second audit")
			}
			if tt.park {
				if tt.torn {
					writeCodexFile(t, path, []byte("{"))
				}
				decision, receipt, _, err := namespace.ResolvePending(t.Context())
				if tt.torn {
					if err == nil || receipt != nil {
						t.Fatal("torn target was resolved")
					}
					if _, err := os.Stat(filepath.Join(filepath.Dir(path), pendingFile)); err != nil {
						t.Fatal("pending grant removed")
					}
					writeCodexFile(t, path, original)
					decision, receipt, _, err = namespace.ResolvePending(t.Context())
				}
				if err != nil || decision.Kind != secret.PendingReplayed || receipt == nil {
					t.Fatalf("decision=%+v receipt=%v err=%v", decision, receipt, err)
				}
				if err := receipt.Consume(); err != nil {
					t.Fatal(err)
				}
			}
			read := ReadAuth(t.Context(), filepath.Dir(path))
			if read.Kind != ResolvedCredentials {
				t.Fatal(read.Kind)
			}
			want := merged.Credentials().Digests()
			if diff := gocmp.Diff(want, read.Credentials.Digests()); diff != "" {
				t.Fatal(diff)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode=%o", info.Mode().Perm())
			}
		})
	}
}

func successfulChildReport(t *testing.T, survey ScratchSurvey) *PostExitReport {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return postExitReportFromChild(nil, nil, survey, cmd.ProcessState)
}

func TestVerifiedLoginSingleInstall(t *testing.T) {
	paths, _, guard, path := writerNamespace(t)
	scratch := t.TempDir()
	writeCodexFile(t, filepath.Join(scratch, authFile), writerCredentials(t, "agctl-test-login"))
	login, err := VerifyLogin(t.Context(), scratch, successfulChildReport(t, ScratchSurvey{}))
	if err != nil {
		t.Fatal(err)
	}
	copy := *login
	receipt, identity, err := WriteAuth(t.Context(), paths, login, guard)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-one" || !receipt.Overwrote() {
		t.Fatalf("identity=%+v overwrote=%t", identity, receipt.Overwrote())
	}
	if err := receipt.Consume(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteAuth(t.Context(), paths, &copy, guard); err == nil {
		t.Fatal("copied verified login installed twice")
	}
	got := ReadAuth(t.Context(), filepath.Dir(path))
	if diff := gocmp.Diff(login.state.doc.Digests(), got.Credentials.Digests()); diff != "" {
		t.Fatal(diff)
	}
	if _, err := os.Stat(filepath.Join(scratch, authFile)); err != nil {
		t.Fatal("installer removed caller-owned scratch")
	}
}

func TestLoginReportRefusesBeforeCredentialRead(t *testing.T) {
	tests := map[string]struct{ survey ScratchSurvey }{"error: daemon": {ScratchSurvey{daemonDir: true}}, "error: held lock": {ScratchSurvey{heldLocks: []string{"held.lock"}}}, "error: odd lock": {ScratchSurvey{oddLocks: []string{"unsafe\n.lock"}}}, "error: truncated": {ScratchSurvey{truncated: true}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := VerifyLogin(t.Context(), filepath.Join(t.TempDir(), "missing"), successfulChildReport(t, tt.survey))
			if err == nil || !strings.Contains(err.Error(), "was refused") {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "unsafe\n") {
				t.Fatal("untrusted name reached diagnostics")
			}
		})
	}
}

func TestNamespaceProofAndForeignRemoval(t *testing.T) {
	paths, namespace, guard, path := writerNamespace(t)
	other := ownedTestRecord("user-other", "acct-one")
	if _, err := OpenOwnedNamespace(paths, Owned(&other), guard); err == nil {
		t.Fatal("foreign namespace opened")
	}
	foreign := filepath.Join(filepath.Dir(path), "sessions")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := namespace.RemoveNamedFiles(); err == nil {
		t.Fatal("foreign directory removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("refusal deleted credential")
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	receipt, err := namespace.RemoveNamedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Consume(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("namespace remains: %v", err)
	}
}
