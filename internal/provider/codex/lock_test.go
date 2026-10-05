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
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func TestCodexLockExclusionAndBudgets(t *testing.T) {
	tests := map[string]struct{ cancelled bool }{
		"error: timed out waiter": {},
		"error: cancelled waiter": {cancelled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, guard, path := writerNamespace(t)
			owned := namespace.Owned()
			expected, err := paths.CodexLockPath("user-one", "acct-one")
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(expected, guard.Path()); diff != "" {
				t.Fatal(diff)
			}
			if filepath.Dir(guard.Path()) != paths.CodexLocksDir() || filepath.Dir(guard.Path()) == filepath.Dir(path) {
				t.Fatal("namespace lock is not external to credential directory")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelled {
				cancel()
			}
			if other, err := AcquireCodex(ctx, paths, owned, 0); err == nil || other != nil {
				t.Fatalf("contended acquisition=%v error=%v", other, err)
			}
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			reacquired, err := AcquireCodex(t.Context(), paths, owned, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := reacquired.Release(); err != nil {
					t.Error(err)
				}
			}()
			if reacquired.Path() != expected {
				t.Fatal("reacquisition changed the lock path")
			}
		})
	}
}

func TestInstallAndOwnedProofUseSameLock(t *testing.T) {
	paths, namespace, guard, _ := writerNamespace(t)
	scratch := t.TempDir()
	writeCodexFile(t, filepath.Join(scratch, authFile), writerCredentials(t, "agctl-test-login"))
	login, err := VerifyLogin(t.Context(), scratch, successfulChildReport(t, ScratchSurvey{}))
	if err != nil {
		t.Fatal(err)
	}
	owned := login.OwnedRecord()
	if owned.ExportSpelling() != nil || owned.Refresh() != config.RefreshAuto {
		t.Fatal("verified login produced an imported or read-only proof")
	}
	if _, err := AcquireCodexForInstall(t.Context(), paths, login, 0); err == nil {
		t.Fatal("install bypassed the held owned lock")
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	install, err := AcquireCodexForInstall(t.Context(), paths, login, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := install.Release(); err != nil {
			t.Error(err)
		}
	}()
	if install.Path() != guard.Path() {
		t.Fatal("install used a different lock")
	}
	if _, err := namespace.Read(); err == nil {
		t.Fatal("a reacquired lock revived a stale namespace")
	}
}

func TestZeroProofCreatesNoLock(t *testing.T) {
	paths := config.NewPaths(t.TempDir())
	if _, err := AcquireCodex(t.Context(), paths, &OwnedRecord{}, time.Second); err == nil {
		t.Fatal("zero owned proof acquired a lock")
	}
	if _, err := AcquireCodexForInstall(t.Context(), paths, &VerifiedLogin{}, time.Second); err == nil {
		t.Fatal("zero login acquired a lock")
	}
	if _, err := os.Lstat(paths.CodexRoot()); !os.IsNotExist(err) {
		t.Fatalf("invalid proofs created store: %v", err)
	}
}
