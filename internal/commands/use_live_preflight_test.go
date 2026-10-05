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
)

func TestUseWriteMissingStoreRefusesBeforeKeychain(t *testing.T) {
	tests := map[string]struct {
		tree   secret.Tree
		reason claude.SwapRefusalKind
	}{
		"error: missing owned store is a lock refusal": {tree: secret.TreeOwn, reason: claude.SwapCompromisedHold},
		"error: missing live store is unreachable":     {tree: secret.TreeLive, reason: claude.SwapLiveUnreachable},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			paths := config.NewPaths(filepath.Join(root, "registry"))
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			env := claude.EnvWithHome(root)
			subject := &useSubject{tree: test.tree, service: "unwritten-service", storeDir: filepath.Join(paths.NamespaceRoot(), "missing")}
			if test.tree == secret.TreeLive {
				subject.storeDir = claude.LiveStoreDir(&env)
			}
			report := useWrite(t.Context(), useWritePhase{paths: paths, env: &env, subject: subject, toDigest8: "aabbccdd"}, nil)
			if diff := gocmp.Diff(claude.SwapOutcome{Kind: claude.SwapRefused, Refusal: claude.SwapRefusal{Kind: test.reason}}, report.outcome); diff != "" {
				t.Fatal(diff)
			}
			if report.lock.HoldMS != nil || report.auditID != nil {
				t.Fatalf("missing store took a hold or wrote an audit: %+v", report)
			}
			if report.lock.BudgetMS == nil || *report.lock.BudgetMS != uint64(secret.HoldBudget.Milliseconds()) {
				t.Fatal("missing protocol budget")
			}
			if _, err := os.Stat(subject.storeDir); !os.IsNotExist(err) {
				t.Fatalf("missing store was created: %v", err)
			}
		})
	}
}

func TestUseRefreshPreflightRefusesWithoutExposingPair(t *testing.T) {
	tests := map[string]struct {
		adopted bool
		pending bool
		want    string
	}{
		"error: own store peer lock blocks save":   {want: "a Claude Code session holds"},
		"error: missing adopted copy blocks save":  {adopted: true, want: "can no longer be read"},
		"error: pending adopted write blocks save": {adopted: true, pending: true, want: "unresolved pending write"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			record := &config.AccountRecord{AccountUUID: "account", OrganizationUUID: "organization", Kind: config.AccountKind{Owned: &config.OwnedKind{ExportSHA8: "aabbccdd"}}}
			dir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			if test.adopted {
				if test.pending {
					if err := os.WriteFile(filepath.Join(dir, secret.PendingFile), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				err = useWriteBackAdopted(t.Context(), paths, dir, nil, claude.Digests{})
			} else {
				if err := os.Mkdir(filepath.Join(dir, secret.RefreshLockName), 0o700); err != nil {
					t.Fatal(err)
				}
				err = useGuardedWriteBack(t.Context(), paths, record, nil, claude.Digests{})
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
			if _, err := os.Stat(filepath.Join(dir, secret.CredentialsFile)); !os.IsNotExist(err) {
				t.Fatalf("preflight created credential file: %v", err)
			}
		})
	}
}
