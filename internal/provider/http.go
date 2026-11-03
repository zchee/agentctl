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

package provider

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter reads a Retry-After header value.
//
// HTTP allows both a delay in seconds and an HTTP-date. A date already in the
// past yields a zero duration — "you may retry now" — rather than false,
// because the server did answer the question.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0, false
	}

	if seconds, err := strconv.ParseUint(text, 10, 64); err == nil {
		// A delay the duration type cannot hold is clamped rather than
		// dropped: the server's answer was "a very long time", and the
		// clamp still says that.
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}

	at, err := http.ParseTime(text)
	if err != nil {
		return 0, false
	}
	if remaining := at.Sub(now); remaining > 0 {
		return remaining, true
	}
	return 0, true
}
