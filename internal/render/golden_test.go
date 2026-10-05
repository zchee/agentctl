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
	"bytes"
	"strings"
	"testing"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/usage"
)

// The stored table bodies were recorded by a snapshot tool that trims
// trailing whitespace at end of file, so they lack the final row's
// right padding that a real run emits. Each table body is therefore
// compared through the end-of-file trim comparator, and the exact-byte
// tests further down pin what the trim cannot see: the untrimmed
// trailing padding and the final newline the printer adds.

func TestGoldenTwoAccounts(t *testing.T) {
	t.Parallel()

	rendered := Render(report(t, []StatusRow{
		healthyRow(t, "alice@example.com"),
		healthyRow(t, "bob@example.com"),
	}, false))
	testutil.GoldenTrimmed(t, "render__table__tests__two_accounts", []byte(rendered))
}

func TestGoldenHiddenFooter(t *testing.T) {
	t.Parallel()

	sibling := healthyRow(t, "5cdc535f")
	sibling.Usage = nil
	sibling.State = "stale sibling of live"
	sibling.VisibleByDefault = false

	switcher := emptyRow("claude-switcher:user", "unclaimed")
	switcher.VisibleByDefault = false

	rendered := Render(report(t, []StatusRow{healthyRow(t, "alice@example.com"), sibling, switcher}, false))
	testutil.GoldenTrimmed(t, "render__table__tests__hidden_footer", []byte(rendered))
}

func TestGoldenAllRows(t *testing.T) {
	t.Parallel()

	sibling := emptyRow("5cdc535f", "stale sibling of live")
	sibling.VisibleByDefault = false

	rendered := Render(report(t, []StatusRow{healthyRow(t, "alice@example.com"), sibling}, true))
	testutil.GoldenTrimmed(t, "render__table__tests__all_rows", []byte(rendered))
}

func TestGoldenContinuationRows(t *testing.T) {
	t.Parallel()

	windows := healthyWindows(t)
	windows = append(windows,
		window(unknownWindowKind("monthly_foo"), 7, ts(t, "2026-10-01T00:00:00Z")),
		window(weeklyScopedWindowKind("opus"), 12, time.Time{}),
	)
	row := healthyRow(t, "alice@example.com")
	row.Usage = &usage.UsageSnapshot{Windows: windows, Credits: creditsUnavailable()}

	rendered := Render(report(t, []StatusRow{row}, false))
	testutil.GoldenTrimmed(t, "render__table__tests__continuation_rows", []byte(rendered))
}

func TestGoldenNoNumbers(t *testing.T) {
	t.Parallel()

	row := emptyRow("alice@example.com", "keychain locked")
	row.Org = "Acme"

	rendered := Render(report(t, []StatusRow{row}, false))
	testutil.GoldenTrimmed(t, "render__table__tests__no_numbers", []byte(rendered))
}

func TestGoldenDegradedStates(t *testing.T) {
	t.Parallel()

	rateLimited := healthyRow(t, "alice@example.com")
	rateLimited.State = "rate-limited (retry in 30s)"
	rateLimited.Note = "showing cached values"

	detected := emptyRow(
		"bob@example.com",
		"claude session detected — refresh refused (lock .oauth_refresh.lock, age 3s)",
	)
	detected.Org = "Acme"

	noLimits := StatusRow{
		Account:          "carol@example.com",
		Org:              "Acme",
		State:            "no subscription limits (API/console account?)",
		Usage:            &usage.UsageSnapshot{Credits: creditsUnavailable()},
		VisibleByDefault: true,
		Kind:             "owned",
	}

	rendered := Render(report(t, []StatusRow{rateLimited, detected, noLimits}, false))
	testutil.GoldenTrimmed(t, "render__table__tests__degraded_states", []byte(rendered))
}

func TestGoldenCreditsCells(t *testing.T) {
	t.Parallel()

	// The five cell formats, one row named after each. The figures are
	// the ones the real capture bodies carry, already reduced to the
	// renderer's vocabulary: $12.34 used of a $50.00 cap at 25%, and the
	// same spend against no cap at all.
	states := []struct {
		name    string
		credits usage.CreditsState
	}{
		{"n/a", creditsUnavailable()},
		{"off", creditsOff()},
		{"capped", creditsOn(money(1234, "USD", 2), money(5000, "USD", 2), 25)},
		{"uncapped", creditsOn(money(1234, "USD", 2), nil, -1)},
		{"unmeasured", creditsOn(nil, nil, -1)},
	}

	rows := make([]StatusRow, 0, len(states))
	for _, state := range states {
		row := healthyRow(t, state.name)
		row.Usage = &usage.UsageSnapshot{Windows: healthyWindows(t), Credits: state.credits}
		rows = append(rows, row)
	}

	rendered := Render(report(t, rows, false))
	testutil.GoldenTrimmed(t, "render__table__tests__credits_cells", []byte(rendered))
}

func TestGoldenByIdentityKindColumn(t *testing.T) {
	t.Parallel()

	folded := healthyRow(t, "alice@example.com")
	folded.Kind = LiveAndOwnedKind
	plain := healthyRow(t, "bob@example.com")
	plain.Kind = "owned"

	rendered := Render(byIdentity(t, []StatusRow{folded, plain}))
	testutil.GoldenTrimmed(t, "render__table__tests__by_identity_kind_column", []byte(rendered))
}

// The exact-byte tests below pin what the trimmed table bodies cannot:
// every row — the last one included — keeps its right padding, and the
// printed form ends in exactly one newline. The expected strings carry
// their trailing spaces deliberately.

func TestClaudeStatusTableExactBytes(t *testing.T) {
	t.Parallel()

	rendered := Render(report(t, []StatusRow{healthyRow(t, "alice@example.com")}, false))
	want := strings.Join([]string{
		" Account           | Org  | Plan | 5h  | Weekly | Fable (weekly) | Credits | 5h reset         | Weekly reset         | State ",
		"-------------------+------+------+-----+--------+----------------+---------+------------------+----------------------+-------",
		" alice@example.com | Acme | max  | 21% | 35%    | 56%            | n/a     | 2h13m (11:13 AM) | 2d20h (Fri 05:00 AM) | ok    ",
	}, "\n")
	var printed bytes.Buffer
	if err := Print(&printed, rendered); err != nil {
		t.Fatalf("print the table: %v", err)
	}
	if diff := gocmp.Diff(want+"\n", printed.String()); diff != "" {
		t.Errorf("printed table mismatch (-want +got):\n%s", diff)
	}
}

func TestCodexStatusTableExactBytes(t *testing.T) {
	t.Parallel()

	rendered := RenderCodex(&CodexReport{
		Rows: []CodexTableRow{codexRow(t, "dev@example.com")},
		Now:  ts(t, reportNow),
		Zone: plusNine,
	})
	want := strings.Join([]string{
		" Account         | Plan | Kind | 5h  | Weekly | Credits | 5h reset         | Weekly reset         | State ",
		"-----------------+------+------+-----+--------+---------+------------------+----------------------+-------",
		" dev@example.com | plus | live | 21% | 35%    | —       | 2h13m (11:13 AM) | 2d20h (Fri 05:00 AM) | ok    ",
	}, "\n")
	var printed bytes.Buffer
	if err := Print(&printed, rendered); err != nil {
		t.Fatalf("print the table: %v", err)
	}
	if diff := gocmp.Diff(want+"\n", printed.String()); diff != "" {
		t.Errorf("printed table mismatch (-want +got):\n%s", diff)
	}
}

func TestFinishedCellTableExactBytes(t *testing.T) {
	t.Parallel()

	// The account listing renders finished cells through the shared
	// engine, so its exact shape is pinned here the same way.
	rendered := Table(
		[]string{"Id", "Account", "Org", "Kind", "Source", "State", "Location"},
		[][]string{{"owned:alice", "alice@example.com", "Acme", "owned", "keychain", "ok", "alice@example.com"}},
	)
	want := strings.Join([]string{
		" Id          | Account           | Org  | Kind  | Source   | State | Location          ",
		"-------------+-------------------+------+-------+----------+-------+-------------------",
		" owned:alice | alice@example.com | Acme | owned | keychain | ok    | alice@example.com ",
	}, "\n")
	var printed bytes.Buffer
	if err := Print(&printed, rendered); err != nil {
		t.Fatalf("print the table: %v", err)
	}
	if diff := gocmp.Diff(want+"\n", printed.String()); diff != "" {
		t.Errorf("printed table mismatch (-want +got):\n%s", diff)
	}
}

// TestColumnsAlignByDisplayWidth renders accounts and windows whose
// names leave the one-byte-one-cell world — full-width CJK, a combining
// mark, an emoji presentation sequence — and asserts every column edge
// still lands on the same terminal cell in every row. Byte or rune
// counting would misplace every separator after the CJK row.
func TestColumnsAlignByDisplayWidth(t *testing.T) {
	t.Parallel()

	cjk := healthyRow(t, "山田太郎")
	combining := healthyRow(t, "café@example.com")
	emoji := healthyRow(t, "alerts⚠️@example.com")
	windows := healthyWindows(t)
	windows = append(windows, window(weeklyScopedWindowKind("超大型モデル"), 12, time.Time{}))
	cjk.Usage = &usage.UsageSnapshot{Windows: windows, Credits: creditsUnavailable()}

	rendered := Render(report(t, []StatusRow{cjk, combining, emoji}, false))
	lines := strings.Split(rendered, "\n")
	if len(lines) < 5 {
		t.Fatalf("expected a header, a rule and three rows at least:\n%s", rendered)
	}

	headerCells := strings.Split(lines[0], "|")
	for i, line := range lines {
		separator := i == 1
		cut := "|"
		if separator {
			cut = "+"
		}
		cells := strings.Split(line, cut)
		if got, want := len(cells), len(headerCells); got != want {
			t.Fatalf("line %d has %d cells, want %d: %q", i, got, want, line)
		}
		for j, cell := range cells {
			if got, want := lipgloss.Width(cell), lipgloss.Width(headerCells[j]); got != want {
				t.Errorf("line %d cell %d is %d cells wide, want %d: %q", i, j, got, want, cell)
			}
		}
	}
}

// TestDisplayWidthCorpus pins the width function itself on the shapes
// the tables meet, so a dependency upgrade that changes any of these
// answers fails here, by name, rather than as a misaligned table.
func TestDisplayWidthCorpus(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		text string
		want int
	}{
		"success: full-width CJK is two cells per character": {
			text: "山田太郎", want: 8,
		},
		"success: a combining mark adds no width": {
			text: "café", want: 4,
		},
		"success: a precomposed accent is one cell": {
			text: "café", want: 4,
		},
		"success: an emoji is two cells": {
			text: "🙂", want: 2,
		},
		"success: an emoji presentation sequence is two cells": {
			// U+26A0 WARNING SIGN alone is narrow; the variation
			// selector asks for the emoji form, which is wide.
			text: "⚠️", want: 2,
		},
		"success: the em dash is one cell": {
			text: EmptyCell, want: 1,
		},
		"success: the continuation marker is one cell": {
			text: ContinuationMarker, want: 1,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := lipgloss.Width(tt.text); got != tt.want {
				t.Errorf("Width(%q) = %d, want %d (bytes % x)", tt.text, got, tt.want, []byte(tt.text))
			}
		})
	}
}
