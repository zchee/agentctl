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
	"bytes"
	json "encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestDoctorRemoveStale(t *testing.T) {
	tests := map[string]struct {
		name, kind, want string
		young, noYes     bool
	}{
		"success: old refresh directory":        {name: secret.RefreshLockName},
		"success: old storage directory":        {name: secret.StorageWriteLockName},
		"success: old legacy directory":         {name: "namespace.lock"},
		"error: fresh directory":                {name: secret.RefreshLockName, young: true, want: "staleness threshold"},
		"error: explicit confirmation required": {name: secret.RefreshLockName, noYes: true, want: "`--yes` is required"},
		"error: nonempty directory":             {name: secret.RefreshLockName, kind: "nonempty", want: "never a tree"},
		"error: regular file":                   {name: secret.RefreshLockName, kind: "file", want: doctorAnomalousFile},
		"error: symlink":                        {name: secret.RefreshLockName, kind: "symlink", want: "symbolic link"},
		"error: obsolete storage artefact":      {name: secret.LegacyStorageWriteArtefact, want: "removal and migration are unsupported"},
		"error: wrong filename":                 {name: "credential", want: "not a Claude Code lock artefact"},
		"error: bare suffix":                    {name: ".lock", want: "not a Claude Code lock artefact"},
		"error: namespace inode must remain":    {name: "namespace", kind: "namespace", want: "never unlinked"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			paths := config.NewPaths(fixture.ConfigDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			dir := paths.NamespaceDir(testutil.Acct, testutil.Org)
			if tt.kind == "namespace" {
				dir = paths.LocksDir()
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, tt.name)
			switch tt.kind {
			case "file", "namespace":
				doctorWrite(t, path, "keep")
			case "symlink":
				if err := os.Symlink(dir, path); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.kind == "nonempty" {
				doctorWrite(t, filepath.Join(path, "held"), "keep")
			}
			if !tt.young && tt.kind != "symlink" {
				doctorAge(t, path)
			}
			var out bytes.Buffer
			command := Doctor{Paths: paths, Out: &out, SampleInterval: time.Millisecond}
			err := command.RemoveStale(t.Context(), path, !tt.noYes)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("want %q, got %v", tt.want, err)
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("refusal changed lock: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("removed lock exists: %v", err)
				}
			}
			if _, err := os.Stat(secret.AuditLogPath(paths)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("explicit removal must not create an automatic-break audit event: %v", err)
			}
		})
	}
}

func TestDoctorRemovalPermit(t *testing.T) {
	tests := map[string]struct {
		record, live, wrong, linked bool
		want                        string
	}{
		"success: reaped writer attests the exact path": {record: true},
		"error: no attestation":                         {want: "is not inside"},
		"error: live writer":                            {record: true, live: true, want: "is being held, not leaked"},
		"error: sibling attestation is insufficient":    {record: true, wrong: true, want: "is not inside"},
		"error: symlinked record directory is ignored":  {record: true, linked: true, want: "is not inside"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			paths := config.NewPaths(fixture.ConfigDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(fixture.Home(), ".claude")
			path := filepath.Join(store, secret.RefreshLockName)
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			doctorAge(t, path)
			if tt.record {
				pid := os.Getpid()
				if !tt.live {
					child := exec.CommandContext(t.Context(), "/usr/bin/true")
					if err := child.Start(); err != nil {
						t.Fatal(err)
					}
					pid = child.Process.Pid
					if err := child.Wait(); err != nil {
						t.Fatal(err)
					}
				}
				attested := path
				if tt.wrong {
					attested = filepath.Join(store, secret.StorageWriteLockName)
				}
				record := secret.HeldLockRecord{WriterPID: uint32(pid), Tree: secret.TreeLive, StoreDir: store, Paths: []string{attested}, TakenAt: time.Now().UTC().Format(time.RFC3339Nano)}
				directory := secret.HeldLocksDir(paths)
				if tt.linked {
					directory = filepath.Join(fixture.Home(), "planted")
					if err := os.Symlink(directory, secret.HeldLocksDir(paths)); err != nil {
						t.Fatal(err)
					}
				}
				document, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				doctorWrite(t, filepath.Join(directory, "held.json"), string(document))
			}
			var out bytes.Buffer
			command := Doctor{Paths: paths, Out: &out, SampleInterval: time.Millisecond}
			err := command.RemoveStale(t.Context(), path, true)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("lock not removed: %v", err)
				}
				if !strings.Contains(out.String(), "is outside") {
					t.Fatalf("missing attestation warning: %s", &out)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("want %q, got %v", tt.want, err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("lock changed: %v", err)
				}
			}
		})
	}
}

func doctorAge(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * DoctorStaleMinAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
