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

package fault

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFromList(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw    string
		active []string
		absent []string
	}{
		"success: names split on commas": {
			raw:    "rename_fail,hold_lock",
			active: []string{"rename_fail", "hold_lock"},
			absent: []string{"security_hang"},
		},
		"success: surrounding whitespace is trimmed": {
			raw:    "rename_fail, hold_lock ",
			active: []string{"rename_fail", "hold_lock"},
		},
		"success: empty entries are dropped": {
			raw:    ",rename_fail,,",
			active: []string{"rename_fail"},
			absent: []string{""},
		},
		"success: the empty list is the empty set": {
			raw:    "",
			absent: []string{"rename_fail", ""},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := FromList(tt.raw)
			for _, want := range tt.active {
				if !f.Is(want) {
					t.Errorf("Is(%q) = false, want true", want)
				}
			}
			for _, wantAbsent := range tt.absent {
				if f.Is(wantAbsent) {
					t.Errorf("Is(%q) = true, want false", wantAbsent)
				}
			}
		})
	}
}

func TestActiveReadsEnvironment(t *testing.T) {
	tests := map[string]struct {
		env    string
		active []string
	}{
		"success: the factory parses the environment list": {
			env:    "rename_fail, hold_lock",
			active: []string{"rename_fail", "hold_lock"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(faultEnv, tt.env)
			f := Active()
			for _, want := range tt.active {
				if !f.Is(want) {
					t.Errorf("Is(%q) = false, want true", want)
				}
			}
		})
	}
}

func TestPausePoint(t *testing.T) {
	tests := map[string]struct {
		faults     string
		resumeAt   time.Duration // when the resume file is written; zero writes it beforehand
		wantMarker bool
		wantHeld   bool
	}{
		"success: an inactive pause returns at once": {
			faults: "pause_other",
		},
		"success: an existing resume file releases the pause immediately": {
			faults:     "pause_before_rename",
			wantMarker: true,
		},
		"success: the pause holds until the resume file appears": {
			faults:     "pause_before_rename",
			resumeAt:   100 * time.Millisecond,
			wantMarker: true,
			wantHeld:   true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resume := filepath.Join(t.TempDir(), "resume")
			t.Setenv(faultEnv, tt.faults)
			t.Setenv(faultResumeEnv, resume)

			switch {
			case tt.resumeAt > 0:
				timer := time.AfterFunc(tt.resumeAt, func() {
					if err := os.WriteFile(resume, nil, 0o600); err != nil {
						t.Errorf("writing the resume file: %v", err)
					}
				})
				defer timer.Stop()
			case tt.wantMarker:
				if err := os.WriteFile(resume, nil, 0o600); err != nil {
					t.Fatalf("writing the resume file: %v", err)
				}
			}

			started := time.Now()
			Active().PausePoint("before_rename")
			elapsed := time.Since(started)

			if tt.wantHeld && elapsed < tt.resumeAt {
				t.Errorf("the pause released after %v, before the resume file existed", elapsed)
			}
			if !tt.wantHeld && elapsed > time.Second {
				t.Errorf("the pause held for %v, want an immediate return", elapsed)
			}
			if _, err := os.Stat(resume + ".reached"); (err == nil) != tt.wantMarker {
				t.Errorf("reached-marker existence = %t, want %t", err == nil, tt.wantMarker)
			}
		})
	}
}

func TestWaitIfWholeName(t *testing.T) {
	tests := map[string]struct{}{
		"success: a whole fault name waits without the pause prefix": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			resume := filepath.Join(t.TempDir(), "resume")
			t.Setenv(faultEnv, "swap_pause_in_locks")
			t.Setenv(faultResumeEnv, resume)

			release := 80 * time.Millisecond
			timer := time.AfterFunc(release, func() {
				if err := os.WriteFile(resume, nil, 0o600); err != nil {
					t.Errorf("writing the resume file: %v", err)
				}
			})
			defer timer.Stop()

			started := time.Now()
			Active().WaitIf("swap_pause_in_locks")
			if elapsed := time.Since(started); elapsed < release {
				t.Errorf("the wait released after %v, before the resume file existed", elapsed)
			}
		})
	}
}

// The pause bounds are behavioural contracts: a crashed test must not
// wedge a run past the budget, and the poll decides how quickly a resume
// file is noticed.
func TestPauseBounds(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		got  time.Duration
		want time.Duration
	}{
		"success: a pause gives up after ten seconds": {
			got:  pauseBudget,
			want: 10 * time.Second,
		},
		"success: a pause re-checks its resume file every twenty milliseconds": {
			got:  pausePollInterval,
			want: 20 * time.Millisecond,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Errorf("bound = %v, want %v", tt.got, tt.want)
			}
		})
	}
}
