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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

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
