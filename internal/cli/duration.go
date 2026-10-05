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

package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// WatchFloor is the shortest polling interval `watch` accepts, so the
	// usage API is never polled more often than once every 60 seconds.
	WatchFloor = 60 * time.Second

	// WatchDefault is the polling interval `watch` uses when --interval is
	// not given.
	WatchDefault = 300 * time.Second

	// HTTPTimeoutDefault is the per-request HTTP timeout used when
	// --timeout is not given.
	HTTPTimeoutDefault = 10 * time.Second
)

// DurationParseKind classifies why a duration argument was rejected.
type DurationParseKind int

const (
	// DurationEmpty means the argument was empty or entirely whitespace.
	DurationEmpty DurationParseKind = iota

	// DurationNoDigits means the argument did not start with a digit.
	DurationNoDigits

	// DurationOverflow means the numeric part did not fit, or the unit
	// conversion did not fit.
	DurationOverflow

	// DurationUnknownUnit means the unit suffix is not one the parser
	// knows.
	DurationUnknownUnit
)

// DurationParseError reports why a duration argument could not be parsed.
type DurationParseError struct {
	// Kind classifies the failure.
	Kind DurationParseKind

	// Input is the trimmed argument, for context.
	Input string

	// Unit is the unrecognised suffix when Kind is DurationUnknownUnit.
	Unit string
}

// Error renders the failure with the input it refused, so the message can
// stand alone on stderr.
func (e *DurationParseError) Error() string {
	switch e.Kind {
	case DurationEmpty:
		return "expected a duration such as `10s`, `5m` or `300`, but the value was empty"
	case DurationNoDigits:
		return fmt.Sprintf("expected a duration such as `10s`, `5m` or `300`, but `%s` does not start with a number", e.Input)
	case DurationOverflow:
		return fmt.Sprintf("duration `%s` is too large to represent", e.Input)
	case DurationUnknownUnit:
		return fmt.Sprintf("unknown duration unit `%s` in `%s`; use `s`, `m`, `h`, or no suffix for seconds", e.Unit, e.Input)
	default:
		return fmt.Sprintf("invalid duration `%s`", e.Input)
	}
}

// maxSeconds is the largest whole-second count a time.Duration can hold;
// anything larger is reported as an overflow rather than wrapped.
const maxSeconds = uint64(math.MaxInt64 / int64(time.Second))

// ParseDuration parses a duration written as `10s`, `5m`, `2h`, or a bare
// `300` meaning seconds, with outer whitespace trimmed.
//
// The grammar is a single unsigned integer with at most one unit, so
// fractions (`1.5s`), negatives (`-5s`), sub-second units (`10ms`) and
// multi-unit forms (`2h30m`) are all rejected. All arithmetic is checked: a
// count that does not fit, either as a 64-bit unsigned second count or as a
// time.Duration, is an overflow error rather than a silently wrapped value.
func ParseDuration(input string) (time.Duration, error) {
	text := strings.TrimSpace(input)
	if text == "" {
		return 0, &DurationParseError{Kind: DurationEmpty, Input: text}
	}

	split := strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' })
	if split < 0 {
		split = len(text)
	}
	digits, unit := text[:split], text[split:]
	if digits == "" {
		return 0, &DurationParseError{Kind: DurationNoDigits, Input: text}
	}

	value, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, &DurationParseError{Kind: DurationOverflow, Input: text}
	}

	var seconds uint64
	switch unit {
	case "", "s":
		seconds = value
	case "m":
		if value > math.MaxUint64/60 {
			return 0, &DurationParseError{Kind: DurationOverflow, Input: text}
		}
		seconds = value * 60
	case "h":
		if value > math.MaxUint64/3600 {
			return 0, &DurationParseError{Kind: DurationOverflow, Input: text}
		}
		seconds = value * 3600
	default:
		return 0, &DurationParseError{Kind: DurationUnknownUnit, Input: text, Unit: unit}
	}

	if seconds > maxSeconds {
		return 0, &DurationParseError{Kind: DurationOverflow, Input: text}
	}
	return time.Duration(seconds) * time.Second, nil
}

// ParseWatchInterval parses an interval argument with ParseDuration and
// additionally enforces the WatchFloor minimum, naming the floor in the
// error so the caller learns the limit, not merely that the value was
// refused.
func ParseWatchInterval(input string) (time.Duration, error) {
	interval, err := ParseDuration(input)
	if err != nil {
		return 0, err
	}
	if interval < WatchFloor {
		return 0, fmt.Errorf("interval `%s` is below the 60s floor; agentctl will not poll the usage API more often than once every 60 seconds", input)
	}
	return interval, nil
}
