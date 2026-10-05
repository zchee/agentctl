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
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/testutil"
)

type fixtureRow struct {
	account, plan, detail string
	bars                  []render.Gauge
	hidden                bool
}

func (fixtureRow) WatchTitle() string            { return "agentctl claude watch" }
func (fixtureRow) HiddenHint() string            { return "agentctl claude status --all" }
func (r fixtureRow) Gauges() []render.Gauge      { return r.bars }
func (r fixtureRow) DetailLine(time.Time) string { return r.detail }
func (r fixtureRow) AccountTitle(selected bool) string {
	return render.AccountTitle(selected, r.account, "Acme", r.plan)
}
func (r fixtureRow) BlockHeight() int       { return render.BlockHeight(len(r.bars)) }
func (r fixtureRow) VisibleByDefault() bool { return !r.hidden }
func (fixtureRow) StateToken() string       { return "ok" }

func fixtureModel() *Model[fixtureRow] {
	now := time.Date(2026, time.September, 8, 12, 0, 30, 0, time.UTC)
	return New[fixtureRow](now, fixtureRow{}.WatchTitle(), fixtureRow{}.HiddenHint())
}

func fixtureRows() Rows[fixtureRow] {
	return Rows[fixtureRow]{
		{account: "owner@example.com", plan: "max", detail: "ok · next reset in 2h12m", bars: []render.Gauge{{Label: "5h", Percent: 21}, {Label: "weekly", Percent: 35}, {Label: "Fable", Percent: 56}}},
		{account: "second@example.com", plan: "max", detail: "ok · next reset in 2h12m", bars: []render.Gauge{{Label: "5h", Percent: 4}, {Label: "weekly", Percent: 11}, {Label: "Fable", Percent: 7}, {Label: "credits", Percent: 25}}},
		{account: "sibling@example.com", hidden: true},
	}
}

func TestModelReducer(t *testing.T) {
	tests := map[string]struct {
		run func(*testing.T, *Model[fixtureRow])
	}{
		"success: shown rows and hidden count": {func(t *testing.T, m *Model[fixtureRow]) {
			m.Reduce(fixtureRows())
			if len(m.Rows) != 2 || m.Hidden != 1 {
				t.Fatalf("rows=%d hidden=%d", len(m.Rows), m.Hidden)
			}
		}},
		"success: only existing rows become stale": {func(t *testing.T, m *Model[fixtureRow]) {
			m.Reduce(PassStarted{})
			if !m.Fetching || m.Stale {
				t.Fatal("first pass must fetch without stale numbers")
			}
			m.Reduce(fixtureRows())
			m.Reduce(PassStarted{})
			if !m.Stale {
				t.Fatal("existing rows must become stale")
			}
			m.Reduce(fixtureRows())
			if m.Stale {
				t.Fatal("new rows must clear stale")
			}
		}},
		"success: finish records schedule without clearing stale": {func(t *testing.T, m *Model[fixtureRow]) {
			m.Reduce(fixtureRows())
			m.Reduce(PassStarted{})
			next := m.Now.Add(time.Minute)
			m.Reduce(PassFinished{At: m.Now, Next: next})
			if m.Fetching || !m.Stale || !m.LastFetch.Equal(m.Now) || !m.NextFetch.Equal(next) {
				t.Fatalf("state=%+v", m)
			}
			m.Reduce(PassFinished{At: m.Now})
			if !m.NextFetch.IsZero() {
				t.Fatal("overflow schedule must stay unset")
			}
		}},
		"success: tick changes frame clock": {func(t *testing.T, m *Model[fixtureRow]) {
			next := m.Now.Add(time.Second)
			m.Reduce(Tick(next))
			if !m.Now.Equal(next) {
				t.Fatalf("clock=%s", m.Now)
			}
		}},
		"success: selection clamps at both ends and after row removal": {func(t *testing.T, m *Model[fixtureRow]) {
			m.Reduce(fixtureRows())
			for range 4 {
				m.Reduce(tea.KeyPressMsg{Code: 'j'})
			}
			if m.Selected != 1 {
				t.Fatalf("selection=%d", m.Selected)
			}
			m.Reduce(fixtureRows()[:1])
			if m.Selected != 0 {
				t.Fatalf("shortened selection=%d", m.Selected)
			}
			m.Reduce(Rows[fixtureRow]{})
			m.Reduce(tea.KeyPressMsg{Code: 'j'})
			m.Reduce(tea.KeyPressMsg{Code: 'k'})
			if m.Selected != 0 {
				t.Fatalf("empty selection=%d", m.Selected)
			}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { tt.run(t, fixtureModel()) })
	}
}

func TestKeyBindings(t *testing.T) {
	tests := map[string]struct {
		msg  tea.Msg
		want Effect
	}{
		"success: q":              {tea.KeyPressMsg{Code: 'q'}, Quit},
		"success: escape":         {tea.KeyPressMsg{Code: tea.KeyEscape}, Quit},
		"success: control c":      {tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, Quit},
		"success: control d":      {tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}, Quit},
		"success: refresh":        {tea.KeyPressMsg{Code: 'r'}, Refresh},
		"success: ignore release": {tea.KeyReleaseMsg{Code: 'q'}, None},
		"success: ignore repeat":  {tea.KeyPressMsg{Code: 'q', IsRepeat: true}, None},
		"success: ignore plain c": {tea.KeyPressMsg{Code: 'c'}, None},
		"success: ignore mouse":   {tea.MouseClickMsg{}, None},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, fixtureModel().Reduce(tt.msg)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestFrameGoldens(t *testing.T) {
	tests := map[string]struct {
		golden        string
		width, height int
		prepare       func(*Model[fixtureRow])
	}{
		"success: two accounts": {"two_accounts_render_as_expected", 84, 20, func(m *Model[fixtureRow]) {
			m.Reduce(fixtureRows())
			at := m.Now.Add(-30 * time.Second)
			m.Reduce(PassFinished{At: at, Next: at.Add(5 * time.Minute)})
		}},
		"success: degraded accounts": {"a_degraded_account_shows_its_badge_and_its_state", 84, 20, func(m *Model[fixtureRow]) {
			rows := fixtureRows()[:2]
			rows[0].detail = "[claude-detected] · claude session detected — refresh refused (lock .oauth_refresh.lock, age 4000ms) · next reset in 2h12m"
			rows[1] = fixtureRow{account: "live@example.com", detail: "[keychain-locked] · keychain locked"}
			m.Reduce(rows)
			m.Reduce(PassStarted{})
		}},
		"success: empty display": {"an_empty_display_says_so_rather_than_showing_nothing", 60, 8, func(*Model[fixtureRow]) {}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := fixtureModel()
			tt.prepare(m)
			tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(tt.width, tt.height))
			tm.Send(tea.WindowSizeMsg{Width: tt.width, Height: tt.height})
			tm.Send(Tick(m.Now))
			tm.Send(tea.KeyPressMsg{Code: 'q'})
			final := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).(*Model[fixtureRow])
			got := testutil.FrameDump(final.View().Content, tt.width, tt.height)
			want := string(testutil.ReadGolden(t, "tui__ui__tests__"+tt.golden))
			// Reflow only the renamed command strings, preserving all other oracle cells.
			var normalized []string
			for line := range strings.SplitSeq(strings.TrimSpace(want), "\n") {
				line = strings.TrimSuffix(strings.TrimPrefix(line, `"`), `"`)
				if strings.Contains(line, "agctl ") {
					line = pad(strings.ReplaceAll(line, "agctl ", "agentctl "), tt.width)
				}
				normalized = append(normalized, `"`+line+`"`)
			}
			if diff := gocmp.Diff(strings.Join(normalized, "\n"), strings.TrimSpace(got)); diff != "" {
				t.Fatalf("frame (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHeaderAndFooter(t *testing.T) {
	m := fixtureModel()
	if !strings.Contains(m.Header(), "last fetch — · next fetch —") {
		t.Fatal(m.Header())
	}
	m.Reduce(fixtureRows())
	m.Reduce(PassStarted{})
	if !strings.Contains(m.Header(), "fetching · stale") || strings.Contains(m.Header(), "next fetch") {
		t.Fatal(m.Header())
	}
	if !strings.Contains(m.Footer(), "1 entry hidden (agentctl claude status --all)") {
		t.Fatal(m.Footer())
	}
}
