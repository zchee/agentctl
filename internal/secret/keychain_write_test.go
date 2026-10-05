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

package secret

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/testutil"
)

func standInWriter(t *testing.T) (*KeychainWriter, *testutil.Fixture) {
	t.Helper()
	f := fakeKeychain(t)
	t.Setenv(keychainBackendEnv, "")
	t.Setenv(securityBinEnv, f.SecurityBin())
	t.Setenv("USER", testutil.KeychainAccount)
	return NewKeychainWriter(), f
}

func updateLine(t *testing.T, service, blob string) *KeychainStdinLine {
	t.Helper()
	line, err := NewKeychainStdinLine(testutil.KeychainAccount, service, []byte(blob))
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestKeychainWriteUpsertsThroughStdinOnly(t *testing.T) {
	tests := map[string]struct{ existing bool }{
		"success: creates an absent item":                    {},
		"success: updates without removing an existing item": {existing: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			writer, f := standInWriter(t)
			const service = "Claude Code-credentials-12345678"
			const blob = `{"token":"sk-ant-planted-write-marker"}`
			f.AllowWrite(service)
			if tt.existing {
				f.KeychainItem(service, "old grant")
			}
			if err := writer.Write(t.Context(), service, updateLine(t, service, blob)); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(f.KeychainItemPath(service))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(blob, string(got)); diff != "" {
				t.Errorf("stored document (-want +got):\n%s", diff)
			}
			want := []string{"-i", fmt.Sprintf(`add-generic-password -U -a "example" -s "%s" -X <REDACTED:%d>`, service, 2*len(blob))}
			if diff := gocmp.Diff(want, f.SecurityLog()); diff != "" {
				t.Errorf("argv log (-want +got):\n%s", diff)
			}
			for _, logged := range f.SecurityLog() {
				if strings.Contains(logged, "sk-ant-") || strings.Contains(logged, hex.EncodeToString([]byte(blob))) {
					t.Fatal("credential escaped into the argv log")
				}
			}
		})
	}
}

func TestKeychainWriteRefusesBeforeSpawn(t *testing.T) {
	tests := map[string]struct{ mutate func(*KeychainStdinLine) }{
		"error: account mismatch":                    {mutate: func(l *KeychainStdinLine) { l.account = "other" }},
		"error: service mismatch":                    {mutate: func(l *KeychainStdinLine) { l.service = "other" }},
		"error: oversized line at the last boundary": {mutate: func(l *KeychainStdinLine) { l.size = KeychainLineLimit + 1 }},
		"error: empty line":                          {mutate: func(l *KeychainStdinLine) { l.line = nil }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			writer, f := standInWriter(t)
			line := updateLine(t, "target", "document")
			tt.mutate(line)
			if err := writer.Write(t.Context(), "target", line); err == nil {
				t.Fatal("invalid line was accepted")
			}
			if got := f.SecurityLog(); len(got) != 0 {
				t.Fatalf("refused write spawned a child: %v", got)
			}
		})
	}
}

func TestKeychainWriteClassifiesFailuresAndPreservesOldGrant(t *testing.T) {
	tests := map[string]struct {
		exit   string
		stderr string
		class  errs.KeychainClass
	}{
		"error: locked exit":                   {exit: "36", class: errs.KeychainLocked},
		"error: absent on upsert is a failure": {exit: "44", class: errs.KeychainNotFound},
		"error: interaction refused":           {exit: "1", stderr: "errSecInteractionNotAllowed", class: StderrInteractionNotAllowed.KeychainClass()},
		"error: unknown failure":               {exit: "1", stderr: "unrecognised failure", class: StderrOther.KeychainClass()},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGCTL_FAKE_SECURITY_WRITE_EXIT", tt.exit)
			t.Setenv("AGCTL_FAKE_SECURITY_STDERR", tt.stderr)
			writer, f := standInWriter(t)
			f.AllowWrite("target").KeychainItem("target", "old grant")
			err := writer.Write(t.Context(), "target", updateLine(t, "target", "new grant"))
			ke, ok := errors.AsType[*KeychainError](err)
			if !ok || ke.Class != tt.class || !ke.Transient {
				t.Fatalf("error = %v, want transient %s", err, tt.class)
			}
			got, err := os.ReadFile(f.KeychainItemPath("target"))
			if err != nil || string(got) != "old grant" {
				t.Fatalf("previous credential changed: %q, %v", got, err)
			}
		})
	}
}

func TestKeychainWriterFailsClosedWithoutAStandIn(t *testing.T) {
	t.Setenv(keychainBackendEnv, "")
	t.Setenv(securityBinEnv, "")
	t.Setenv("USER", testutil.KeychainAccount)
	if err := NewKeychainWriter().Write(t.Context(), "target", updateLine(t, "target", "blob")); !errors.Is(err, ErrKeychainUnsupported) {
		t.Fatalf("unconfigured write = %v, want unsupported", err)
	}
}

func TestKeychainWriteDeadlineKillsAndReapsTheChild(t *testing.T) {
	tests := map[string]struct{ cancelled bool }{
		"error: write budget":        {},
		"error: caller cancellation": {cancelled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGCTL_FAKE_SECURITY_SLEEP", "2")
			writer, _ := standInWriter(t)
			writer.budget = 100 * time.Millisecond
			ctx := t.Context()
			if tt.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			err := writer.Write(ctx, "target", updateLine(t, "target", "blob"))
			if !errors.Is(err, ErrKeychainTimeout) {
				t.Fatalf("write = %v, want timeout", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("write waited for inherited pipes: %v", elapsed)
			}
		})
	}
}

func TestKeychainWriteMissingBinaryIsPermanent(t *testing.T) {
	writer, _ := standInWriter(t)
	writer.bin = "/nonexistent/security"
	err := writer.Write(t.Context(), "target", updateLine(t, "target", "blob"))
	ke, ok := errors.AsType[*KeychainError](err)
	if !ok || ke.Class != errs.KeychainUnavailable || ke.Transient {
		t.Fatalf("write = %v, want permanent unavailable", err)
	}
}
