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
	"fmt"
	"math"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/errs"
)

const (
	// Reserve eight simultaneous token-sized buffers, double that count for
	// overlap, and allow four pages per buffer. A constrained 16-page limit
	// held only four buffers, so counting just each plaintext page is unsafe.
	lockedMemoryBudgetPages = 8 * 2 * 4
)

// EnsureLockedMemoryBudget rejects a soft RLIMIT_MEMLOCK below the startup
// budget, before any secret is sealed or opened. Unlimited limits pass.
// A low limit returns an errs.ConfigError (fatal exit status 1), because a
// memguard allocation failure can deadlock while purging rather than return
// an error. Failure to read the limit returns an errs.IOError.
// This check does not raise limits or reserve memory; callers must still
// bound concurrent opens and account for payloads larger than one page.
func EnsureLockedMemoryBudget() error {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &limit); err != nil {
		return errs.NewIO("could not read RLIMIT_MEMLOCK for secret memory", err)
	}
	return checkLockedMemoryBudget(limit.Cur, unix.Getpagesize())
}

func checkLockedMemoryBudget(softLimit uint64, pageSize int) error {
	if softLimit == uint64(unix.RLIM_INFINITY) {
		return nil
	}
	if pageSize <= 0 || uint64(pageSize) > math.MaxUint64/lockedMemoryBudgetPages {
		return errs.NewConfig("cannot determine the required locked-memory budget from the system page size")
	}
	required := uint64(pageSize) * lockedMemoryBudgetPages
	if softLimit < required {
		return errs.NewConfig(fmt.Sprintf("RLIMIT_MEMLOCK soft limit is %d bytes; secret memory requires at least %d bytes (%d pages); raise it in the launching shell with `ulimit -l %d` (KiB), or ask the administrator to raise the hard limit, then restart", softLimit, required, lockedMemoryBudgetPages, (required-1)/1024+1))
	}
	return nil
}
