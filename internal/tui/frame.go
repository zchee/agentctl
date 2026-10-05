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
	"fmt"
	"math"
	"strings"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/zchee/agentctl/internal/usage"
)

// HelpLine lists the watch display's key bindings.
const HelpLine = "q quit · r refresh · ↑↓ select"

const gaugeLabelWidth = 10

// Header describes shown rows, fetch timing and whether their numbers are stale.
func (m *Model[R]) Header() string {
	noun := "accounts"
	if len(m.Rows) == 1 {
		noun = "account"
	}
	parts := []string{fmt.Sprintf("%s · %d %s", m.Title, len(m.Rows), noun)}
	if m.LastFetch.IsZero() {
		parts = append(parts, "last fetch —")
	} else {
		parts = append(parts, "last fetch "+usage.RenderCountdown(m.LastFetch, m.Now)+" ago")
	}
	switch {
	case m.Fetching:
		parts = append(parts, "fetching")
	case m.NextFetch.IsZero():
		parts = append(parts, "next fetch —")
	default:
		parts = append(parts, "next fetch in "+usage.RenderCountdown(m.Now, m.NextFetch))
	}
	if m.Stale {
		parts = append(parts, "stale")
	}
	return strings.Join(parts, " · ")
}

// Footer points hidden entries at the provider's full status command.
func (m *Model[R]) Footer() string {
	if m.Hidden == 0 {
		return HelpLine
	}
	noun := "entries"
	if m.Hidden == 1 {
		noun = "entry"
	}
	return fmt.Sprintf("%s\n%d %s hidden (%s)", HelpLine, m.Hidden, noun, m.HiddenHint)
}

// Frame lays out a header, top-aligned account blocks, and a bottom footer.
func (m *Model[R]) Frame() string {
	if m.Width <= 0 || m.Height <= 0 {
		return ""
	}
	lines := make([]string, m.Height)
	lines[0] = m.Header()
	footer := strings.Split(m.Footer(), "\n")
	footerStart := max(1, m.Height-len(footer))
	for i, line := range footer {
		if footerStart+i < len(lines) {
			lines[footerStart+i] = line
		}
	}
	if len(m.Rows) == 0 && footerStart > 1 {
		lines[1] = "no accounts to show"
	}
	at := 1
	for i, row := range m.Rows {
		if at >= footerStart {
			break
		}
		height := min(row.BlockHeight(), footerStart-at)
		inner := max(0, m.Width-2)
		border := lipgloss.NormalBorder()
		title := fit(row.AccountTitle(i == m.Selected), inner)
		top := border.TopLeft + title + strings.Repeat(border.Top, max(0, inner-lipgloss.Width(title))) + border.TopRight
		lines[at] = top
		for y := 1; y < height-1; y++ {
			lines[at+y] = border.Left + strings.Repeat(" ", inner) + border.Right
		}
		bars := row.Gauges()
		for j, gauge := range bars {
			if j+1 >= height-1 {
				break
			}
			labelWidth := min(gaugeLabelWidth, inner)
			label := pad(gauge.Label, labelWidth)
			lines[at+j+1] = border.Left + label + gaugeBar(gauge.Percent, inner-labelWidth) + border.Right
		}
		if len(bars)+1 < height-1 {
			lines[at+len(bars)+1] = border.Left + pad(row.DetailLine(m.Now), inner) + border.Right
		}
		if height > 1 {
			lines[at+height-1] = border.BottomLeft + strings.Repeat(border.Bottom, inner) + border.BottomRight
		}
		at += height
	}
	for i := range lines {
		lines[i] = pad(lines[i], m.Width)
	}
	return strings.Join(lines, "\n")
}

func fit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(width).MaxHeight(1).Render(text)
}

func pad(text string, width int) string {
	text = fit(text, width)
	return text + strings.Repeat(" ", max(0, width-lipgloss.Width(text)))
}

func gaugeBar(percent, width int) string {
	if width <= 0 {
		return ""
	}
	percent = min(100, max(0, percent))
	filled := int(math.Round(float64(width) * float64(percent) / 100))
	cells := []rune(strings.Repeat("█", filled) + strings.Repeat(" ", width-filled))
	label := []rune(fmt.Sprintf("%d%%", percent))
	start := max(0, (width-len(label))/2)
	copy(cells[start:], label)
	// Keep the cell immediately after the centered label clear of the fill.
	if end := start + len(label); end < width {
		cells[end] = ' '
	}
	return string(cells)
}
