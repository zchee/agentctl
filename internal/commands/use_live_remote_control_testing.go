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
	"os"
	"strconv"
	"time"
)

// useRemoteControlTiming shortens the follow-up's long waits in a tagged
// build: the variable names how many milliseconds stand for one second of
// the drop wait, the reconnect lifetime and the follow-up budget. The
// preflight and status waits stay real, because the refusal they decide
// is what the tests assert.
func useRemoteControlTiming() useRCTiming {
	timing := useRCDefaultTiming()
	scale, err := strconv.ParseInt(os.Getenv("AGENTCTL_REMOTE_CONTROL_TIME_SCALE"), 10, 64)
	if err != nil || scale <= 0 || scale >= 1000 {
		return timing
	}
	shorten := func(d time.Duration) time.Duration { return d * time.Duration(scale) / 1000 }
	timing.drop, timing.reconnect, timing.followUp = shorten(timing.drop), shorten(timing.reconnect), shorten(timing.followUp)
	timing.poll = min(timing.poll, max(timing.drop/20, 10*time.Millisecond))
	return timing
}
