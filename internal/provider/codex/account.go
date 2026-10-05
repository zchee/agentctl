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
	"fmt"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/usage"
)

// StateKind is the stable machine-readable account state.
type StateKind string

const (
	// StateOK means usage was read.
	StateOK StateKind = "ok"
	// StateExpired means the grant has expired.
	StateExpired StateKind = "expired"
	// StateNeedsLogin means no usable grant exists.
	StateNeedsLogin StateKind = "needs_login"
	// StateNoUsageSource is an exit-neutral credential mode without ChatGPT usage.
	StateNoUsageSource StateKind = "no_usage_source"
	// StateNoUsageWindows means the response had no rate limit.
	StateNoUsageWindows StateKind = "no_usage_windows"
	// StateStoreUnsupported means credentials are not read from this store.
	StateStoreUnsupported StateKind = "store_mode_unsupported"
	// StateHomeUnreadable means the home could not be resolved or read.
	StateHomeUnreadable StateKind = "home_unreadable"
	// StateTornRead means the file was being rewritten.
	StateTornRead StateKind = "torn_read"
	// StateUnauthorized means the endpoint rejected the access token.
	StateUnauthorized StateKind = "unauthorized"
	// StateStaleSibling means an imported live sibling has a different grant.
	StateStaleSibling StateKind = "stale_sibling_of_live"
	// StateForgotten means the account is hidden by the user.
	StateForgotten StateKind = "forgotten"
	// StateSessionDetected means a Codex session uses this namespace.
	StateSessionDetected StateKind = "codex_session_detected"
	// StateBusy means another process holds the namespace lock.
	StateBusy StateKind = "busy"
	// StateLockUnavailable means the lock cannot be taken.
	StateLockUnavailable StateKind = "lock_unavailable"
	// StateStale means the figures are cached.
	StateStale StateKind = "stale"
	// StateRateLimited means the endpoint asked for a pause.
	StateRateLimited StateKind = "rate_limited"
	// StateRefreshDiscarded means a refresh response was discarded.
	StateRefreshDiscarded StateKind = "refresh_discarded"
	// StateIdentityDrift means the renewed token names a different workspace.
	StateIdentityDrift StateKind = "identity_drift"
	// StateRefreshUnknown means a sent refresh has no known outcome.
	StateRefreshUnknown StateKind = "refresh_outcome_unknown"
	// StateRefreshUnavailable means the marker cannot be read or written.
	StateRefreshUnavailable StateKind = "refresh_state_unavailable"
	// StateRefreshDisabled means policy forbids automatic renewal.
	StateRefreshDisabled StateKind = "refresh_disabled"
	// StateUnauthorizedFloor means a rejected token is within the refresh floor.
	StateUnauthorizedFloor StateKind = "unauthorized_floor"
	// StateUnauthorizedTerminal means repeated renewal did not help.
	StateUnauthorizedTerminal StateKind = "unauthorized_terminal"
	// StateAdoptedDead means an adopted grant was rejected.
	StateAdoptedDead StateKind = "adopted_grant_dead"
	// StateDiscardedExternal means an external grant replaced the refresh response.
	StateDiscardedExternal StateKind = "discarded_external"
	// StateError is a content-free error sentence.
	StateError StateKind = "error"
)

// State holds only derived, non-secret account status information.
type State struct {
	Kind              StateKind
	Reason            string
	Mode              string
	Evidence          string
	RefreshedRecently bool
	RetryAfter        *uint64
	Since             time.Time
	Class             string
	ResendEligible    bool
}

// Name returns the stable state token.
func (s State) Name() string { return string(s.Kind) }

// IsExitNeutral reports states that leave the exit status untouched.
func (s State) IsExitNeutral() bool {
	return s.Kind == StateOK || s.Kind == StateNoUsageSource || s.Kind == StateForgotten
}

// Label returns the display sentence, not a machine-readable discriminator.
func (s State) Label() string {
	switch s.Kind {
	case StateOK:
		return "ok"
	case StateExpired:
		return "expired (" + s.Reason + ")"
	case StateNeedsLogin:
		return "needs login"
	case StateNoUsageSource:
		return "no usage source (" + s.Mode + ")"
	case StateNoUsageWindows:
		return "no usage windows"
	case StateStoreUnsupported:
		return "not read (credential store: " + s.Mode + ")"
	case StateHomeUnreadable, StateError:
		return s.Reason
	case StateTornRead:
		return ShownName() + " was being rewritten; retrying next pass"
	case StateUnauthorized:
		return "unauthorized"
	case StateStaleSibling:
		return "stale sibling of live"
	case StateForgotten:
		return "forgotten"
	case StateSessionDetected:
		return "codex session detected (" + s.Evidence + ")"
	case StateBusy:
		return "busy"
	case StateLockUnavailable:
		return "lock unavailable"
	case StateStale:
		return "stale"
	case StateRateLimited:
		if s.RetryAfter != nil {
			return fmt.Sprintf("rate-limited (retry in %ds)", *s.RetryAfter)
		}
		return "rate-limited"
	case StateRefreshDiscarded:
		return "refresh discarded"
	case StateIdentityDrift:
		return "identity drift"
	case StateRefreshUnknown:
		return "refresh outcome unknown (" + s.Class + ")"
	case StateRefreshUnavailable:
		return "refresh state unavailable: " + s.Reason
	case StateRefreshDisabled:
		return "refresh disabled"
	case StateUnauthorizedFloor:
		return "unauthorized (refresh floor)"
	case StateUnauthorizedTerminal:
		return "unauthorized (refresh did not help)"
	case StateAdoptedDead:
		return "needs login (adopted grant dead)"
	case StateDiscardedExternal:
		return "refresh discarded (external writer)"
	default:
		return s.Reason
	}
}

// Badge returns a watch badge when the state earns one.
func Badge(state State) string {
	switch state.Kind {
	case StateStale, StateTornRead:
		return "stale"
	case StateRateLimited:
		return "rate-limited"
	case StateSessionDetected:
		return "codex-detected"
	case StateBusy:
		return "busy"
	case StateExpired:
		return "expired"
	case StateNeedsLogin, StateAdoptedDead:
		return "needs login"
	case StateRefreshUnknown, StateRefreshUnavailable:
		return "refresh unknown"
	default:
		return ""
	}
}

// RowKind names the credential's location without carrying a writable path.
type RowKind string

const (
	// RowLive is the user's live home.
	RowLive RowKind = "live"
	// RowHomeReadOnly is an imported read-only home.
	RowHomeReadOnly RowKind = "home_read_only"
	// RowOwned is an owned namespace.
	RowOwned RowKind = "owned"
)

// Name returns the registry and display spelling.
func (k RowKind) Name() string { return string(k) }

// Account is one finished pass row; credentials can never reach its renderers.
type Account struct {
	Index     int
	ID        string
	UserID    string
	AccountID string
	Email     *string
	Plan      *string
	Kind      RowKind
	State     State
	LockState string
	Note      *string
	Usage     *Usage
	Visible   bool
}

// TableRow is a renderer-neutral table projection.
type TableRow struct {
	Account          string
	Plan             string
	Kind             string
	Session          *usage.LimitWindow
	Weekly           *usage.LimitWindow
	Extra            []usage.LimitWindow
	Credits          string
	State            string
	VisibleByDefault bool
}

// Gauge is a renderer-neutral floored utilization percentage.
type Gauge struct {
	Label   string
	Percent int
}

// Account returns the email when nonempty, otherwise the display id.
func (a Account) Account() string {
	if a.Email != nil && *a.Email != "" {
		return *a.Email
	}
	return a.ID
}

// StateCell appends a nonempty note to the display state.
func (a Account) StateCell() string {
	if a.Note != nil && *a.Note != "" {
		return a.State.Label() + " (" + *a.Note + ")"
	}
	return a.State.Label()
}

// Window returns the first limit window in the requested family.
func (a Account) Window(kind usage.WindowClass) *usage.LimitWindow {
	if a.Usage != nil {
		for i := range a.Usage.Windows {
			if a.Usage.Windows[i].Window.Kind.Class == kind {
				return &a.Usage.Windows[i].Window
			}
		}
	}
	return nil
}

// CreditsCell preserves the wire balance, without inventing a currency.
func (a Account) CreditsCell() string {
	if a.Usage == nil || !a.Usage.Credits.Available {
		return "n/a"
	}
	credits := a.Usage.Credits
	if credits.Unlimited {
		return "Unlimited"
	}
	if credits.Balance != nil {
		return *credits.Balance
	}
	return "—"
}

// ToTableRow spends only the first session and weekly windows on fixed columns.
func (a Account) ToTableRow() TableRow {
	row := TableRow{Account: a.Account(), Kind: a.Kind.Name(), Session: a.Window(usage.WindowSession), Weekly: a.Window(usage.WindowWeeklyAll), Credits: a.CreditsCell(), State: a.StateCell(), VisibleByDefault: a.Visible}
	if a.Plan != nil {
		row.Plan = *a.Plan
	}
	session, weekly := false, false
	if a.Usage != nil {
		for _, entry := range a.Usage.Windows {
			window := entry.Window
			switch window.Kind.Class {
			case usage.WindowSession:
				if !session {
					session = true
					continue
				}
			case usage.WindowWeeklyAll:
				if !weekly {
					weekly = true
					continue
				}
			}
			row.Extra = append(row.Extra, window)
		}
	}
	return row
}

// NextReset returns the earliest known window reset.
func (a Account) NextReset() time.Time {
	var next time.Time
	if a.Usage != nil {
		for _, entry := range a.Usage.Windows {
			at := entry.Window.ResetsAt
			if !at.IsZero() && (next.IsZero() || at.Before(next)) {
				next = at
			}
		}
	}
	return next
}

// Gauges returns only described session and weekly percentages.
func (a Account) Gauges() []Gauge {
	var gauges []Gauge
	for _, entry := range []struct {
		kind  usage.WindowClass
		label string
	}{{usage.WindowSession, "5h"}, {usage.WindowWeeklyAll, "weekly"}} {
		if window := a.Window(entry.kind); window != nil && window.PercentFloor != nil {
			gauges = append(gauges, Gauge{Label: entry.label, Percent: *window.PercentFloor})
		}
	}
	return gauges
}

// DetailLine renders state, note, and the soonest reset using the shared countdown.
func (a Account) DetailLine(now time.Time) string {
	var parts []string
	if badge := Badge(a.State); badge != "" {
		parts = append(parts, "["+badge+"]")
	}
	parts = append(parts, a.State.Label())
	if a.Note != nil && *a.Note != "" {
		parts = append(parts, "("+*a.Note+")")
	}
	if reset := a.NextReset(); !reset.IsZero() {
		parts = append(parts, "next reset in "+usage.RenderCountdown(now, reset))
	}
	return strings.Join(parts, " · ")
}

// AccountTitle renders a selectable block heading.
func (a Account) AccountTitle(selected bool) string {
	marker := " "
	if selected {
		marker = "▸"
	}
	plan := "—"
	if a.Plan != nil && *a.Plan != "" {
		plan = *a.Plan
	}
	return fmt.Sprintf("%s %s · %s · %s ", marker, a.Account(), a.Kind.Name(), plan)
}

// BlockHeight includes the two borders and detail line.
func (a Account) BlockHeight() int { return 3 + len(a.Gauges()) }

// VisibleByDefault reports whether this row is shown without --all.
func (a Account) VisibleByDefault() bool { return a.Visible }

// StateToken returns the neutral state discriminator.
func (a Account) StateToken() string { return a.State.Name() }

// WatchTitle names this provider's display.
func (Account) WatchTitle() string { return "agentctl codex watch" }

// HiddenHint names the command that reveals hidden rows.
func (Account) HiddenHint() string { return "agentctl codex status --all" }
