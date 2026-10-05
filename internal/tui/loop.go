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

package tui

import (
	"context"
	"errors"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/runtime/cleanup"
)

const (
	// DeadlineMargin is the part of an interval reserved between passes.
	DeadlineMargin = 5 * time.Second
	// PollInterval bounds the time between countdown updates.
	PollInterval = 250 * time.Millisecond
	// QuitDrainBudget bounds the wait for a canceled pass before cleanup.
	QuitDrainBudget = 250 * time.Millisecond
)

// Pass returns one complete set of rows. An error preserves the previous
// display; an empty successful slice means there are no accounts.
// The context carries both cancellation and the pass deadline.
type Pass[R render.TuiRow] func(ctx context.Context, forced bool) ([]R, error)

type passResult[R render.TuiRow] struct {
	rows []R
	err  error
}

// Loop schedules at most one pass and keeps the display responsive while it
// runs. Repeated refresh keys coalesce into one uncached follow-up pass.
// Close must be called after the program exits, including error paths.
type Loop[R render.TuiRow] struct {
	Display        *Model[R]
	ctx            context.Context
	cancel         context.CancelFunc
	pass           Pass[R]
	interval       time.Duration
	due            time.Time
	inFlight       <-chan passResult[R]
	done           <-chan struct{}
	forced, closed bool
}

// NewLoop creates a watch session with an immediate first pass.
func NewLoop[R render.TuiRow](ctx context.Context, display *Model[R], interval time.Duration, pass Pass[R]) *Loop[R] {
	ctx, cancel := context.WithCancel(ctx)
	return &Loop[R]{Display: display, ctx: ctx, cancel: cancel, interval: interval, pass: pass}
}

// Init starts the first pass and the display clock without blocking input.
func (m *Loop[R]) Init() tea.Cmd {
	if m.ctx.Err() != nil {
		return tea.Quit
	}
	return tea.Batch(m.startPass(), clockTick())
}

// Update handles keys, completed passes, resize events and scheduled ticks.
func (m *Loop[R]) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.closed {
		return m, nil
	}
	if m.ctx.Err() != nil {
		m.Close()
		return m, tea.Quit
	}
	switch msg := msg.(type) {
	case passResult[R]:
		m.inFlight = nil
		m.done = nil
		at := time.Now()
		if msg.err == nil {
			m.Display.Reduce(Rows[R](msg.rows))
		} else {
			slog.WarnContext(m.ctx, "a watch pass ended without producing rows", "error", msg.err)
		}
		m.due = at.Add(m.interval)
		m.Display.Reduce(PassFinished{At: at, Next: m.due})
		if m.forced {
			return m, m.startPass()
		}
	case Tick:
		m.Display.Reduce(msg)
		var start tea.Cmd
		if m.inFlight == nil && !m.due.IsZero() && !time.Time(msg).Before(m.due) {
			start = m.startPass()
		}
		return m, tea.Batch(start, clockTick())
	default:
		switch m.Display.Reduce(msg) {
		case Quit:
			m.Close()
			return m, tea.Quit
		case Refresh:
			m.forced = true
			if m.inFlight == nil {
				return m, m.startPass()
			}
		}
	}
	return m, nil
}

// View renders the state without reading a clock or waiting on a pass.
func (m *Loop[R]) View() tea.View { return m.Display.View() }

// Close cancels the worker, drains it for a bounded interval and runs emergency
// cleanup. It never joins a pass that ignores cancellation.
func (m *Loop[R]) Close() {
	if m.closed {
		return
	}
	m.closed = true
	m.cancel()
	if m.done != nil {
		timer := time.NewTimer(QuitDrainBudget)
		defer timer.Stop()
		select {
		case <-m.done:
		case <-timer.C:
		}
	}
	cleanup.Run()
}

func (m *Loop[R]) startPass() tea.Cmd {
	forced := m.forced
	m.forced = false
	m.due = time.Time{}
	m.Display.Reduce(PassStarted{})
	results := make(chan passResult[R], 1)
	done := make(chan struct{})
	m.inFlight = results
	m.done = done
	ctx, cancel := context.WithTimeout(m.ctx, max(0, m.interval-DeadlineMargin))
	go func() {
		result := passResult[R]{}
		defer func() {
			if recover() != nil {
				result.err = errors.New("the watch pass panicked")
			}
			cancel()
			results <- result
			close(done)
		}()
		result.rows, result.err = m.pass(ctx, forced)
	}()
	return func() tea.Msg {
		select {
		case result := <-results:
			return result
		case <-m.ctx.Done():
			return nil
		}
	}
}

func clockTick() tea.Cmd {
	return tea.Tick(PollInterval, func(at time.Time) tea.Msg { return Tick(at) })
}
