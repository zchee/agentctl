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

//go:build darwin

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// walkHookMu serializes the tests that set the package-level walk hook.
var walkHookMu sync.Mutex

func TestRemoveProfile(t *testing.T) {
	const name = "fresh-00000000000000a1"
	tests := map[string]struct {
		prepare func(t *testing.T, profiles, outside string)
		hook    func(t *testing.T, outside string) func(int, string)
		// wantRemoved is whether the profile must be gone afterwards.
		wantRemoved bool
		wantPartial bool
		// wantExact is whether a partial count must be exact (every
		// remaining directory listed) rather than a lower bound.
		wantExact bool
		wantErr   error
		// check runs last, for what must still be on disk.
		check func(t *testing.T, profile string)
	}{
		"success: nested tree with files and empty directories": {
			prepare: func(t *testing.T, profiles, _ string) {
				p := filepath.Join(profiles, name)
				mkdir(t, filepath.Join(p, "claude-code", "bin"))
				mkdir(t, filepath.Join(p, "Local Storage", "leveldb"))
				writeFile(t, filepath.Join(p, "config.json"), "{}")
				writeFile(t, filepath.Join(p, "claude-code", "bin", "claude"), "binary")
			},
			wantRemoved: true,
		},
		"error: the profile itself is a symbolic link": {
			prepare: func(t *testing.T, profiles, outside string) {
				if err := os.Symlink(outside, filepath.Join(profiles, name)); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrSymlinkInProfile,
		},
		"error: a link inside the profile stops the walk and is left in place": {
			prepare: func(t *testing.T, profiles, outside string) {
				p := filepath.Join(profiles, name)
				mkdir(t, p)
				writeFile(t, filepath.Join(p, "a"), "x")
				if err := os.Symlink(outside, filepath.Join(p, "link")); err != nil {
					t.Fatal(err)
				}
			},
			wantPartial: true,
			wantExact:   true,
			wantErr:     ErrSymlinkInProfile,
			check: func(t *testing.T, profile string) {
				if _, err := os.Lstat(filepath.Join(profile, "link")); err != nil {
					t.Errorf("the link was removed: %v", err)
				}
			},
		},
		"error: a directory replaced by a link mid-walk": {
			prepare: func(t *testing.T, profiles, _ string) {
				p := filepath.Join(profiles, name)
				mkdir(t, filepath.Join(p, "sub"))
				writeFile(t, filepath.Join(p, "sub", "f"), "x")
			},
			hook: func(t *testing.T, outside string) func(int, string) {
				done := false
				return func(fd int, dirName string) {
					if done || dirName != name {
						return
					}
					done = true
					if err := unix.Renameat(fd, "sub", fd, "sub.moved"); err != nil {
						t.Errorf("renaming sub: %v", err)
					}
					if err := unix.Symlinkat(outside, fd, "sub"); err != nil {
						t.Errorf("planting the link: %v", err)
					}
				}
			},
			wantPartial: true,
			wantExact:   true,
			wantErr:     ErrSymlinkInProfile,
		},
		"error: a file replaced by a link mid-walk": {
			prepare: func(t *testing.T, profiles, _ string) {
				p := filepath.Join(profiles, name)
				mkdir(t, p)
				writeFile(t, filepath.Join(p, "f"), "x")
			},
			hook: func(t *testing.T, outside string) func(int, string) {
				return func(fd int, dirName string) {
					if dirName != name {
						return
					}
					if err := unix.Unlinkat(fd, "f", 0); err != nil {
						t.Errorf("removing f: %v", err)
					}
					if err := unix.Symlinkat(filepath.Join(outside, "keep"), fd, "f"); err != nil {
						t.Errorf("planting the link: %v", err)
					}
				}
			},
			wantPartial: true,
			wantExact:   true,
			wantErr:     ErrSymlinkInProfile,
		},
		"error: an unremovable directory stops the walk with a partial count": {
			prepare: func(t *testing.T, profiles, _ string) {
				p := filepath.Join(profiles, name)
				mkdir(t, filepath.Join(p, "locked"))
				writeFile(t, filepath.Join(p, "locked", "f"), "x")
				if err := os.Chmod(filepath.Join(p, "locked"), 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(p, "locked"), 0o700) })
			},
			wantPartial: true,
			wantExact:   true,
		},
		"error: an unlistable directory makes the count a lower bound": {
			prepare: func(t *testing.T, profiles, _ string) {
				p := filepath.Join(profiles, name)
				mkdir(t, filepath.Join(p, "sealed", "inner"))
				writeFile(t, filepath.Join(p, "sealed", "inner", "f"), "x")
				if err := os.Chmod(filepath.Join(p, "sealed"), 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(p, "sealed"), 0o700) })
			},
			wantPartial: true,
			wantExact:   false,
		},
	}
	for testName, tt := range tests {
		t.Run(testName, func(t *testing.T) {
			if os.Geteuid() == 0 && tt.wantPartial && tt.hook == nil {
				t.Skip("root ignores directory permissions")
			}
			base := t.TempDir()
			profiles, outside := filepath.Join(base, "profiles"), filepath.Join(base, "outside")
			mkdir(t, profiles)
			mkdir(t, outside)
			writeFile(t, filepath.Join(outside, "keep"), "must survive")
			tt.prepare(t, profiles, outside)
			if tt.hook != nil {
				walkHookMu.Lock()
				walkHook = tt.hook(t, outside)
				t.Cleanup(func() { walkHook = nil; walkHookMu.Unlock() })
			}
			err := RemoveProfile(profiles, name)
			t.Logf("RemoveProfile: %v", err)
			if data, readErr := os.ReadFile(filepath.Join(outside, "keep")); readErr != nil || string(data) != "must survive" {
				t.Fatalf("a file outside the profile changed: %q %v", data, readErr)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error %v, want %v", err, tt.wantErr)
			}
			var partial *PartialRemoveError
			if tt.wantPartial {
				if !errors.As(err, &partial) || partial.Remaining == 0 {
					t.Fatalf("error %v, want a partial removal with a count", err)
				}
				t.Logf("remaining %d, complete %v: %s", partial.Remaining, partial.Complete, partial.RemainingText())
				if partial.Complete != tt.wantExact {
					t.Fatalf("count complete = %v, want %v", partial.Complete, tt.wantExact)
				}
				if !tt.wantExact && !strings.HasPrefix(partial.RemainingText(), "at least ") {
					t.Fatalf("a lower bound reads %q, want it to say at least", partial.RemainingText())
				}
			}
			if tt.check != nil {
				tt.check(t, filepath.Join(profiles, name))
			}
			exists, _ := Exists(filepath.Join(profiles, name))
			if tt.wantRemoved {
				if err != nil || exists {
					t.Fatalf("profile not removed: err %v, exists %v", err, exists)
				}
				return
			}
			if err == nil {
				t.Fatal("RemoveProfile succeeded; want an error")
			}
		})
	}
}

func TestRemoveProfileRefusesBadNames(t *testing.T) {
	t.Parallel()
	profiles := t.TempDir()
	for _, name := range []string{"", ".", "..", "../x", "a/b", "Claude", "fresh-xyz"} {
		if err := RemoveProfile(profiles, name); err == nil {
			t.Fatalf("RemoveProfile(%q) succeeded", name)
		}
	}
}
