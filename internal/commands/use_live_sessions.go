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

package commands

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/zchee/agentctl/internal/runtime/proc"
	"github.com/zchee/agentctl/internal/secret"
)

type useSessionHints struct {
	names      []string
	unreadable bool
}

func useScanSessions(ctx context.Context, dir string) useSessionHints {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return useSessionHints{unreadable: !os.IsNotExist(err)}
	}
	var result useSessionHints
	considered := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if considered == 512 {
			break
		}
		considered++
		read, err := secret.ReadFile(filepath.Join(dir, entry.Name()), 64*1024-1)
		if err != nil || !read.Present {
			continue
		}
		var document struct {
			PID    *uint32 `json:"pid"`
			Name   *string `json:"name"`
			Bridge *string `json:"bridgeSessionId"`
		}
		if json.Unmarshal(read.Bytes, &document) != nil || document.Bridge == nil || *document.Bridge == "" {
			continue
		}
		pid := uint64(0)
		if document.PID != nil {
			pid = uint64(*document.PID)
		} else {
			pid, _ = strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 32)
		}
		if pid == 0 {
			continue
		}
		if _, err := proc.Lookup(ctx, int(pid)); errors.Is(err, proc.ErrProcessGone) {
			continue
		}
		name := ""
		if document.Name != nil {
			name = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				if r == '`' {
					return '\''
				}
				return r
			}, strings.TrimSpace(*document.Name))
			name = strings.TrimSpace(name)
			chars := []rune(name)
			if len(chars) > 48 {
				name = string(chars[:47]) + "…"
			}
		}
		result.names = append(result.names, name)
	}
	return result
}

func (p SessionProcess) tellUseSessions(ctx context.Context, hints useSessionHints, report *useReport) {
	if message := hints.completion(true); message != "" {
		if err := tell(p.Err, "warning: "+message); err != nil {
			slog.ErrorContext(ctx, "the Remote Control advisory could not be written", slog.Any("error", err))
		}
		report.warnings = append(report.warnings, hints.completion(false))
	}
}

func (hints useSessionHints) list() string {
	var named []string
	unnamed := 0
	for _, name := range hints.names {
		if name == "" {
			unnamed++
		} else {
			named = append(named, name)
		}
	}
	slices.Sort(named)
	var parts []string
	for _, name := range named[:min(3, len(named))] {
		parts = append(parts, "`"+name+"`")
	}
	if unnamed != 0 {
		parts = append(parts, fmt.Sprintf("%d unnamed", unnamed))
	}
	if len(named) > 3 {
		parts = append(parts, fmt.Sprintf("%d more", len(named)-3))
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func (hints useSessionHints) consent() string {
	if len(hints.names) == 0 {
		return ""
	}
	plural, have := "", "has"
	if len(hints.names) != 1 {
		plural, have = "s", "have"
	}
	return fmt.Sprintf(". %d running Claude Code session%s (%s) %s Remote Control on, and agentctl cannot tell which of them use this store. To keep a session's claude.ai history, answer n, run `/remote-control` in that session and disconnect, then run this command again — do not leave this question open while you do it, because it counts against this swap's 120-second limit. If you answer y, Remote Control stops in each session that uses this store, and starting it again there begins a remote session without the earlier conversation", len(hints.names), plural, hints.list(), have)
}

func (hints useSessionHints) completion(names bool) string {
	if len(hints.names) == 0 {
		return ""
	}
	plural, list := "", ""
	if len(hints.names) != 1 {
		plural = "s"
	}
	if names {
		list = " (" + hints.list() + ")"
	}
	return fmt.Sprintf("%d Claude Code session%s%s had Remote Control on when this swap started, and agentctl cannot tell which of them use this store. In each one that does, Remote Control stops (now, or on its next account check): run `/remote-control` there to start it again. Its earlier conversation reaches claude.ai only if Remote Control was disconnected there before the swap", len(hints.names), plural, list)
}
