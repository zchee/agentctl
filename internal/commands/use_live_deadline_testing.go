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

package commands

import (
	"math"
	"os"
	"strconv"
	"time"
)

func useSwapDuration() time.Duration {
	milliseconds, err := strconv.ParseInt(os.Getenv("AGENTCTL_SWAP_DEADLINE_MS"), 10, 64)
	if err != nil || milliseconds < 0 || milliseconds > math.MaxInt64/int64(time.Millisecond) {
		return useSwapDeadline
	}
	return time.Duration(milliseconds) * time.Millisecond
}
