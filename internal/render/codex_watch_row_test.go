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

package render_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/tui"
)

func TestCodexWatchGolden(t *testing.T) {
	accounts, at := codexProjectionFixture(t)
	row := render.CodexWatchRow{Account: accounts[0]}
	if diff := gocmp.Diff([]render.Gauge{{Label: "5h", Percent: 21}, {Label: "weekly", Percent: 35}}, row.Gauges()); diff != "" {
		t.Fatalf("gauges (-want +got):\n%s", diff)
	}
	model := tui.New[render.CodexWatchRow](at.Add(30*time.Second), row.WatchTitle(), row.HiddenHint())
	rows := make(tui.Rows[render.CodexWatchRow], len(accounts))
	for i, account := range accounts {
		rows[i] = render.CodexWatchRow{Account: account}
	}
	model.Reduce(rows)
	model.Reduce(tui.PassFinished{At: at, Next: at.Add(5 * time.Minute)})
	model.Reduce(tea.WindowSizeMsg{Width: 84, Height: 16})
	got := strings.TrimSpace(testutil.FrameDump(model.View().Content, 84, 16))
	want := strings.TrimSpace(string(testutil.ReadGolden(t, "provider__codex__account__tests__codex_watch_two_rows")))
	lines := strings.Split(want, "\n")
	for i, line := range lines {
		if strings.Contains(line, "agctl codex watch") {
			text := strings.TrimSuffix(strings.TrimPrefix(line, `"`), `"`)
			text = strings.ReplaceAll(text, "agctl codex watch", "agentctl codex watch")
			text = strings.TrimRight(text, " ")
			lines[i] = `"` + text + strings.Repeat(" ", max(84-len([]rune(text)), 0)) + `"`
		}
	}
	if diff := gocmp.Diff(strings.Join(lines, "\n"), got); diff != "" {
		t.Fatalf("watch frame (-want +got):\n%s", diff)
	}
}
