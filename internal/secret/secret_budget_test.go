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
	"errors"
	"math"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/errs"
)

func TestLockedMemoryBudgetCheck(t *testing.T) {
	tests := map[string]struct {
		limit     uint64
		pageSize  int
		wantError string
	}{
		"error: zero budget": {
			pageSize:  16384,
			wantError: "RLIMIT_MEMLOCK soft limit is 0 bytes; secret memory requires at least 1048576 bytes (64 pages); raise it in the launching shell with `ulimit -l 1024` (KiB), or ask the administrator to raise the hard limit, then restart",
		},
		"error: measured exhaustion limit": {
			limit: 262144, pageSize: 16384,
			wantError: "RLIMIT_MEMLOCK soft limit is 262144 bytes; secret memory requires at least 1048576 bytes (64 pages); raise it in the launching shell with `ulimit -l 1024` (KiB), or ask the administrator to raise the hard limit, then restart",
		},
		"error: one byte below threshold": {
			limit: 1048575, pageSize: 16384,
			wantError: "RLIMIT_MEMLOCK soft limit is 1048575 bytes; secret memory requires at least 1048576 bytes (64 pages); raise it in the launching shell with `ulimit -l 1024` (KiB), or ask the administrator to raise the hard limit, then restart",
		},
		"success: exact threshold": {limit: 1048576, pageSize: 16384},
		"success: above threshold": {limit: 1048577, pageSize: 16384},
		"success: unlimited":       {limit: uint64(unix.RLIM_INFINITY), pageSize: 16384},
		"error: small page below threshold": {
			limit: 262143, pageSize: 4096,
			wantError: "RLIMIT_MEMLOCK soft limit is 262143 bytes; secret memory requires at least 262144 bytes (64 pages); raise it in the launching shell with `ulimit -l 256` (KiB), or ask the administrator to raise the hard limit, then restart",
		},
		"success: small page exact threshold": {limit: 262144, pageSize: 4096},
		"error: invalid page size": {
			pageSize:  0,
			wantError: "cannot determine the required locked-memory budget from the system page size",
		},
		"error: negative page size": {
			pageSize:  -1,
			wantError: "cannot determine the required locked-memory budget from the system page size",
		},
		"error: budget overflow": {
			pageSize:  math.MaxInt,
			wantError: "cannot determine the required locked-memory budget from the system page size",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := checkLockedMemoryBudget(tt.limit, tt.pageSize)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("budget unexpectedly refused: %v", err)
				}
				return
			}
			configErr, ok := errors.AsType[*errs.ConfigError](err)
			if !ok {
				t.Fatalf("error = %v (%T), want *errs.ConfigError", err, err)
			}
			if diff := gocmp.Diff(tt.wantError, configErr.Error()); diff != "" {
				t.Errorf("diagnostic (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(errs.ExitFatal, errs.ExitCode(err)); diff != "" {
				t.Errorf("exit status (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEnsureLockedMemoryBudget(t *testing.T) {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &limit); err != nil {
		t.Fatal(err)
	}
	want := checkLockedMemoryBudget(limit.Cur, unix.Getpagesize())
	got := EnsureLockedMemoryBudget()
	if (got == nil) != (want == nil) {
		t.Fatalf("live budget check = %v, want %v", got, want)
	}
	if want != nil {
		if diff := gocmp.Diff(want.Error(), got.Error()); diff != "" {
			t.Errorf("live diagnostic (-want +got):\n%s", diff)
		}
	}
}
