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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/secret"
)

func TestPendingExternalGrantDiscardHasReceipt(t *testing.T) {
	_, namespace, _, path := writerNamespace(t)
	merged := mergedCredential(t, lockedRead(t, namespace))
	parked, err := namespace.Park(t.Context(), merged)
	if err != nil {
		t.Fatal(err)
	}
	if err := parked.Receipt.Consume(); err != nil {
		t.Fatal(err)
	}
	external := writerCredentials(t, "agctl-test-refresh-external")
	writeCodexFile(t, path, external)
	decision, receipt, _, err := namespace.ResolvePending(t.Context())
	if err != nil || decision.Kind != secret.PendingDiscarded || receipt == nil || receipt.Kind() != WritePendingDiscarded {
		t.Fatalf("decision=%+v receipt=%v err=%v", decision, receipt, err)
	}
	if err := receipt.Consume(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(external, got); diff != "" {
		t.Fatal(diff)
	}
	for _, name := range []string{pendingFile, pendingMeta} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(path), name)); !os.IsNotExist(err) {
			t.Fatalf("%s survives discard: %v", name, err)
		}
	}
}

func TestUnopenableNamespaceFilesKeepPending(t *testing.T) {
	tests := map[string]struct{ name string }{
		"error: unreadable auth":    {authFile},
		"error: unreadable pending": {pendingFile},
		"error: unreadable meta":    {pendingMeta},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, namespace, _, path := writerNamespace(t)
			merged := mergedCredential(t, lockedRead(t, namespace))
			parked, err := namespace.Park(t.Context(), merged)
			if err != nil {
				t.Fatal(err)
			}
			if err := parked.Receipt.Consume(); err != nil {
				t.Fatal(err)
			}
			broken := filepath.Join(filepath.Dir(path), tt.name)
			if err := os.Chmod(broken, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(broken, 0o600); err != nil {
					t.Error(err)
				}
			})
			if decision, receipt, _, err := namespace.ResolvePending(t.Context()); err == nil || receipt != nil {
				t.Fatalf("decision=%+v receipt=%v err=%v", decision, receipt, err)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), pendingMeta)); err != nil {
				t.Fatal("resolver removed pending metadata")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), pendingFile)); err != nil {
				t.Fatal("resolver removed pending path")
			}
			if tt.name == authFile {
				if result, err := namespace.Write(t.Context(), merged); err == nil || result.Receipt != nil {
					t.Fatal("unopenable auth became a discard or successful write")
				}
			}
		})
	}
}

func TestPendingReplayUnderLiveDaemon(t *testing.T) {
	_, namespace, _, path := writerNamespace(t)
	merged := mergedCredential(t, lockedRead(t, namespace))
	parked, err := namespace.Park(t.Context(), merged)
	if err != nil {
		t.Fatal(err)
	}
	if err := parked.Receipt.Consume(); err != nil {
		t.Fatal(err)
	}
	writeCodexFile(t, filepath.Join(filepath.Dir(path), "app-server-daemon", "app-server.pid"), fmt.Appendf(nil, `{"pid":%d}`, os.Getpid()))
	decision, receipt, evidence, err := namespace.ResolvePending(t.Context())
	if err != nil || decision.Kind != secret.PendingReplayed || receipt == nil || evidence.Kind != DaemonAlive || evidence.PID != uint32(os.Getpid()) {
		t.Fatalf("decision=%+v evidence=%+v err=%v", decision, evidence, err)
	}
	if err := receipt.Consume(); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(merged.Credentials().Digests(), ReadAuth(t.Context(), filepath.Dir(path)).Credentials.Digests()); diff != "" {
		t.Fatal(diff)
	}
}

func TestLoginCredentialRefusalMatrix(t *testing.T) {
	missingIdentity := fmt.Appendf(nil, `{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":"agctl-test-access"}}`, syntheticJWT(`{"https://api.openai.com/auth":{"chatgpt_user_id":"user-one"}}`))
	dotted := strings.ReplaceAll(string(writerCredentials(t, "agctl-test-refresh")), `"account_id":"acct-one"`, `"account_id":".locks"`)
	tests := map[string]struct {
		data []byte
		kind string
	}{
		"error: absent":           {nil, "absent"},
		"error: api key":          {[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"agctl-test-codex-ak-0001"}`), "mode"},
		"error: torn":             {[]byte(`{"auth_mode":"chat`), ""},
		"error: missing identity": {missingIdentity, "identity"},
		"error: reserved account": {[]byte(dotted), "ids"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			scratch := t.TempDir()
			if tt.data != nil {
				writeCodexFile(t, filepath.Join(scratch, authFile), tt.data)
			}
			_, err := VerifyLogin(t.Context(), scratch, successfulChildReport(t, ScratchSurvey{}))
			refusal, ok := errors.AsType[*LoginRefusal](err)
			if !ok || refusal.Kind != tt.kind {
				t.Fatalf("refusal kind=%v; expected=%s", refusal, tt.kind)
			}
			assertNoCredentialNeedles(t, err.Error())
		})
	}
}

func TestForeignEntryNamesAreEscaped(t *testing.T) {
	_, namespace, _, path := writerNamespace(t)
	writeCodexFile(t, filepath.Join(filepath.Dir(path), "evil\x1b[2Jname"), []byte("x"))
	_, err := namespace.RemoveNamedFiles()
	if err == nil || strings.ContainsRune(err.Error(), '\x1b') || !strings.Contains(err.Error(), `\x1b`) {
		t.Fatal("foreign control byte was not escaped in the refusal")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("foreign-name refusal removed credentials")
	}
}

func TestOddLockAnomalyOnlyNamesSafeFinalComponent(t *testing.T) {
	survey := ScratchSurvey{oddLocks: []string{"/private-scratch/a\x1b]0;pwned\x07$(id)agctl-test-codex-ak-0001.lock", "/private-scratch/codex.lock"}}
	text := strings.Join(successfulChildReport(t, survey).anomalies(), "; ")
	assertNoCredentialNeedles(t, text)
	for _, fragment := range []string{"\x1b", "$(id)", "/private-scratch"} {
		if strings.Contains(text, fragment) {
			t.Fatal("an unnameable child path reached the refusal")
		}
	}
	for _, fragment := range []string{"codex.lock", "1 unnameable lock file(s)", "2 entr(y/ies)"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing report shape %q", fragment)
		}
	}
}

func TestForeignLockedCredentialCannotMutateNamespace(t *testing.T) {
	_, first, _, path := writerNamespace(t)
	_, second, _, _ := writerNamespace(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := mergedCredential(t, lockedRead(t, second))
	if _, err := first.Write(t.Context(), foreign); err == nil {
		t.Fatal("a foreign locked read wrote credentials")
	}
	if _, err := first.Park(t.Context(), foreign); err == nil {
		t.Fatal("a foreign locked read parked credentials")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(before, after); diff != "" {
		t.Fatal(diff)
	}
}
