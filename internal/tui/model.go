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

// Package tui implements the provider-neutral watch display.
package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/zchee/agentctl/internal/render"
)

// Effect is work requested by a key, outside the display's state.
type Effect uint8

const (
	// None means only the display state changed.
	None Effect = iota
	// Quit cancels the running pass and leaves the display.
	Quit
	// Refresh requests a pass that bypasses the cache.
	Refresh
)

// Rows replaces the display with a completed pass, including hidden rows.
type Rows[R render.TuiRow] []R

// Tick advances the clock used by every countdown in the frame.
type Tick time.Time

// PassStarted marks existing numbers as stale while a pass is running.
type PassStarted struct{}

// PassFinished records the completed pass and its next scheduled fetch.
// A zero Next means that no further fetch is scheduled.
type PassFinished struct {
	At, Next time.Time
}

// Model holds display state without terminal, worker, or clock handles.
type Model[R render.TuiRow] struct {
	Rows                 []R
	LastFetch, NextFetch time.Time
	Stale, Fetching      bool
	Selected, Hidden     int
	Now                  time.Time
	Width, Height        int
	Title, HiddenHint    string
}

// New creates an empty display before the first pass finishes.
func New[R render.TuiRow](now time.Time, title, hiddenHint string) *Model[R] {
	return &Model[R]{Now: now, Title: title, HiddenHint: hiddenHint}
}

// Init leaves scheduling to the watch loop.
func (m *Model[R]) Init() tea.Cmd { return nil }

// Update implements the Bubble Tea model, emitting effects as messages.
func (m *Model[R]) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch effect := m.Reduce(msg); effect {
	case Quit:
		return m, tea.Quit
	case Refresh:
		return m, func() tea.Msg { return Refresh }
	default:
		return m, nil
	}
}

// Reduce folds one event into the display and returns any requested work.
func (m *Model[R]) Reduce(msg tea.Msg) Effect {
	switch msg := msg.(type) {
	case Rows[R]:
		m.Rows = make([]R, 0, len(msg))
		m.Hidden = 0
		for _, row := range msg {
			if row.VisibleByDefault() {
				m.Rows = append(m.Rows, row)
			} else {
				m.Hidden++
			}
		}
		m.Selected = min(m.Selected, max(0, len(m.Rows)-1))
		m.Stale = false
	case Tick:
		m.Now = time.Time(msg)
	case PassStarted:
		m.Fetching = true
		m.Stale = len(m.Rows) != 0
	case PassFinished:
		m.Fetching = false
		m.LastFetch, m.NextFetch = msg.At, msg.Next
	case tea.WindowSizeMsg:
		m.Width, m.Height = max(0, msg.Width), max(0, msg.Height)
	case tea.KeyPressMsg:
		if msg.IsRepeat {
			return None
		}
		switch {
		case msg.Code == tea.KeyEscape || msg.Code == 'q' || (msg.Mod&tea.ModCtrl != 0 && (msg.Code == 'c' || msg.Code == 'd')):
			return Quit
		case msg.Code == 'r':
			return Refresh
		case msg.Code == tea.KeyUp || msg.Code == 'k':
			m.Selected = max(0, m.Selected-1)
		case msg.Code == tea.KeyDown || msg.Code == 'j':
			if m.Selected < len(m.Rows)-1 {
				m.Selected++
			}
		}
	}
	return None
}

// View returns a full-screen frame with no mouse tracking.
func (m *Model[R]) View() tea.View {
	view := tea.NewView(m.Frame())
	view.AltScreen = true
	return view
}
