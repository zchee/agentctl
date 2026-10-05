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

package testutil

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"
	gocmp "github.com/google/go-cmp/cmp"
)

func TestFrameDump(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		content string
		width   int
		height  int
		want    string
	}{
		"success: rows pad to the width": {
			content: "ab\ncdef",
			width:   6,
			height:  2,
			want:    "\"ab    \"\n\"cdef  \"\n",
		},
		"success: missing rows render blank": {
			content: "ab",
			width:   3,
			height:  3,
			want:    "\"ab \"\n\"   \"\n\"   \"\n",
		},
		"success: quotes and backslashes escape": {
			content: `say "hi" \ bye`,
			width:   14,
			height:  1,
			want:    "\"say \\\"hi\\\" \\\\ bye\"\n",
		},
		"success: styling is stripped before measuring": {
			content: "\x1b[1mbold\x1b[0m",
			width:   6,
			height:  1,
			want:    "\"bold  \"\n",
		},
		"success: a double-width character fills two cells": {
			content: "日本",
			width:   6,
			height:  1,
			want:    "\"日本  \"\n",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := gocmp.Diff(tt.want, FrameDump(tt.content, tt.width, tt.height)); diff != "" {
				t.Fatalf("FrameDump mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFrameDumpReproducesTheFrameOracles proves the adapter's format is
// the oracles' format byte for byte: every stored frame golden, unquoted
// back into plain rows, round-trips through FrameDump unchanged.
func TestFrameDumpReproducesTheFrameOracles(t *testing.T) {
	t.Parallel()

	frames := []string{
		"tui__ui__tests__an_empty_display_says_so_rather_than_showing_nothing",
		"tui__ui__tests__two_accounts_render_as_expected",
		"tui__ui__tests__a_degraded_account_shows_its_badge_and_its_state",
		"provider__codex__account__tests__codex_watch_two_rows",
	}

	for _, name := range frames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			golden := string(ReadGolden(t, name))
			quoted := strings.Split(strings.TrimRight(golden, "\n"), "\n")
			rows := make([]string, 0, len(quoted))
			for _, line := range quoted {
				row, err := strconv.Unquote(line)
				if err != nil {
					t.Fatalf("golden line is not a quoted row: %q: %v", line, err)
				}
				rows = append(rows, row)
			}

			width := lipgloss.Width(rows[0])
			got := FrameDump(strings.Join(rows, "\n"), width, len(rows))
			if diff := gocmp.Diff(golden, got); diff != "" {
				t.Fatalf("round trip through FrameDump mismatch (-golden +got):\n%s", diff)
			}
		})
	}
}

// pairQuitMsg asks the test model to stop.
type pairQuitMsg struct{}

// pairModel is the smallest model that proves the test harness delivers
// the initial terminal size and runs the program to completion.
type pairModel struct {
	width  int
	height int
}

func (m pairModel) Init() tea.Cmd { return nil }

func (m pairModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case pairQuitMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m pairModel) View() tea.View {
	return tea.NewView(fmt.Sprintf("size %dx%d", m.width, m.height))
}

// TestTeatestPairRunsAModel proves the pinned TUI test library and the
// pinned framework version work together: the model is constructed, the
// initial terminal size arrives, a sent message is processed, and the
// final output carries the rendered view.
func TestTeatestPairRunsAModel(t *testing.T) {
	tm := teatest.NewTestModel(t, pairModel{}, teatest.WithInitialTermSize(84, 20))
	tm.Send(pairQuitMsg{})

	output, err := io.ReadAll(tm.FinalOutput(t, teatest.WithFinalTimeout(10*time.Second)))
	if err != nil {
		t.Fatalf("read the final output: %v", err)
	}
	if !strings.Contains(StripANSI(string(output)), "size 84x20") {
		t.Fatalf("the final output never rendered the delivered size; output:\n%s", output)
	}

	final, ok := tm.FinalModel(t).(pairModel)
	if !ok {
		t.Fatalf("the final model is not the test model: %T", tm.FinalModel(t))
	}
	if final.width != 84 || final.height != 20 {
		t.Fatalf("final model size = %dx%d, want 84x20", final.width, final.height)
	}
}
