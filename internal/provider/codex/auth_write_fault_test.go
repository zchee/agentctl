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

package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/secret"
)

func TestCodexWriterNamedFaults(t *testing.T) {
	tests := map[string]struct {
		fault          string
		pending, fails bool
	}{
		"success: refresh failure parks grant":              {fault: "codex_rename_fail", pending: true},
		"success: generic failure does not affect codex":    {fault: "rename_fail"},
		"error: post rename inspection leaves landed grant": {fault: "codex_error_after_rename", fails: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, namespace, _, path := writerNamespace(t)
			old := lockedRead(t, namespace).Credentials().Digests()
			merged := mergedCredential(t, lockedRead(t, namespace))
			t.Setenv("AGENTCTL_FAULT", tt.fault)
			result, err := namespace.Write(t.Context(), merged)
			if (err != nil) != tt.fails {
				t.Fatalf("write error=%v; fails=%t", err, tt.fails)
			}
			if !tt.fails {
				if result.Outcome.SavedToPending != tt.pending {
					t.Fatalf("saved to pending=%t; want=%t", result.Outcome.SavedToPending, tt.pending)
				}
				if err := result.Receipt.Consume(); err != nil {
					t.Fatal(err)
				}
			}
			want := merged.Credentials().Digests()
			if tt.pending {
				want = old
			}
			if diff := gocmp.Diff(want, ReadAuth(t.Context(), filepath.Dir(path)).Credentials.Digests()); diff != "" {
				t.Fatal(diff)
			}
			if tt.pending {
				t.Setenv("AGENTCTL_FAULT", "")
				decision, receipt, _, err := namespace.ResolvePending(t.Context())
				if err != nil || decision.Kind != secret.PendingReplayed || receipt == nil {
					t.Fatalf("decision=%+v receipt=%v err=%v", decision, receipt, err)
				}
				if err := receipt.Consume(); err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(merged.Credentials().Digests(), ReadAuth(t.Context(), filepath.Dir(path)).Credentials.Digests()); diff != "" {
					t.Fatal(diff)
				}
			}
		})
	}
}

func TestCodexInstallRenameFailureKeepsPreviousGrant(t *testing.T) {
	paths, namespace, guard, path := writerNamespace(t)
	before := lockedRead(t, namespace).Credentials().Digests()
	scratch := t.TempDir()
	writeCodexFile(t, filepath.Join(scratch, authFile), writerCredentials(t, "agctl-test-login"))
	login, err := VerifyLogin(t.Context(), scratch, successfulChildReport(t, ScratchSurvey{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_FAULT", "codex_install_rename_fail")
	if receipt, _, err := WriteAuth(t.Context(), paths, login, guard); err == nil || receipt != nil {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if diff := gocmp.Diff(before, ReadAuth(t.Context(), filepath.Dir(path)).Credentials.Digests()); diff != "" {
		t.Fatal(diff)
	}
	for _, name := range []string{pendingFile, pendingMeta} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(path), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must not exist: %v", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != authFile {
		t.Fatalf("install left %v", entries)
	}
}
