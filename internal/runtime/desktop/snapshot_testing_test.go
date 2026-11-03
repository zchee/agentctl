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

package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestTestingWorldConstruction(t *testing.T) {
	tests := map[string]struct {
		fakeStat bool
	}{
		"success: the real device reader is the default": {},
		"success: a fixture stat program is selected":    {fakeStat: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			table := filepath.Join(root, "processes.json")
			if err := os.WriteFile(table, []byte(`{"records":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(processTableEnv, table)
			t.Setenv(testRootEnv, root)
			if tt.fakeStat {
				bin := filepath.Join(root, "usr", "bin")
				if err := os.MkdirAll(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, "stat"), []byte("#!/bin/sh\nprintf '12345\\n'\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			dataDir := filepath.Join(root, "data")
			world, err := testingWorld(dataDir)
			if err != nil {
				t.Fatalf("constructing an absolute test world: %v", err)
			}
			wantPaths := []string{filepath.Join(root, "usr", "bin", "osascript"), filepath.Join(root, "usr", "bin", "open"), filepath.Join(root, strings.TrimPrefix(AppBundle, "/")), dataDir}
			gotPaths := []string{world.Osascript, world.Open, world.Bundle, world.Observer.DataDir}
			if diff := gocmp.Diff(wantPaths, gotPaths); diff != "" {
				t.Fatalf("test-world paths (-want +got):\n%s", diff)
			}
			if world.Observer.UID != uint32(os.Getuid()) || world.PollInterval != testPollPause || world.QuitPolls != QuitPolls || world.LaunchPolls != LaunchPolls {
				t.Fatalf("test world has wrong uid or poll budgets: %+v", world)
			}
			rows, err := world.Observer.List(t.Context(), world.Observer.UID)
			if err != nil || len(rows) != 0 {
				t.Fatalf("fixture table listing = %+v, %v; want no rows", rows, err)
			}
			wantDevice, err := StatDevice(root)
			if err != nil {
				t.Fatal(err)
			}
			if tt.fakeStat {
				wantDevice = 12345
			}
			device, err := world.DeviceOf(root)
			if err != nil || device != wantDevice {
				t.Fatalf("DeviceOf = %d, %v; want %d", device, err, wantDevice)
			}
		})
	}
}

func TestFakeDeviceExecutesTheStatContract(t *testing.T) {
	tests := map[string]struct {
		body    string
		missing bool
		want    uint64
		wantErr bool
	}{
		"success: stat receives flags and the unmodified path": {
			body: `test "$#" -eq 3 && test "$1" = -f && test "$2" = %d && test "$3" = 'a path with spaces' || exit 9
printf '  18446744073709551615\n'
`, want: ^uint64(0),
		},
		"error: a failed stat program is reported":  {body: "exit 7\n", wantErr: true},
		"error: a missing stat program is reported": {missing: true, wantErr: true},
		"error: malformed output is rejected":       {body: "printf 'not-a-device\\n'\n", wantErr: true},
		"error: overflowing output is rejected":     {body: "printf '18446744073709551616\\n'\n", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			stat := filepath.Join(t.TempDir(), "stat")
			if !tt.missing {
				if err := os.WriteFile(stat, []byte("#!/bin/sh\n"+tt.body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			device, err := fakeDevice(stat, "a path with spaces")
			if (err != nil) != tt.wantErr {
				t.Fatalf("fakeDevice error = %v, want error %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if diff := gocmp.Diff(tt.want, device); diff != "" {
					t.Fatalf("device (-want +got): %s", diff)
				}
			}
		})
	}
}

func TestTableLister(t *testing.T) {
	t.Parallel()

	const start = `"start":"2026-10-11T10:00:00Z"`
	tests := map[string]struct {
		// body is the table file; absent when empty.
		body string
		// listings is how many times to list; the last result is checked.
		listings int
		want     []int32
		wantErr  bool
	}{
		"success: other users' paths are dropped and rows keep their parents": {
			body:     `{"records":[{"pid":100,"ppid":1,"uid":501,` + start + `,"path":"` + MainPath + `"},{"pid":200,"ppid":1,"uid":502,` + start + `,"path":"/x"}]}`,
			listings: 1, want: []int32{100, 200},
		},
		"success: a row hidden until a later read appears exactly then": {
			body:     `{"records":[{"pid":100,"ppid":1,"uid":501,` + start + `,"path":"` + MainPath + `","appear_at_read":3}]}`,
			listings: 3, want: []int32{100},
		},
		"success: a row hidden until a later read is absent before it": {
			body:     `{"records":[{"pid":100,"ppid":1,"uid":501,` + start + `,"path":"` + MainPath + `","appear_at_read":3}]}`,
			listings: 2, want: []int32{},
		},
		"success: a process gone before its path read is skipped": {
			body:     `{"records":[{"pid":100,"ppid":1,"uid":501,` + start + `,"path":"` + MainPath + `","gone":true}]}`,
			listings: 1, want: []int32{},
		},
		"error: an absent table fails like the kernel would": {
			listings: 1, wantErr: true,
		},
		"error: a record without a parent pid is malformed": {
			body:     `{"records":[{"pid":100,"uid":501,` + start + `,"path":"` + MainPath + `"}]}`,
			listings: 1, wantErr: true,
		},
		"error: a record of this user with an empty argument block": {
			body:     `{"records":[{"pid":100,"ppid":1,"uid":501,` + start + `}]}`,
			listings: 1, wantErr: true,
		},
		"error: a path read that fails with another error": {
			body:     `{"records":[{"pid":100,"ppid":1,"uid":501,` + start + `,"path":"/x","read_error":"EIO"}]}`,
			listings: 1, wantErr: true,
		},
		"error: an unknown member": {
			body:     `{"records":[{"pid":100,"ppid":1,"uid":501,` + start + `,"path":"/x","colour":"red"}]}`,
			listings: 1, wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "table.json")
			if tt.body != "" {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			list := TableLister(path)
			var rows []Record
			var err error
			for range tt.listings {
				rows, err = list(t.Context(), 501)
			}
			t.Logf("rows %+v err %v", rows, err)
			if tt.wantErr {
				if err == nil {
					t.Fatal("the listing succeeded; want a fail-closed error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			pids := []int32{}
			for _, r := range rows {
				pids = append(pids, r.PID)
				if r.UID != 501 && r.Path != "" {
					t.Fatalf("pid %d of another user kept its path", r.PID)
				}
			}
			if diff := gocmp.Diff(tt.want, pids); diff != "" {
				t.Fatalf("pids (-want +got):\n%s", diff)
			}
		})
	}
}
