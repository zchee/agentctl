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
	"testing"
)

func TestMove(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// prepare builds the layout under base and returns src, dst and the
		// pair to require, if any.
		prepare func(t *testing.T, base string) (src, dst string, want *FileID)
		// wantOK is set for the cases that must move.
		wantOK  bool
		wantErr error
	}{
		"success: directory with contents moves and keeps its inode": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				src := filepath.Join(base, "Claude")
				mkdir(t, src)
				writeFile(t, filepath.Join(src, "config.json"), "{}")
				id, err := StatID(src)
				if err != nil {
					t.Fatal(err)
				}
				return src, filepath.Join(base, "profiles", "fresh-00000000000000a1"), &id
			},
			wantOK: true,
		},
		"success: a path with spaces": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				src := filepath.Join(base, "Application Support", "Claude")
				mkdir(t, src)
				return src, filepath.Join(base, "profiles", "unknown-00000000000000a1"), nil
			},
			wantOK: true,
		},
		"error: destination exists as an empty directory": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				src := filepath.Join(base, "Claude")
				mkdir(t, src)
				dst := filepath.Join(base, "profiles", "fresh-00000000000000a1")
				mkdir(t, dst)
				return src, dst, nil
			},
			wantErr: ErrDestinationExists,
		},
		"error: destination exists as a file": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				src := filepath.Join(base, "Claude")
				mkdir(t, src)
				dst := filepath.Join(base, "profiles", "fresh-00000000000000a1")
				writeFile(t, dst, "x")
				return src, dst, nil
			},
			wantErr: ErrDestinationExists,
		},
		"error: source does not carry the recorded pair": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				src := filepath.Join(base, "Claude")
				mkdir(t, src)
				return src, filepath.Join(base, "profiles", "fresh-00000000000000a1"), &FileID{Dev: 1, Ino: 2}
			},
			wantErr: ErrSourceChanged,
		},
		"error: source is a symbolic link": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				real := filepath.Join(base, "real")
				mkdir(t, real)
				src := filepath.Join(base, "Claude")
				if err := os.Symlink(real, src); err != nil {
					t.Fatal(err)
				}
				return src, filepath.Join(base, "profiles", "fresh-00000000000000a1"), nil
			},
		},
		"error: destination parent is a symbolic link": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				src := filepath.Join(base, "Claude")
				mkdir(t, src)
				real := filepath.Join(base, "elsewhere")
				mkdir(t, real)
				if err := os.Remove(filepath.Join(base, "profiles")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, filepath.Join(base, "profiles")); err != nil {
					t.Fatal(err)
				}
				return src, filepath.Join(base, "profiles", "fresh-00000000000000a1"), nil
			},
		},
		"error: source missing": {
			prepare: func(t *testing.T, base string) (string, string, *FileID) {
				return filepath.Join(base, "Claude"), filepath.Join(base, "profiles", "fresh-00000000000000a1"), nil
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			mkdir(t, filepath.Join(base, "profiles"))
			src, dst, want := tt.prepare(t, base)
			srcBefore, srcErr := StatID(src)
			got, err := Move(src, dst, want)
			t.Logf("Move(%s, %s) = %v, %v", src, dst, got, err)
			if !tt.wantOK {
				if err == nil {
					t.Fatal("Move succeeded; want an error")
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Fatalf("Move error %v, want %v", err, tt.wantErr)
				}
				// A refused move leaves the source where it was.
				if srcErr == nil {
					if after, err := StatID(src); err != nil || after != srcBefore {
						t.Fatalf("the source moved on a refusal: %v %v", after, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Move: %v", err)
			}
			if got != srcBefore {
				t.Fatalf("Move returned %v, the source carried %v", got, srcBefore)
			}
			after, err := StatID(dst)
			if err != nil || after != srcBefore {
				t.Fatalf("destination carries %v (%v), want %v", after, err, srcBefore)
			}
			if ok, _ := Exists(src); ok {
				t.Fatal("the source path still exists")
			}
		})
	}
}

func TestMoveRefusesAcrossFileSystems(t *testing.T) {
	t.Parallel()
	// /dev is devfs on every Mac, so a rename from a temporary directory
	// into it can only fail; the source must stay where it was.
	base := t.TempDir()
	src := filepath.Join(base, "Claude")
	mkdir(t, src)
	if _, err := Move(src, "/dev/agentctl-desktop-test-never-created", nil); err == nil {
		t.Fatal("a rename onto another file system succeeded")
	} else {
		t.Logf("refused: %v", err)
	}
	if ok, _ := Exists(src); !ok {
		t.Fatal("the source disappeared")
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
