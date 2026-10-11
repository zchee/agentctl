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
	"context"
	"encoding/binary"
	"errors"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

func argBlock(path string) []byte {
	return append(binary.NativeEndian.AppendUint32(nil, 1), path+"\x00"+path+"\x00"...)
}

func TestRecordsFrom(t *testing.T) {
	t.Parallel()

	listing := []proc.Process{
		{PID: 1, PPID: 0, RealUID: 0, Start: t0},
		{PID: 100, PPID: 1, RealUID: me, Start: t0},
		{PID: 101, PPID: 100, RealUID: me, Start: t0},
		{PID: 102, PPID: 1, RealUID: other, Start: t0},
	}
	paths := map[int32]string{100: MainPath, 101: helper}
	alive := func(context.Context, int) (proc.Process, error) { return proc.Process{}, nil }
	gone := func(context.Context, int) (proc.Process, error) { return proc.Process{}, proc.ErrProcessGone }

	tests := map[string]struct {
		listing []proc.Process
		// failures maps a pid to the errors its reads return in order;
		// after the list is used up the read succeeds.
		failures map[int32][]error
		lookup   func(context.Context, int) (proc.Process, error)
		want     []int32
		// wantReads is the number of reads of pid 101.
		wantReads int
		wantErr   bool
	}{
		"success: other users' paths are never read": {
			listing: listing, lookup: alive, want: []int32{1, 100, 101, 102}, wantReads: 1,
		},
		"success: a process gone between list and read is skipped (ESRCH)": {
			listing: listing, lookup: alive, failures: map[int32][]error{101: {syscall.ESRCH, syscall.ESRCH, syscall.ESRCH, syscall.ESRCH}},
			want: []int32{1, 100, 102}, wantReads: 4,
		},
		"success: EINVAL that persists is the plan's gone case": {
			listing: listing, lookup: alive, failures: map[int32][]error{101: {syscall.EINVAL, syscall.EINVAL, syscall.EINVAL, syscall.EINVAL}},
			want: []int32{1, 100, 102}, wantReads: 4,
		},
		"success: a transient EIO cleared by a retry is read": {
			listing: listing, lookup: alive, failures: map[int32][]error{101: {syscall.EIO}},
			want: []int32{1, 100, 101, 102}, wantReads: 2,
		},
		"success: a persistent EIO on a process that is gone by then is skipped": {
			listing: listing, lookup: gone, failures: map[int32][]error{101: {syscall.EIO, syscall.EIO, syscall.EIO, syscall.EIO}},
			want: []int32{1, 100, 102}, wantReads: 4,
		},
		"success: a zombie of this user is skipped without a read": {
			listing: append(append([]proc.Process(nil), listing...), proc.Process{PID: 103, PPID: 1, RealUID: me, Start: t0, Holder: proc.HolderDead}),
			lookup:  alive, want: []int32{1, 100, 101, 102}, wantReads: 1,
		},
		"error: a persistent EIO on a live process fails closed": {
			listing: listing, lookup: alive, failures: map[int32][]error{101: {syscall.EIO, syscall.EIO, syscall.EIO, syscall.EIO}},
			wantErr: true, wantReads: 4,
		},
		"error: EPERM on a live process fails closed": {
			listing: listing, lookup: alive, failures: map[int32][]error{101: {syscall.EPERM, syscall.EPERM, syscall.EPERM, syscall.EPERM}},
			wantErr: true, wantReads: 4,
		},
		"error: an empty argument block fails closed": {
			listing: listing, lookup: alive, failures: map[int32][]error{101: {errEmptyBlock}},
			wantErr: true, wantReads: 1,
		},
		"error: a row of this user without a plausible start fails closed": {
			listing: []proc.Process{{PID: 100, PPID: 1, RealUID: me}},
			lookup:  alive, wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reads := map[int32]int{}
			read := func(pid int32) ([]byte, error) {
				n := reads[pid]
				reads[pid]++
				if errs := tt.failures[pid]; n < len(errs) {
					if errors.Is(errs[n], errEmptyBlock) {
						return binary.NativeEndian.AppendUint32(nil, 0), nil
					}
					return nil, errs[n]
				}
				return argBlock(paths[pid]), nil
			}
			rows, err := recordsFrom(t.Context(), tt.listing, me, read, tt.lookup)
			t.Logf("rows %+v err %v reads %v", rows, err, reads)
			if reads[102] != 0 {
				t.Fatal("another user's path was read")
			}
			if tt.wantReads != 0 && reads[101] != tt.wantReads {
				t.Fatalf("pid 101 read %d times, want %d", reads[101], tt.wantReads)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("recordsFrom succeeded; want a fail-closed error")
				}
				return
			}
			if err != nil {
				t.Fatalf("recordsFrom: %v", err)
			}
			var pids []int32
			for _, r := range rows {
				pids = append(pids, r.PID)
				if r.UID != me && r.Path != "" {
					t.Fatalf("row %d of another user carries a path", r.PID)
				}
			}
			if diff := gocmp.Diff(tt.want, pids); diff != "" {
				t.Fatalf("pids (-want +got):\n%s", diff)
			}
		})
	}
}

var errEmptyBlock = errors.New("empty block")

// TestSystemListerReadsThisMac lists the real process table once and only
// checks that every row of this user carries a path; it prints counts, never
// a path or an argument.
func TestSystemListerReadsThisMac(t *testing.T) {
	t.Parallel()
	uid := uint32(syscall.Getuid())
	rows, err := SystemLister(t.Context(), uid)
	if err != nil {
		t.Fatalf("SystemLister: %v", err)
	}
	mine := 0
	for _, r := range rows {
		if r.UID == uid {
			mine++
			if r.Path == "" || r.Start.IsZero() {
				t.Fatalf("pid %d of this user lacks a path or a start", r.PID)
			}
		}
	}
	t.Logf("%d rows, %d of this user", len(rows), mine)
	if mine == 0 {
		t.Fatal("not even this test process is in the table")
	}
}
