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

import "github.com/zchee/agentctl/internal/provider/codex"

// CodexWatchRow adapts a provider-neutral account to the terminal display.
type CodexWatchRow struct{ codex.Account }

// Gauges converts the described windows without inventing missing figures.
func (r CodexWatchRow) Gauges() []Gauge {
	source := r.Account.Gauges()
	gauges := make([]Gauge, len(source))
	for i, gauge := range source {
		gauges[i] = Gauge{Label: gauge.Label, Percent: gauge.Percent}
	}
	return gauges
}

// WatchTitle names the command that owns this terminal display.
func (CodexWatchRow) WatchTitle() string { return "agentctl codex watch" }

// HiddenHint names the command that reveals the hidden accounts.
func (CodexWatchRow) HiddenHint() string { return "agentctl codex status --all" }

var _ TuiRow = CodexWatchRow{}
