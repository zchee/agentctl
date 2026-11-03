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
	"time"

	"github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/usage"
)

// CodexWatchRow adapts a provider-neutral account to the terminal display.
type CodexWatchRow struct{ codex.Account }

// Gauges returns the described windows without inventing missing figures.
func (r CodexWatchRow) Gauges() []Gauge {
	gauges := make([]Gauge, 0, 2)
	for _, entry := range []struct {
		kind  usage.WindowClass
		label string
	}{{usage.WindowSession, "5h"}, {usage.WindowWeeklyAll, "weekly"}} {
		if window := r.Window(entry.kind); window != nil && window.PercentFloor != nil {
			gauges = append(gauges, Gauge{Label: entry.label, Percent: *window.PercentFloor})
		}
	}
	return gauges
}

// DetailLine renders state, note, and the soonest reset using the shared countdown.
func (r CodexWatchRow) DetailLine(now time.Time) string {
	var note string
	if r.Note != nil {
		note = *r.Note
	}
	return DetailLine(now, codex.Badge(r.State), r.State.Label(), note, r.NextReset())
}

// AccountTitle renders a selectable block heading.
func (r CodexWatchRow) AccountTitle(selected bool) string {
	var plan string
	if r.Plan != nil {
		plan = *r.Plan
	}
	return AccountTitle(selected, r.Account.Account(), r.Kind.Name(), plan)
}

// BlockHeight includes the two borders and detail line.
func (r CodexWatchRow) BlockHeight() int { return BlockHeight(len(r.Gauges())) }

var _ TuiRow = CodexWatchRow{}
