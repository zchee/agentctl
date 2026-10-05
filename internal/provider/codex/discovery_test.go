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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
)

func TestReadAuthOutcomes(t *testing.T) {
	tests := map[string]struct {
		kind    ResolvedKind
		prepare func(*testing.T, string)
	}{"success: absent": {ResolvedAbsent, func(*testing.T, string) {}}, "success: credential": {ResolvedCredentials, func(t *testing.T, path string) { writeCodexFile(t, path, codexFixture(t, "auth-codex-format.json")) }}, "error: empty": {ResolvedTorn, func(t *testing.T, path string) { writeCodexFile(t, path, nil) }}, "error: truncated": {ResolvedTorn, func(t *testing.T, path string) { writeCodexFile(t, path, []byte(`{"tokens":`)) }}, "error: malformed": {ResolvedTransient, func(t *testing.T, path string) { writeCodexFile(t, path, []byte(`{"tokens":"agctl-test-secret"}`)) }}, "error: directory": {ResolvedTransient, func(t *testing.T, path string) {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}}, "error: fifo": {ResolvedTransient, func(t *testing.T, path string) {
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}}, "error: oversized": {ResolvedTransient, func(t *testing.T, path string) {
		writeCodexFile(t, path, []byte(strings.Repeat(" ", int(secret.MaxCredentialsBytes)+1)))
	}}, "error: symlink": {ResolvedTransient, func(t *testing.T, path string) {
		target := filepath.Join(t.TempDir(), "target")
		writeCodexFile(t, target, []byte(`{}`))
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			tt.prepare(t, filepath.Join(home, ShownName()))
			got := ReadAuth(t.Context(), home)
			if got.Kind != tt.kind {
				t.Fatalf("outcome=%+v want %s", got, tt.kind)
			}
			if strings.Contains(got.Reason, "agctl-test-secret") {
				t.Fatal("reason exposes data")
			}
		})
	}
	home := t.TempDir()
	writeCodexFile(t, filepath.Join(home, ShownName()), []byte(`{}`))
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	if got := ReadAuth(t.Context(), link); got.Kind != ResolvedCredentials {
		t.Fatalf("parent link outcome=%+v", got)
	}
}

func ownedTestRecord(user, account string) config.CodexAccountRecord {
	return config.CodexAccountRecord{ChatGPTUserID: user, ChatGPTAccountID: account, Kind: config.CodexKind{Owned: &config.CodexOwnedKind{ExportSpelling: "/not-a-write-target", Refresh: config.RefreshAuto}}}
}

func TestDiscoverySources(t *testing.T) {
	root := t.TempDir()
	paths := config.NewPaths(filepath.Join(root, "store"))
	records := []config.CodexAccountRecord{ownedTestRecord("user-one", "acct-one"), {Kind: config.CodexKind{HomeReadOnly: &config.CodexHomeReadOnlyKind{Dir: "elsewhere"}}}, ownedTestRecord("user-forgotten", "acct-one"), ownedTestRecord(".locks", "acct-one"), {Kind: config.CodexKind{Live: true}}}
	records[2].Forgotten = true
	dir, err := paths.CodexNamespaceDir("user-one", "acct-one")
	if err != nil {
		t.Fatal(err)
	}
	writeCodexFile(t, filepath.Join(dir, "app-server-daemon", "daemon.lock"), nil)
	tests := map[string]struct {
		env       Env
		want      []SourceKind
		liveError bool
	}{"success: live first": {Env{Home: root}, []SourceKind{SourceLive, SourceOwned, SourceHomeReadOnly, SourceInvalidOwned}, false}, "error: live unreadable retains registry": {Env{CodexHome: filepath.Join(root, "missing")}, []SourceKind{SourceOwned, SourceHomeReadOnly, SourceInvalidOwned}, true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Discover(t.Context(), paths, records, tt.env)
			if (err != nil) != tt.liveError {
				t.Fatalf("live error=%v", err)
			}
			var kinds []SourceKind
			for _, source := range got {
				kinds = append(kinds, source.Kind)
				if source.Kind == SourceOwned && (source.Owned.User() != "user-one" || source.Evidence.Kind != DaemonArtefact) {
					t.Fatalf("owned source=%+v", source)
				}
			}
			if diff := gocmp.Diff(tt.want, kinds); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestDiscoveryOrphans(t *testing.T) {
	paths := config.NewPaths(filepath.Join(t.TempDir(), "store"))
	if got, err := Orphans(t.Context(), paths, nil, time.Unix(1800000000, 0)); err != nil || len(got) != 0 {
		t.Fatalf("empty orphans=%v error=%v", got, err)
	}
	records := []config.CodexAccountRecord{ownedTestRecord("user-present", "acct-one"), ownedTestRecord("user-empty", "acct-one")}
	dir, err := paths.CodexNamespaceDir("user-present", "acct-one")
	if err != nil {
		t.Fatal(err)
	}
	writeCodexFile(t, filepath.Join(dir, ShownName()), []byte(`{}`))
	orphanDir, err := paths.CodexNamespaceDir("user-orphan", "acct-two")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(paths.CodexScratchRoot(), ScratchPrefix+"old")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1800000000, 0)
	if err := os.Chtimes(scratch, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(paths.CodexRoot(), "linked")
	if err := os.Symlink(orphanDir, linked); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		now  time.Time
		want []OrphanKind
	}{"success: recent scratch excluded": {stamp.Add(time.Second), []OrphanKind{OrphanNamespace, OrphanRecord}}, "success: stale scratch included": {stamp.Add(ScratchStaleAfter + time.Second), []OrphanKind{OrphanNamespace, OrphanRecord, OrphanScratch}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Orphans(t.Context(), paths, records, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			var kinds []OrphanKind
			for _, orphan := range got {
				kinds = append(kinds, orphan.Kind)
			}
			if diff := gocmp.Diff(tt.want, kinds); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
