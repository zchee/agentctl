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

package codex

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRefreshConsentsRequireInteractiveConfirmation(t *testing.T) {
	tests := map[string]struct {
		answer   string
		tty, yes bool
		want     error
	}{
		"success: explicit answer":     {answer: "yes", tty: true},
		"success: case and whitespace": {answer: " \tYeS\n", tty: true},
		"success: terminal yes flag":   {tty: true, yes: true},
		"error: piped yes flag":        {answer: "yes", yes: true, want: ErrRefreshNotATerminal},
		"error: piped answer":          {answer: "yes", want: ErrRefreshNotATerminal},
		"error: empty answer":          {tty: true, want: ErrRefreshNotConfirmed},
		"error: abbreviated answer":    {answer: "y", tty: true, want: ErrRefreshNotConfirmed},
		"error: unicode folded answer": {answer: "yeſ", tty: true, want: ErrRefreshNotConfirmed},
		"error: rejection":             {answer: "no", tty: true, want: ErrRefreshNotConfirmed},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resend, err := NewResendConsent(test.answer, test.tty, test.yes)
			if !errors.Is(err, test.want) {
				t.Fatalf("resend refusal = %v, want %v", err, test.want)
			}
			reset, err := NewResetConsent(test.answer, test.tty, test.yes)
			if !errors.Is(err, test.want) {
				t.Fatalf("reset refusal = %v, want %v", err, test.want)
			}
			if test.want == nil {
				if resend == nil || resend.state == nil || resend.state.used.Load() || reset == nil || reset.state == nil || reset.state.used.Load() {
					t.Fatal("accepted consent was not fresh")
				}
			} else if resend != nil || reset != nil {
				t.Fatal("refusal yielded a consent")
			}
		})
	}
	if (ResendConsent{}).state != nil || (ResetConsent{}).state != nil {
		t.Fatal("zero-value consent is valid")
	}
}

func TestRefreshConsentIsOneUseEvenAcrossCallers(t *testing.T) {
	resend, err := NewResendConsent("yes", true, false)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := NewResetConsent("", true, true)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		consume func() bool
		want    int32
	}{
		"success: resend":    {resend.consume, 1},
		"success: reset":     {reset.consume, 1},
		"error: nil resend":  {(*ResendConsent)(nil).consume, 0},
		"error: nil reset":   {(*ResetConsent)(nil).consume, 0},
		"error: zero resend": {new(ResendConsent).consume, 0},
		"error: zero reset":  {new(ResetConsent).consume, 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var accepted atomic.Int32
			var group sync.WaitGroup
			for range 32 {
				group.Go(func() {
					if test.consume() {
						accepted.Add(1)
					}
				})
			}
			group.Wait()
			if diff := gocmp.Diff(test.want, accepted.Load()); diff != "" {
				t.Fatal(diff)
			}
			if test.consume() {
				t.Fatal("consent could be consumed again")
			}
		})
	}
}

func TestRefreshConsentCopiesCannotDuplicateAuthority(t *testing.T) {
	resend, err := NewResendConsent("yes", true, false)
	if err != nil {
		t.Fatal(err)
	}
	resendCopy := *resend
	reset, err := NewResetConsent("yes", true, false)
	if err != nil {
		t.Fatal(err)
	}
	resetCopy := *reset
	tests := map[string]struct{ original, copied func() bool }{
		"success: copied resend": {resend.consume, resendCopy.consume},
		"success: copied reset":  {reset.consume, resetCopy.consume},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var group sync.WaitGroup
			var accepted atomic.Int32
			for range 32 {
				group.Go(func() {
					if test.original() {
						accepted.Add(1)
					}
				})
				group.Go(func() {
					if test.copied() {
						accepted.Add(1)
					}
				})
			}
			group.Wait()
			if diff := gocmp.Diff(int32(1), accepted.Load()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestRefreshResendEligibility(t *testing.T) {
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		class RefreshUnknownClass
		retry *uint64
		wait  time.Duration
	}{
		"success: ambiguous waits one hour":      {class: RefreshUnknownAmbiguous, wait: time.Hour},
		"success: interrupted waits one hour":    {class: RefreshUnknownInterrupted, wait: time.Hour},
		"success: server error ignores retry":    {class: RefreshUnknownServerError, retry: new(uint64(7200)), wait: time.Hour},
		"success: no rate limit hint":            {class: RefreshUnknownRateLimited, wait: time.Hour},
		"success: short hint has one hour floor": {class: RefreshUnknownRateLimited, retry: new(uint64(1)), wait: time.Hour},
		"success: long hint":                     {class: RefreshUnknownRateLimited, retry: new(uint64(7200)), wait: 2 * time.Hour},
		"success: hint capped to one day":        {class: RefreshUnknownRateLimited, retry: new(uint64(math.MaxUint64)), wait: 24 * time.Hour},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(since.Add(test.wait), ResendEligibleAt(since, test.class, test.retry)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
	if got := ResendEligibleAt(maxRefreshTimestamp.Add(-time.Minute), RefreshUnknownRateLimited, new(uint64(math.MaxUint64))); !got.Equal(maxRefreshTimestamp) {
		t.Fatalf("timestamp did not saturate: %v", got)
	}
}

func TestRefreshFloorsAndServerTimeClamp(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 123, time.UTC)
	tests := map[string]struct {
		at   *time.Time
		want *time.Time
	}{
		"success: absent":                    {},
		"success: past is ignored":           {at: new(now.Add(-time.Second))},
		"success: present is ignored":        {at: new(now)},
		"success: future is retained":        {at: new(now.Add(time.Hour)), want: new(now.Add(time.Hour))},
		"success: distant future is bounded": {at: new(now.Add(365 * 24 * time.Hour)), want: new(now.Add(30 * 24 * time.Hour))},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, clampEarliestRefresh(test.at, now)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
	if got := refreshFloorUntil(&RefreshState{}); got != nil {
		t.Fatalf("absent last send has a floor: %v", got)
	}
	state := RefreshState{LastSentAt: new(now), FloorMin: 120}
	if diff := gocmp.Diff(new(now.Add(2*time.Hour)), refreshFloorUntil(&state)); diff != "" {
		t.Fatal(diff)
	}
	state.FloorMin = 1_000_000_000
	want := time.Unix(now.Unix()+1_000_000_000*60, int64(now.Nanosecond())).UTC()
	if diff := gocmp.Diff(new(want), refreshFloorUntil(&state)); diff != "" {
		t.Fatal(diff)
	}
	state.FloorMin = math.MaxUint32
	if got := refreshFloorUntil(&state); got == nil || !got.Equal(maxRefreshTimestamp) {
		t.Fatalf("maximum floor did not saturate: %v", got)
	}
	state.LastSentAt = new(maxRefreshTimestamp)
	if got := refreshFloorUntil(&state); got == nil || !got.Equal(maxRefreshTimestamp) {
		t.Fatalf("large floor did not saturate: %v", got)
	}
}

func TestRefreshClassLabelsAreFixed(t *testing.T) {
	tests := map[string]struct {
		class RefreshUnknownClass
		want  string
	}{
		"success: ambiguous":   {RefreshUnknownAmbiguous, "ambiguous"},
		"success: server":      {RefreshUnknownServerError, "server_error"},
		"success: rate":        {RefreshUnknownRateLimited, "rate_limited"},
		"success: interrupted": {RefreshUnknownInterrupted, "interrupted"},
		"success: TLS":         {RefreshUnknownTLS, "tls"},
		"success: write":       {RefreshUnknownWriteFailed, "write_failed"},
		"error: unknown":       {RefreshUnknownClass("planted-private-value"), ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, test.class.Label()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
	if RefreshPostBudget != 19*time.Second {
		t.Fatalf("POST phase-sum budget = %v", RefreshPostBudget)
	}
}
