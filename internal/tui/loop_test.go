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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
)

func TestLoopCoalescesRefreshAndSchedulesFromCompletion(t *testing.T) {
	type started struct {
		forced   bool
		deadline time.Time
	}
	starts := make(chan started, 3)
	release := make(chan struct{}, 3)
	m := NewLoop(t.Context(), fixtureModel(), time.Minute, func(ctx context.Context, forced bool) ([]fixtureRow, error) {
		deadline, _ := ctx.Deadline()
		starts <- started{forced, deadline}
		select {
		case <-release:
			return fixtureRows(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	defer m.Close()
	before := time.Now()
	first := m.startPass()
	a := <-starts
	if a.forced || a.deadline.Before(before.Add(55*time.Second)) || a.deadline.After(time.Now().Add(55*time.Second)) {
		t.Fatalf("first pass=%+v", a)
	}
	for range 5 {
		m.Update(tea.KeyPressMsg{Code: 'r'})
	}
	m.Update(Tick(time.Now().Add(time.Hour)))
	if !m.Display.Fetching {
		t.Fatal("tick stopped the in-flight pass")
	}
	select {
	case <-starts:
		t.Fatal("refresh overlapped a pass")
	default:
	}
	release <- struct{}{}
	_, second := m.Update(first())
	if second == nil {
		t.Fatal("pending refresh was lost")
	}
	if next := <-starts; !next.forced {
		t.Fatal("manual refresh did not bypass the cache")
	}
	release <- struct{}{}
	m.Update(second())
	if m.Display.Fetching || m.Display.Stale {
		t.Fatal("successful completion must clear fetching and stale")
	}
	if diff := gocmp.Diff(time.Minute, m.due.Sub(m.Display.LastFetch)); diff != "" {
		t.Fatal(diff)
	}
	m.Update(Tick(m.due.Add(-time.Nanosecond)))
	select {
	case <-starts:
		t.Fatal("pass started before its due time")
	default:
	}
	m.Update(Tick(m.due))
	if next := <-starts; next.forced {
		t.Fatal("scheduled pass unexpectedly bypassed cache")
	}
}

func TestLoopFailedPassPreservesRows(t *testing.T) {
	tests := map[string]struct{ fail func() error }{
		"error: registry failure": {func() error { return errors.New("registry unreadable") }},
		"error: worker panic":     {func() error { panic("planted panic detail") }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			display := fixtureModel()
			display.Reduce(fixtureRows())
			m := NewLoop(t.Context(), display, time.Minute, func(context.Context, bool) ([]fixtureRow, error) { return nil, tt.fail() })
			defer m.Close()
			wait := m.startPass()
			m.Update(wait())
			if len(display.Rows) != 2 || !display.Stale || display.Fetching || m.due.IsZero() {
				t.Fatalf("failed pass lost data or schedule: %+v", display)
			}
		})
	}
}

func TestLoopQuitDoesNotJoinAStuckPass(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	stopped := make(chan struct{})
	m := NewLoop(t.Context(), fixtureModel(), time.Minute, func(ctx context.Context, _ bool) ([]fixtureRow, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		<-release
		return nil, ctx.Err()
	})
	m.startPass()
	<-started
	done := m.done
	defer func() { close(release); <-done }()
	path := t.TempDir() + "/staged-credential"
	if err := os.WriteFile(path, []byte("test material"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := cleanup.Register(func() { _ = os.Remove(path) })
	defer cleanup.Unregister(token)
	before := time.Now()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q'})
	if elapsed := time.Since(before); elapsed >= 500*time.Millisecond {
		t.Fatalf("quit took %s", elapsed)
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("quit command was not emitted")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("pass was not canceled before cleanup")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file survived cleanup: %v", err)
	}
	m.Close()
}

func TestLoopQuitReapsAChild(t *testing.T) {
	started := make(chan *exec.Cmd, 1)
	m := NewLoop(t.Context(), fixtureModel(), time.Minute, func(ctx context.Context, _ bool) ([]fixtureRow, error) {
		cmd := exec.CommandContext(ctx, "sleep", "30")
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		started <- cmd
		return nil, cmd.Wait()
	})
	defer m.Close()
	m.startPass()
	var child *exec.Cmd
	select {
	case child = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("child did not start")
	}
	done := m.done
	m.Update(tea.KeyPressMsg{Code: 'q'})
	select {
	case <-done:
	default:
		t.Fatal("canceled child was not reaped during the drain")
	}
	if child.ProcessState == nil {
		t.Fatal("child has no exit state")
	}
}

func TestLoopRedrawsWhilePassIsBlocked(t *testing.T) {
	started := make(chan struct{})
	m := NewLoop(t.Context(), fixtureModel(), time.Minute, func(ctx context.Context, _ bool) ([]fixtureRow, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer m.Close()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(84, 20))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("initial pass did not start")
	}
	tick := time.Now().Add(time.Hour)
	tm.Send(Tick(tick))
	tm.Send(tea.WindowSizeMsg{Width: 60, Height: 8})
	tm.Send(tea.KeyPressMsg{Code: 'q'})
	final := tm.FinalModel(t, teatest.WithFinalTimeout(time.Second)).(*Loop[fixtureRow])
	if final.Display.Width != 60 || !final.Display.Now.Equal(tick) {
		t.Fatalf("blocked pass prevented updates: %+v", final.Display)
	}
	if !strings.Contains(final.View().Content, "agentctl claude watch") {
		t.Fatal("watch title missing")
	}
}

func TestLoopBudgets(t *testing.T) {
	tests := map[string]struct{ got, want time.Duration }{
		"success: deadline margin": {DeadlineMargin, 5 * time.Second},
		"success: poll interval":   {PollInterval, 250 * time.Millisecond},
		"success: quit drain":      {QuitDrainBudget, 250 * time.Millisecond},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, tt.got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
