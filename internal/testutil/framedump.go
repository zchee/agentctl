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
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// FrameDump renders one frame of view content in the quoted format the
// terminal-frame goldens use: one line per terminal row, each row padded
// with spaces to the terminal's width, wrapped in double quotes, with
// backslashes and quotes escaped. Styling sequences are stripped first,
// so the dump describes what a viewer sees, never the escape-code stream.
//
// Padding is by display width, because a cell grid is measured in cells:
// a double-width character fills two, and len or a rune count would pad
// such a row past the grid's edge.
func FrameDump(content string, width, height int) string {
	var out strings.Builder
	lines := strings.Split(StripANSI(content), "\n")
	for row := range height {
		line := ""
		if row < len(lines) {
			line = lines[row]
		}
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		line = strings.ReplaceAll(line, `\`, `\\`)
		line = strings.ReplaceAll(line, `"`, `\"`)
		out.WriteByte('"')
		out.WriteString(line)
		out.WriteString("\"\n")
	}
	return out.String()
}
