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
	"errors"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestParseDuration(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input   string
		want    time.Duration
		wantErr *DurationParseError
	}{
		"success: bare seconds": {
			input: "300",
			want:  300 * time.Second,
		},
		"success: explicit seconds": {
			input: "10s",
			want:  10 * time.Second,
		},
		"success: minutes": {
			input: "5m",
			want:  300 * time.Second,
		},
		"success: hours": {
			input: "2h",
			want:  7200 * time.Second,
		},
		"success: zero": {
			input: "0",
			want:  0,
		},
		"success: surrounding whitespace is trimmed": {
			input: "  45s  ",
			want:  45 * time.Second,
		},
		"error: empty": {
			input:   "",
			wantErr: &DurationParseError{Kind: DurationEmpty, Input: ""},
		},
		"error: whitespace only": {
			input:   "   ",
			wantErr: &DurationParseError{Kind: DurationEmpty, Input: ""},
		},
		"error: no leading digits": {
			input:   "s",
			wantErr: &DurationParseError{Kind: DurationNoDigits, Input: "s"},
		},
		"error: unit only": {
			input:   "abc",
			wantErr: &DurationParseError{Kind: DurationNoDigits, Input: "abc"},
		},
		"error: value above the unsigned 64-bit range": {
			input:   "99999999999999999999s",
			wantErr: &DurationParseError{Kind: DurationOverflow, Input: "99999999999999999999s"},
		},
		"error: minutes multiplication overflows": {
			input:   "999999999999999999m",
			wantErr: &DurationParseError{Kind: DurationOverflow, Input: "999999999999999999m"},
		},
		"error: hours multiplication overflows": {
			input:   "99999999999999999h",
			wantErr: &DurationParseError{Kind: DurationOverflow, Input: "99999999999999999h"},
		},
		"error: seconds beyond the time.Duration range overflow": {
			input:   "99999999999999999999",
			wantErr: &DurationParseError{Kind: DurationOverflow, Input: "99999999999999999999"},
		},
		"error: unknown unit": {
			input:   "10d",
			wantErr: &DurationParseError{Kind: DurationUnknownUnit, Input: "10d", Unit: "d"},
		},
		"error: sub-second units are not supported": {
			input:   "10ms",
			wantErr: &DurationParseError{Kind: DurationUnknownUnit, Input: "10ms", Unit: "ms"},
		},
		"error: multi-unit forms are not supported": {
			input:   "2h30m",
			wantErr: &DurationParseError{Kind: DurationUnknownUnit, Input: "2h30m", Unit: "h30m"},
		},
		"error: fractional is not supported": {
			input:   "1.5s",
			wantErr: &DurationParseError{Kind: DurationUnknownUnit, Input: "1.5s", Unit: ".5s"},
		},
		"error: negative is not supported": {
			input:   "-5s",
			wantErr: &DurationParseError{Kind: DurationNoDigits, Input: "-5s"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseDuration(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %v, want error %v", tt.input, got, tt.wantErr)
				}
				if diff := gocmp.Diff(tt.wantErr, err); diff != "" {
					t.Fatalf("ParseDuration(%q) error mismatch (-want +got):\n%s", tt.input, diff)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q): unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseDurationOverflowMessageSaysWhy(t *testing.T) {
	t.Parallel()

	// Stated separately from the table because the message is part of the
	// contract: a count that cannot be represented must say it is too
	// large, never hand back a plausible-looking short duration.
	_, err := ParseDuration("999999999999999999m")
	if err == nil {
		t.Fatal("a minutes value that overflows a 64-bit second count must be an error")
	}
	parseErr, ok := errors.AsType[*DurationParseError](err)
	if !ok || parseErr.Kind != DurationOverflow {
		t.Fatalf("expected an overflow error, got %#v", err)
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("message should say why: %v", err)
	}
}

func TestParseWatchInterval(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input     string
		want      time.Duration
		wantFloor bool
	}{
		"success: exactly the floor": {
			input: "60s",
			want:  60 * time.Second,
		},
		"success: bare seconds at the floor": {
			input: "60",
			want:  60 * time.Second,
		},
		"success: one minute spelled as minutes": {
			input: "1m",
			want:  60 * time.Second,
		},
		"success: well above the floor": {
			input: "10m",
			want:  600 * time.Second,
		},
		"error: zero is below the floor": {
			input:     "0",
			wantFloor: true,
		},
		"error: one second is below the floor": {
			input:     "1s",
			wantFloor: true,
		},
		"error: fifty-nine seconds is below the floor": {
			input:     "59s",
			wantFloor: true,
		},
		"error: bare fifty-nine is below the floor": {
			input:     "59",
			wantFloor: true,
		},
		"error: thirty seconds is below the floor": {
			input:     "30s",
			wantFloor: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseWatchInterval(tt.input)
			if tt.wantFloor {
				if err == nil {
					t.Fatalf("ParseWatchInterval(%q) = %v, want a floor rejection", tt.input, got)
				}
				// The message must tell the user what the limit is, not
				// merely that their value was refused.
				if !strings.Contains(err.Error(), "60") {
					t.Fatalf("ParseWatchInterval(%q) must name the 60s floor: %v", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWatchInterval(%q): unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseWatchInterval(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestWatchFloorIsSixtySeconds(t *testing.T) {
	t.Parallel()

	if WatchFloor != 60*time.Second {
		t.Fatalf("WatchFloor = %v, want 60s", WatchFloor)
	}
}

func TestWatchDefaultIsThreeHundredSeconds(t *testing.T) {
	t.Parallel()

	if WatchDefault != 300*time.Second {
		t.Fatalf("WatchDefault = %v, want 300s", WatchDefault)
	}
}

func TestHTTPTimeoutDefaultIsTenSeconds(t *testing.T) {
	t.Parallel()

	if HTTPTimeoutDefault != 10*time.Second {
		t.Fatalf("HTTPTimeoutDefault = %v, want 10s", HTTPTimeoutDefault)
	}
}
