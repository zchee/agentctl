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

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestStatDeviceDoesNotFollowTheFinalLink(t *testing.T) {
	tests := map[string]struct {
		kind    string
		wantErr bool
	}{
		"success: a directory yields its device":            {kind: "directory"},
		"success: a dangling final symlink is not followed": {kind: "symlink"},
		"error: an absent path reports its lstat failure":   {kind: "absent", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var stat unix.Stat_t
			if err := unix.Lstat(dir, &stat); err != nil {
				t.Fatal(err)
			}
			path := dir
			switch tt.kind {
			case "symlink":
				path = filepath.Join(dir, "link")
				if err := os.Symlink(filepath.Join(dir, "missing-target"), path); err != nil {
					t.Fatal(err)
				}
			case "absent":
				path = filepath.Join(dir, "absent")
			}
			device, err := StatDevice(path)
			if tt.wantErr {
				pathErr, ok := errors.AsType[*os.PathError](err)
				if !ok || pathErr.Op != "lstat" || pathErr.Path != path || !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("StatDevice = %d, %v; want a path-specific lstat ENOENT", device, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("StatDevice(%q): %v", path, err)
			}
			if diff := gocmp.Diff(uint64(stat.Dev), device); diff != "" {
				t.Fatalf("device (-want +got): %s", diff)
			}
		})
	}
}
