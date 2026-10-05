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

package proc

import (
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestHolderLabel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		holder Holder
		want   string
	}{
		"success: alive prints alive": {
			holder: HolderAlive,
			want:   "alive",
		},
		"success: stopped prints stopped": {
			holder: HolderStopped,
			want:   "stopped",
		},
		"success: dead prints dead": {
			holder: HolderDead,
			want:   "dead",
		},
		"success: an unknown state prints alive because existence was already proven": {
			holder: Holder(42),
			want:   "alive",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := gocmp.Diff(tt.want, tt.holder.Label()); diff != "" {
				t.Errorf("Label() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStartIdentity(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		start time.Time
		want  string
	}{
		"success: microseconds are rendered in full": {
			start: time.Unix(1757400000, 123456000).UTC(),
			want:  "2025-09-09T06:40:00.123456Z",
		},
		"success: trailing zeros in the fraction are trimmed": {
			start: time.Unix(1757400000, 120000000).UTC(),
			want:  "2025-09-09T06:40:00.12Z",
		},
		"success: a whole second renders without a fraction": {
			start: time.Unix(1757400000, 0).UTC(),
			want:  "2025-09-09T06:40:00Z",
		},
		"success: a refused start renders empty so it can never match": {
			start: time.Time{},
			want:  "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p := Process{Start: tt.start}
			if diff := gocmp.Diff(tt.want, p.StartIdentity()); diff != "" {
				t.Errorf("StartIdentity() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStartIdentityDistinguishesOneMicrosecond(t *testing.T) {
	t.Parallel()

	first := Process{Start: time.Unix(1757400000, 123456000).UTC()}
	second := Process{Start: time.Unix(1757400000, 123457000).UTC()}
	if first.StartIdentity() == second.StartIdentity() {
		t.Errorf("one microsecond apart must be two different identities, both rendered %q", first.StartIdentity())
	}
}

func TestUnsupportedPlatformError(t *testing.T) {
	t.Parallel()

	err := &UnsupportedPlatformError{GOOS: "plan9"}
	want := "process observation is not supported on plan9"
	if diff := gocmp.Diff(want, err.Error()); diff != "" {
		t.Errorf("Error() mismatch (-want +got):\n%s", diff)
	}
}
