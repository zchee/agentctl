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
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/tty"
)

// Prompter prints a proposed action and asks for explicit human confirmation.
// Commands bypass Confirm only when their own --yes option is set.
type Prompter interface {
	Tell(message string) error
	Confirm(ctx context.Context, question string) (bool, error)
}

// TerminalPrompt asks questions on a terminal and prints on Out.
// In must be a terminal; piped answers cannot authorize a destructive action.
type TerminalPrompt struct {
	In  *os.File
	Out io.Writer
}

// Tell prints one report with its trailing newline.
func (p TerminalPrompt) Tell(message string) error { return tell(p.Out, message) }

// Confirm accepts y or yes, case-insensitively, after trimming whitespace.
// It refuses non-terminal input and returns I/O errors or cancellation without
// authorizing the action. Machine-readable output never implies consent.
func (p TerminalPrompt) Confirm(ctx context.Context, question string) (bool, error) {
	if !tty.IsTerminal(p.In) {
		return false, errs.NewRefused(0, question+" — standard input is not a terminal, so there is nobody to ask; pass `--yes` to say so up front")
	}
	if _, err := fmt.Fprintf(p.Out, "%s [y/N] ", question); err != nil {
		return false, errs.NewIO("could not write the confirmation prompt", err)
	}
	if flusher, ok := p.Out.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return false, errs.NewIO("could not write the confirmation prompt", err)
		}
	}
	var answer strings.Builder
	var readiness tty.Readiness
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ready, err := readiness.WaitReadable(p.In, 250*time.Millisecond)
		if err != nil {
			return false, errs.NewIO("could not read the confirmation", err)
		}
		if !ready {
			continue
		}
		var b [1]byte
		n, err := p.In.Read(b[:])
		if err != nil && err != io.EOF {
			return false, errs.NewIO("could not read the confirmation", err)
		}
		if n == 0 || b[0] == '\n' {
			break
		}
		answer.WriteByte(b[0])
	}
	value := strings.TrimSpace(answer.String())
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes"), nil
}
