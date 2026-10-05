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
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/usage"
)

// StatusRow is the boundary between an account pass and its presentation.
// Everything above it — the keychain, the namespace lock, the HTTP client
// — has finished by the time a row exists, and everything below it is
// presentation. That split is why the table can be tested without
// standing up a pass.
//
// A row carries a rendered state string rather than a state value, so
// that the renderer cannot accidentally make a policy decision — such as
// deciding that some state or other should be hidden. Visibility and the
// exit status are settled before a row is built.

// SameIdentityNote is what the State column appends to a row whose
// account and organization UUIDs are the live credential's.
//
// A note rather than a state, because it is not a problem: two
// independent token pairs for one account are a legitimate setup, and
// both sessions stay valid. What the reader needs is the reason the same
// address appears twice.
const SameIdentityNote = "same identity as live"

// LiveAndOwnedKind is the Kind cell of a row two rows were folded into:
// the live credential first, because that is the one already in use,
// then the store this binary owns.
const LiveAndOwnedKind = "live+owned"

// HeadlineScope is the display name of the scoped weekly window that has
// a column of its own. The renderer owns the pairing between this scope
// and the fixed column heading that names it; every other scoped window
// becomes a continuation row.
const HeadlineScope = "Fable"

// StatusRow is one account, ready to render.
type StatusRow struct {
	// Account is how the account is named in the first column: its email
	// when known, otherwise the id the account flag accepts.
	Account string
	// Org is the organization's display name, or its UUID when unnamed.
	Org string
	// Plan is the subscription tier, as the credential recorded it.
	Plan string
	// State is the state column's text.
	State string
	// Note is a short explanation appended to the state, when there is
	// one.
	Note string
	// Usage holds the numbers, when this row has any.
	Usage *usage.UsageSnapshot
	// VisibleByDefault says whether the row appears without the flag
	// that shows every row.
	VisibleByDefault bool
	// SameIdentityAsLive says whether this row's account and
	// organization pair is the live credential's, which is what puts
	// SameIdentityNote in the state cell.
	SameIdentityAsLive bool
	// Kind is carried on every row but printed only when the identity
	// column is requested, so the default table keeps its fixed columns.
	Kind string
}

// StateCell returns the state column's full text: the state, then its
// notes in parentheses. Two notes are joined with a semicolon rather
// than one replacing the other: a row can be both expired for a stated
// reason and the live account's twin, and dropping either would answer
// half the reader's question.
func (r *StatusRow) StateCell() string {
	notes := make([]string, 0, 2)
	if r.Note != "" {
		notes = append(notes, r.Note)
	}
	if r.SameIdentityAsLive {
		notes = append(notes, SameIdentityNote)
	}
	if len(notes) == 0 {
		return r.State
	}
	return r.State + " (" + strings.Join(notes, "; ") + ")"
}

// Report is a whole pass, ready to render.
type Report struct {
	// Rows holds every row the pass produced, hidden ones included.
	Rows []StatusRow
	// Now is the moment the report was built; every countdown is
	// relative to it, so a table cannot show two cells computed against
	// different clocks.
	Now time.Time
	// Zone is where the reset columns are printed, carried for the same
	// reason Now is: one report, one zone, so two cells cannot disagree
	// about what "Sunday" means. A test injects a fixed one.
	Zone *time.Location
	// ShowAll says whether every row was requested.
	ShowAll bool
	// ByIdentity says whether the identity view was requested, which is
	// the only thing that puts the Kind column on the table. The folding
	// itself has already happened by the time a report exists — which
	// rows survive is a decision about accounts, not about rendering.
	ByIdentity bool
}

// Shown returns the rows that will actually be printed.
func (r *Report) Shown() []*StatusRow {
	var shown []*StatusRow
	for i := range r.Rows {
		if r.ShowAll || r.Rows[i].VisibleByDefault {
			shown = append(shown, &r.Rows[i])
		}
	}
	return shown
}

// HiddenCount returns how many rows the show-everything flag would add.
func (r *Report) HiddenCount() int {
	if r.ShowAll {
		return 0
	}
	hidden := 0
	for i := range r.Rows {
		if !r.Rows[i].VisibleByDefault {
			hidden++
		}
	}
	return hidden
}

// CodexTableRow is one Codex account, ready to render. A separate type
// from StatusRow for the same reason that type exists: the renderer is
// handed finished cells and windows, never a provider state, so it
// cannot decide what to hide. The windows arrive already sorted into the
// two columns and the continuation rows by the pass that built this.
type CodexTableRow struct {
	// Account is the first column: the email when known, else the
	// display id.
	Account string
	// Plan is the plan, or empty when unknown.
	Plan string
	// Kind says where the credential lives.
	Kind string
	// Session is the five-hour window, when the response described one.
	Session *usage.LimitWindow
	// Weekly is the weekly window, when the response described one.
	Weekly *usage.LimitWindow
	// Extra holds every other window, one continuation row each.
	Extra []usage.LimitWindow
	// Credits is the finished credits cell.
	Credits string
	// State is the finished state cell, notes included.
	State string
	// VisibleByDefault says whether the row appears without the flag
	// that shows every row.
	VisibleByDefault bool
}

// CodexReport is a whole Codex pass, ready to render.
type CodexReport struct {
	// Rows holds every row the pass produced, hidden ones included.
	Rows []CodexTableRow
	// Now is the moment every countdown is relative to.
	Now time.Time
	// Zone is where the reset columns are printed.
	Zone *time.Location
	// ShowAll says whether every row was requested.
	ShowAll bool
}

// HiddenCount returns how many rows the show-everything flag would add.
func (r *CodexReport) HiddenCount() int {
	if r.ShowAll {
		return 0
	}
	hidden := 0
	for i := range r.Rows {
		if !r.Rows[i].VisibleByDefault {
			hidden++
		}
	}
	return hidden
}
