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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestStoredNames lists profiles the way status does: only names in the
// profile grammar, sorted, and never through a profiles directory that a
// link has replaced, which would list another directory's profiles.
func TestStoredNames(t *testing.T) {
	const (
		a = "11111111-2222-3333-4444-555555555555"
		b = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	tests := map[string]struct {
		prepare func(t *testing.T, dir, elsewhere string)
		want    []string
		wantErr bool
	}{
		"success: grammar names sorted, anything else left out": {
			prepare: func(t *testing.T, dir, _ string) {
				for _, name := range []string{b, a, "notes"} {
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: []string{a, b},
		},
		"success: an absent profiles directory lists nothing": {
			prepare: func(t *testing.T, dir, _ string) {
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
			},
		},
		"error: a profiles directory replaced by a link is not listed": {
			prepare: func(t *testing.T, dir, elsewhere string) {
				if err := os.Mkdir(filepath.Join(elsewhere, a), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, dir); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dir, elsewhere := filepath.Join(base, "profiles"), filepath.Join(base, "elsewhere")
			for _, d := range []string{dir, elsewhere} {
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			tt.prepare(t, dir, elsewhere)
			got, err := storedNames(dir)
			t.Logf("names %v error %v", got, err)
			if (err != nil) != tt.wantErr {
				t.Fatalf("storedNames error = %v, want error %v", err, tt.wantErr)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("storedNames (-want +got):\n%s", diff)
			}
		})
	}
}
