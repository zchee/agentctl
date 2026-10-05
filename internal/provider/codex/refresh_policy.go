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
	"strings"
	"sync/atomic"
	"time"
)

const (
	// RefreshPostBudget is the sum of the six independent POST phase bounds.
	RefreshPostBudget = TimeoutResolve + TimeoutConnect + TimeoutSendRequest + TimeoutSendBody + TimeoutRecvResponse + TimeoutRecvBody
	// RefreshWriteAllowance reserves time to persist the server's answer.
	RefreshWriteAllowance = time.Second
	// PassLockBudget is the namespace-lock wait used by a status pass.
	PassLockBudget = time.Second
	// ResendWait is the shortest interval before a confirmed unknown re-send.
	ResendWait = time.Hour
	// TerminalDidNotHelp is the count that stops unauthorized-driven refreshes.
	TerminalDidNotHelp uint8 = 3
	// MaxRefreshRetryAfter bounds server-directed re-send delays.
	MaxRefreshRetryAfter = 24 * time.Hour
	// MaxEarliestRefreshAhead bounds a server's floor on refreshing its grant.
	MaxEarliestRefreshAhead = 30 * 24 * time.Hour
)

var maxRefreshTimestamp = time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)

// ErrRefreshNotATerminal refuses even --yes when stdin is not interactive.
var ErrRefreshNotATerminal = errors.New("this needs an interactive terminal; `--yes` is refused when stdin is not one")

// ErrRefreshNotConfirmed means an interactive answer was not exactly yes.
var ErrRefreshNotConfirmed = errors.New("not confirmed")

func confirmedRefresh(answer string, stdinIsTTY, yesFlag bool) error {
	if !stdinIsTTY {
		return ErrRefreshNotATerminal
	}
	answer = strings.TrimSpace(answer)
	if yesFlag || len(answer) == 3 && (answer[0] == 'y' || answer[0] == 'Y') && (answer[1] == 'e' || answer[1] == 'E') && (answer[2] == 's' || answer[2] == 'S') {
		return nil
	}
	return ErrRefreshNotConfirmed
}

// ResendConsent authorizes one interactive re-send; its zero value is invalid.
type ResendConsent struct {
	state *refreshConsentState
}

type refreshConsentState struct {
	used atomic.Bool
}

// NewResendConsent accepts only a terminal and either --yes or a yes answer.
// It returns ErrRefreshNotATerminal or ErrRefreshNotConfirmed on refusal.
func NewResendConsent(answer string, stdinIsTTY, yesFlag bool) (*ResendConsent, error) {
	if err := confirmedRefresh(answer, stdinIsTTY, yesFlag); err != nil {
		return nil, err
	}
	return &ResendConsent{state: new(refreshConsentState)}, nil
}

func (c *ResendConsent) consume() bool {
	return c != nil && c.state != nil && !c.state.used.Swap(true)
}

// ResetConsent authorizes one interactive floor reset; its zero value is invalid.
type ResetConsent struct {
	state *refreshConsentState
}

// NewResetConsent applies the same terminal and answer rules as NewResendConsent.
// It returns ErrRefreshNotATerminal or ErrRefreshNotConfirmed on refusal.
func NewResetConsent(answer string, stdinIsTTY, yesFlag bool) (*ResetConsent, error) {
	if err := confirmedRefresh(answer, stdinIsTTY, yesFlag); err != nil {
		return nil, err
	}
	return &ResetConsent{state: new(refreshConsentState)}, nil
}

func (c *ResetConsent) consume() bool {
	return c != nil && c.state != nil && !c.state.used.Swap(true)
}

// ResendEligibleAt returns the earliest confirmed re-send instant.
// RetryAfter matters only for rate-limited outcomes and is clamped between
// one hour and one day. Arithmetic saturates at the largest RFC 3339 instant.
func ResendEligibleAt(since time.Time, class RefreshUnknownClass, retryAfter *uint64) time.Time {
	wait := ResendWait
	if class == RefreshUnknownRateLimited && retryAfter != nil {
		seconds := max(uint64(ResendWait/time.Second), min(*retryAfter, uint64(MaxRefreshRetryAfter/time.Second)))
		wait = time.Duration(seconds) * time.Second
	}
	at := since.Add(wait)
	if at.After(maxRefreshTimestamp) {
		return maxRefreshTimestamp
	}
	return at
}

func refreshFloorUntil(state *RefreshState) *time.Time {
	if state.LastSentAt == nil {
		return nil
	}
	at := time.Unix(state.LastSentAt.Unix()+int64(state.FloorMin)*60, int64(state.LastSentAt.Nanosecond())).UTC()
	if at.After(maxRefreshTimestamp) {
		at = maxRefreshTimestamp
	}
	return new(at)
}

func clampEarliestRefresh(at *time.Time, now time.Time) *time.Time {
	if at == nil || !at.After(now) {
		return nil
	}
	limit := now.Add(MaxEarliestRefreshAhead)
	if limit.After(maxRefreshTimestamp) {
		limit = maxRefreshTimestamp
	}
	if at.After(limit) {
		return new(limit)
	}
	return new(*at)
}
