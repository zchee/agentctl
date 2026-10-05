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

package render

import (
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// Every zone in these tests is injected — a fixed offset, or a named
// entry asserted to agree with one — and never the host zone. A test
// that read the machine's zone would pass in Tokyo and fail in Berlin,
// which is the one thing these assertions must not do.

// plusNine is the zone most of these tests render in: +09:00, fixed and
// DST-free. Fixed rather than named so the assertions do not depend on a
// tzdb entry, and +09:00 specifically because it is far enough east that
// the local date differs from the UTC date for a quarter of the day —
// which is what pins "the weekday follows the local calendar".
var plusNine = time.FixedZone("+09:00", 9*60*60)

// minusSeven is a zone west of UTC, for the same instants seen from the
// other side.
var minusSeven = time.FixedZone("-07:00", -7*60*60)

func ts(tb testing.TB, text string) time.Time {
	tb.Helper()
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		tb.Fatalf("parse test timestamp %q: %v", text, err)
	}
	return parsed
}

// testNow is 10:00 on Wednesday 2026-09-09 in +09:00.
const testNow = "2026-09-09T01:00:00Z"

func TestAbsoluteLocal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		now  string
		at   string
		loc  *time.Location
		want string
	}{
		"success: a reset on the same local day is a bare clock time": {
			// 05:00Z is 14:00 in +09:00, the same local day as now, so the
			// weekday would say nothing the reader does not already know,
			// and the today shape keeps the hour un-padded.
			now: testNow, at: "2026-09-09T05:00:00Z", loc: plusNine,
			want: "2:00 PM",
		},
		"success: another local day carries its weekday and a zero-padded hour": {
			// 2026-09-13 is a Sunday, four days out — inside the week, so
			// the weekday still names exactly one day.
			now: testNow, at: "2026-09-13T05:00:00Z", loc: plusNine,
			want: "Sun 02:00 PM",
		},
		"success: a week or more out carries the date and a zero-padded hour": {
			// Seven days and four hours: "Wed" would name both this
			// Wednesday and that one, so the month and day replace it.
			now: testNow, at: "2026-09-16T05:00:00Z", loc: plusNine,
			want: "Sep 16 02:00 PM",
		},
		"success: 6d23h is still inside the week": {
			now: testNow, at: "2026-09-16T00:00:00Z", loc: plusNine,
			want: "Wed 09:00 AM",
		},
		"success: exactly seven days is past the weekday's reach": {
			now: testNow, at: "2026-09-16T01:00:00Z", loc: plusNine,
			want: "Sep 16 10:00 AM",
		},
		"success: the date shape pads the hour but not the day of month": {
			// "Oct 01" would read as a field rather than as a date, but
			// "9:00 AM" becoming "09:00 AM" is exactly the padding this
			// shape wants.
			now: testNow, at: "2026-10-01T00:00:00Z", loc: plusNine,
			want: "Oct 1 09:00 AM",
		},
		"success: local midnight is twelve AM, not zero": {
			// 15:00Z is 00:00 on Thursday 2026-09-10 in +09:00.
			now: testNow, at: "2026-09-09T15:00:00Z", loc: plusNine,
			want: "Thu 12:00 AM",
		},
		"success: local noon is twelve PM, not zero": {
			// 03:00Z is local noon, the same day — the today shape, so no
			// weekday.
			now: testNow, at: "2026-09-09T03:00:00Z", loc: plusNine,
			want: "12:00 PM",
		},
		"success: the today shape never pads the hour": {
			now: testNow, at: "2026-09-09T05:05:00Z", loc: plusNine,
			want: "2:05 PM",
		},
		"success: the minute is always two digits": {
			now: testNow, at: "2026-09-09T00:09:00Z", loc: plusNine,
			want: "9:09 AM",
		},
		"success: the weekday follows the local date east of UTC": {
			// 23:30 UTC on Saturday 2026-09-12 is 08:30 on Sunday in
			// +09:00. A weekday taken from the instant would print "Sat"
			// and be wrong for every reader east of about UTC+01.
			now: "2026-09-12T01:00:00Z", at: "2026-09-12T23:30:00Z", loc: plusNine,
			want: "Sun 08:30 AM",
		},
		"success: the same instant west of UTC keeps its Saturday": {
			// One instant, two zones, two different weekdays — which is
			// the whole reason the comparison is made on the zoned value.
			now: "2026-09-12T01:00:00Z", at: "2026-09-12T23:30:00Z", loc: minusSeven,
			want: "Sat 04:30 PM",
		},
		"success: the zone decides whether two instants share a day, east": {
			// In +09:00 now is Wednesday morning and the reset is
			// Wednesday afternoon.
			now: testNow, at: "2026-09-09T05:00:00Z", loc: plusNine,
			want: "2:00 PM",
		},
		"success: the zone decides whether two instants share a day, west": {
			// In -07:00 both instants are Tuesday evening, so the shape is
			// still the bare clock time, eight hours apart from the
			// eastern reading.
			now: testNow, at: "2026-09-09T05:00:00Z", loc: minusSeven,
			want: "10:00 PM",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := absoluteLocal(ts(t, tt.now), ts(t, tt.at), tt.loc)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("absoluteLocal(%s, %s) mismatch (-want +got):\n%s", tt.now, tt.at, diff)
			}
		})
	}
}

func TestAbsoluteLocalNamedZoneAgreesWithItsFixedOffset(t *testing.T) {
	t.Parallel()

	// Asia/Tokyo has never observed daylight saving, so the named entry
	// and +09:00 must render identically. If this fails, the zone
	// database is not what it claims and the fixed-offset tests are the
	// ones to trust.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("the platform zone database should know Asia/Tokyo: %v", err)
	}
	now := ts(t, testNow)
	for _, at := range []string{"2026-09-09T05:00:00Z", "2026-09-13T05:00:00Z", "2026-09-16T05:00:00Z"} {
		named := absoluteLocal(now, ts(t, at), tokyo)
		fixed := absoluteLocal(now, ts(t, at), plusNine)
		if named != fixed {
			t.Errorf("%s renders %q in Asia/Tokyo but %q in +09:00", at, named, fixed)
		}
	}
}

func TestCountdown(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		now    string
		target string
		want   string
	}{
		"success: days lead with the remaining hours": {
			now: testNow, target: "2026-09-13T05:00:00Z", want: "4d4h",
		},
		"success: hours lead with the remaining minutes": {
			now: testNow, target: "2026-09-09T03:13:40Z", want: "2h13m",
		},
		"success: bare minutes under an hour": {
			now: testNow, target: "2026-09-09T01:45:00Z", want: "45m",
		},
		"success: bare seconds under a minute": {
			now: testNow, target: "2026-09-09T01:00:45Z", want: "45s",
		},
		"success: a reset that has already passed prints now": {
			now: testNow, target: "2026-09-09T00:30:00Z", want: "now",
		},
		"success: a reset at this very instant prints now": {
			now: testNow, target: testNow, want: "now",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Countdown(ts(t, tt.now), ts(t, tt.target))
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Countdown(%s, %s) mismatch (-want +got):\n%s", tt.now, tt.target, diff)
			}
		})
	}
}

func TestResetCell(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		now  string
		at   string
		want string
	}{
		"success: the countdown then the absolute time in parentheses": {
			now: testNow, at: "2026-09-09T02:12:00Z", want: "1h12m (11:12 AM)",
		},
		"success: a weekday shape inside the week": {
			now: testNow, at: "2026-09-13T05:00:00Z", want: "4d4h (Sun 02:00 PM)",
		},
		"success: a date shape from seven days out": {
			now: testNow, at: "2026-09-16T05:00:00Z", want: "7d4h (Sep 16 02:00 PM)",
		},
		"success: seconds-granularity countdown": {
			now: testNow, at: "2026-09-09T01:00:45Z", want: "45s (10:00 AM)",
		},
		"success: a reset in the past still says when it was": {
			// A window rolling over between the fetch and the render is
			// ordinary, and "now" is the honest countdown for it — but the
			// absolute half still names the moment, so the row is not
			// reduced to a bare "now".
			now: testNow, at: "2026-09-09T00:30:00Z", want: "now (9:30 AM)",
		},
		"success: eight days back is a date, not a weekday": {
			now: testNow, at: "2026-09-01T00:30:00Z", want: "now (Sep 1 09:30 AM)",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ResetCell(ts(t, tt.now), ts(t, tt.at), plusNine)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ResetCell(%s, %s) mismatch (-want +got):\n%s", tt.now, tt.at, diff)
			}
		})
	}
}

func TestResetParts(t *testing.T) {
	t.Parallel()

	countdown, absolute := resetParts(ts(t, testNow), ts(t, "2026-09-09T02:12:00Z"), plusNine)
	if diff := gocmp.Diff("1h12m", countdown); diff != "" {
		t.Errorf("countdown mismatch (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff("11:12 AM", absolute); diff != "" {
		t.Errorf("absolute mismatch (-want +got):\n%s", diff)
	}
}

func TestJustifyReset(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		countdown string
		absolute  string
		width     int
		want      string
	}{
		"success: the countdown flush left and the absolute time flush right": {
			// The width is exactly this row's own natural length, so the
			// gap is the guaranteed single space and nothing more.
			countdown: "1h42m", absolute: "7:09 AM", width: 15,
			want: "1h42m (7:09 AM)",
		},
		"success: never pads by less than one space": {
			// A width narrower than the cell's own natural length still
			// renders, with the guaranteed single space rather than a
			// truncated cell.
			countdown: "19h32m", absolute: "Sat 12:59 AM", width: 10,
			want: "19h32m (Sat 12:59 AM)",
		},
		"success: a now cell is treated like any other row": {
			// A reset that has already passed is still a countdown and an
			// absolute half, so it takes part in a column's width the same
			// way an ordinary row does.
			countdown: "now", absolute: "11:59 PM", width: 15,
			want: "now  (11:59 PM)",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := justifyReset(tt.countdown, tt.absolute, tt.width)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("justifyReset(%q, %q, %d) mismatch (-want +got):\n%s", tt.countdown, tt.absolute, tt.width, diff)
			}
		})
	}
}

func TestJustifyResetLinesUpEveryClosingParen(t *testing.T) {
	t.Parallel()

	// Four rows checked against the same column width the widest one
	// ("19h32m") sets: 6 + 1 + len("(Sat 12:59 AM)") == 21. Every row
	// must reach the column's width and end its closing paren on the
	// same right edge.
	const width = 21
	rows := []struct {
		countdown string
		absolute  string
		want      string
	}{
		{"6d5h", "Thu 10:59 AM", "6d5h   (Thu 10:59 AM)"},
		{"19h32m", "Sat 12:59 AM", "19h32m (Sat 12:59 AM)"},
		{"2d8h", "Sun 01:59 PM", "2d8h   (Sun 01:59 PM)"},
		{"6d23h", "Fri 05:00 AM", "6d23h  (Fri 05:00 AM)"},
	}

	for _, row := range rows {
		cell := justifyReset(row.countdown, row.absolute, width)
		if diff := gocmp.Diff(row.want, cell); diff != "" {
			t.Errorf("justifyReset(%q, %q, %d) mismatch (-want +got):\n%s", row.countdown, row.absolute, width, diff)
		}
		if got := len(cell); got != width {
			t.Errorf("cell %q has width %d, want %d", cell, got, width)
		}
	}
}
