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

package secret

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/runtime/cleanup"
)

func TestAdoptedStoreStagingLifecycle(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		resolve string
		want    string
	}{
		"success: commit replaces the adopted copy":          {resolve: "commit", want: "displaced grant"},
		"success: discard leaves the adopted copy":           {resolve: "discard", want: "restored grant"},
		"success: emergency cleanup leaves the adopted copy": {resolve: "emergency", want: "restored grant"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := testStore(t)
			nsDir := paths.NamespaceDir("acct", "org")
			if err := os.MkdirAll(nsDir, config.DirMode); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(nsDir, AdoptedFile)
			if err := os.WriteFile(target, []byte("restored grant"), 0o644); err != nil {
				t.Fatal(err)
			}
			credentials := filepath.Join(nsDir, CredentialsFile)
			if err := os.WriteFile(credentials, []byte("store grant"), config.FileMode); err != nil {
				t.Fatal(err)
			}
			blob, err := NewSecret([]byte("displaced grant"))
			if err != nil {
				t.Fatal(err)
			}
			registry := new(cleanup.Registry)
			staged, err := stageAdopted(t.Context(), paths, nsDir, blob, registry)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Discard()
			tmp := filepath.Join(nsDir, staged.tmpName)
			info, err := os.Stat(tmp)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(config.FileMode, info.Mode().Perm()); diff != "" {
				t.Fatalf("temporary mode (-want +got):\n%s", diff)
			}
			requireAdoptedBytes(t, target, "restored grant")
			requireAdoptedBytes(t, tmp, "displaced grant")

			switch tt.resolve {
			case "commit":
				snap, err := CommitStaged(paths, staged)
				if err != nil {
					t.Fatal(err)
				}
				read, err := ReadAdopted(nsDir)
				if err != nil || !read.Present {
					t.Fatalf("ReadAdopted = present %t, error %v", read.Present, err)
				}
				if diff := gocmp.Diff(snap, read.Snap); diff != "" {
					t.Fatalf("committed snapshot (-want +got):\n%s", diff)
				}
				info, err = os.Stat(target)
				if err != nil || info.Mode().Perm() != config.FileMode {
					t.Fatalf("committed mode = %v, error %v", info, err)
				}
			case "discard":
				staged.Discard()
			case "emergency":
				registry.Run()
			}
			staged.Discard()
			if staged.unregister() {
				t.Fatal("resolved staging retained an emergency-cleanup entry")
			}
			if _, err := os.Lstat(tmp); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("temporary survived resolution: %v", err)
			}
			requireAdoptedBytes(t, target, tt.want)
			requireAdoptedBytes(t, credentials, "store grant")
			if _, err := os.Lstat(filepath.Join(nsDir, PendingFile)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("adoption must not write pending credentials: %v", err)
			}
			// A later peer may reuse the temporary name. Withdrawn cleanup
			// must not delete that peer's file, even after repeated Discard.
			if err := os.WriteFile(tmp, []byte("peer file"), config.FileMode); err != nil {
				t.Fatal(err)
			}
			registry.Run()
			staged.Discard()
			requireAdoptedBytes(t, tmp, "peer file")
			if _, err := CommitStaged(paths, staged); err == nil {
				t.Fatal("resolved staging committed twice")
			}
		})
	}
}

func TestAdoptedStoreWriteAndCancellation(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		cancel  bool
		invalid bool
	}{
		"success: write creates an adopted copy only":  {},
		"error: cancellation discards staging":         {cancel: true},
		"error: an invalid secret leaves no temporary": {invalid: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := testStore(t)
			nsDir := paths.NamespaceDir("acct", "org")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			blob, err := NewSecret([]byte("displaced grant"))
			if err != nil {
				t.Fatal(err)
			}
			if tt.invalid {
				blob = nil
			}
			_, err = WriteAdopted(ctx, paths, nsDir, blob)
			if tt.cancel {
				if _, ok := errors.AsType[*WriteCancelledError](err); !ok {
					t.Fatalf("want WriteCancelledError, got %v", err)
				}
			} else if (err != nil) != tt.invalid {
				t.Fatalf("WriteAdopted error = %v, want error %t", err, tt.invalid)
			}
			entries, err := os.ReadDir(nsDir)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, entry := range entries {
				got = append(got, entry.Name())
			}
			var want []string
			if !tt.cancel && !tt.invalid {
				want = []string{AdoptedFile}
				requireAdoptedBytes(t, filepath.Join(nsDir, AdoptedFile), "displaced grant")
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Fatalf("namespace contents (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAdoptedStoreRefusesUnsafePaths(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ kind string }{
		"error: target symlink":         {kind: "target link"},
		"error: target directory":       {kind: "target directory"},
		"error: target fifo":            {kind: "target fifo"},
		"error: namespace symlink":      {kind: "namespace link"},
		"error: outside namespace root": {kind: "outside"},
		"error: missing paths":          {kind: "nil paths"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := testStore(t)
			nsDir := paths.NamespaceDir("acct", "org")
			if err := os.MkdirAll(nsDir, config.DirMode); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			victim := filepath.Join(outside, "untouched")
			if err := os.WriteFile(victim, []byte("foreign grant"), config.FileMode); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(nsDir, AdoptedFile)
			switch tt.kind {
			case "target link":
				if err := os.Symlink(victim, target); err != nil {
					t.Fatal(err)
				}
			case "target directory":
				if err := os.Mkdir(target, config.DirMode); err != nil {
					t.Fatal(err)
				}
			case "target fifo":
				if err := unix.Mkfifo(target, uint32(config.FileMode)); err != nil {
					t.Fatal(err)
				}
			case "namespace link":
				if err := os.Remove(nsDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, nsDir); err != nil {
					t.Fatal(err)
				}
			case "outside":
				nsDir = outside
			case "nil paths":
				paths = nil
			}
			blob, err := NewSecret([]byte("displaced grant"))
			if err != nil {
				t.Fatal(err)
			}
			staged, err := StageAdopted(t.Context(), paths, nsDir, blob)
			if err == nil {
				staged.Discard()
				t.Fatal("unsafe staging was accepted")
			}
			switch tt.kind {
			case "target link", "namespace link":
				if _, ok := errors.AsType[*SymlinkRefusedError](err); !ok {
					t.Fatalf("want SymlinkRefusedError, got %v", err)
				}
			case "target directory", "target fifo":
				if _, ok := errors.AsType[*NotRegularError](err); !ok {
					t.Fatalf("want NotRegularError, got %v", err)
				}
			case "outside", "nil paths":
				if _, ok := errors.AsType[*OutsideRootError](err); !ok {
					t.Fatalf("want OutsideRootError, got %v", err)
				}
			}
			requireAdoptedBytes(t, victim, "foreign grant")
			matches, err := filepath.Glob(filepath.Join(nsDir, AdoptedFile+".tmp.*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("refusal left temporaries %v, error %v", matches, err)
			}
		})
	}
}

func TestAdoptedStoreDirectoryReplacementAndRenameFailure(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ kind string }{
		"error: namespace replaced by symlink":         {kind: "symlink"},
		"error: namespace replaced by plain directory": {kind: "directory"},
		"success: cleanup after directory rename":      {kind: "cleanup"},
		"error: adopted target became a directory":     {kind: "rename"},
		"error: different namespace root at commit":    {kind: "root"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := testStore(t)
			nsDir := paths.NamespaceDir("acct", "org")
			blob, err := NewSecret([]byte("displaced grant"))
			if err != nil {
				t.Fatal(err)
			}
			registry := new(cleanup.Registry)
			staged, err := stageAdopted(t.Context(), paths, nsDir, blob, registry)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Discard()
			target := filepath.Join(nsDir, AdoptedFile)
			if err := os.WriteFile(target, []byte("restored grant"), config.FileMode); err != nil {
				t.Fatal(err)
			}
			original := nsDir
			switch tt.kind {
			case "symlink", "directory", "cleanup":
				original = nsDir + "-moved"
				if err := os.Rename(nsDir, original); err != nil {
					t.Fatal(err)
				}
				if tt.kind == "directory" {
					if err := os.Mkdir(nsDir, config.DirMode); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(t.TempDir(), nsDir); err != nil {
					t.Fatal(err)
				}
			case "rename":
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, config.DirMode); err != nil {
					t.Fatal(err)
				}
			case "root":
				paths = testStore(t)
			}
			if tt.kind == "cleanup" {
				registry.Run()
			} else if _, err := CommitStaged(paths, staged); err == nil {
				t.Fatal("commit accepted an invalid destination")
			}
			if staged.unregister() {
				t.Fatal("failed commit retained emergency cleanup")
			}
			if _, err := os.Lstat(filepath.Join(original, staged.tmpName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("temporary survived failed commit/cleanup: %v", err)
			}
			if tt.kind != "rename" {
				requireAdoptedBytes(t, filepath.Join(original, AdoptedFile), "restored grant")
			}
		})
	}
}

func TestAdoptedStoreCleanupRacesWithCommit(t *testing.T) {
	t.Parallel()
	paths := testStore(t)
	nsDir := paths.NamespaceDir("acct", "org")
	blob, err := NewSecret([]byte("displaced grant"))
	if err != nil {
		t.Fatal(err)
	}
	registry := new(cleanup.Registry)
	staged, err := stageAdopted(t.Context(), paths, nsDir, blob, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	var wg sync.WaitGroup
	wg.Go(registry.Run)
	_, commitErr := CommitStaged(paths, staged)
	wg.Wait()
	if _, err := os.Lstat(filepath.Join(nsDir, staged.tmpName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temporary survived concurrent resolution: %v", err)
	}
	if commitErr == nil {
		requireAdoptedBytes(t, filepath.Join(nsDir, AdoptedFile), "displaced grant")
	} else if _, err := os.Lstat(filepath.Join(nsDir, AdoptedFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cleanup won but adopted file appeared: %v (commit %v)", err, commitErr)
	}
}

func requireAdoptedBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(want, string(got)); diff != "" {
		t.Fatalf("file %q (-want +got):\n%s", path, diff)
	}
}
