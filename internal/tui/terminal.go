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
	"io"
	"os"
	"sync"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/logbuf"
	"github.com/zchee/agentctl/internal/runtime/tty"
)

// Run owns the terminal until the model quits or ctx is canceled. It refuses
// redirected streams, restores raw mode on every exit route, and flushes logs
// only after returning to the ordinary screen. The program's panic handler
// restores its renderer before printing a panic; the independent cleanup entry
// also covers process-level signal teardown and partial initialization errors.
func Run(ctx context.Context, model tea.Model, input, output *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !tty.IsTerminal(input) || !tty.IsTerminal(output) {
		return errs.NewIO("the watch display requires a terminal on stdin and stdout", errors.New("not a terminal"))
	}
	state, err := term.GetState(int(input.Fd()))
	if err != nil {
		return errs.NewIO("the terminal state could not be read", err)
	}
	width, height, err := tty.Size(output)
	if err != nil {
		return errs.NewIO("the terminal size could not be read", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var restoreMu sync.Mutex
	restore := func() {
		restoreMu.Lock()
		defer restoreMu.Unlock()
		cancel()
		// Do not depend on renderer initialization having completed: emergency
		// cleanup may run before the program has drawn its first frame.
		_ = term.Restore(int(input.Fd()), state)
		_, _ = io.WriteString(output, "\x1b[?1049l\x1b[?25h")
		logbuf.ReleaseTerminal()
	}
	token := cleanup.Register(restore)
	defer func() {
		restore()
		cleanup.Unregister(token)
	}()
	logbuf.HoldTerminal()
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output), tea.WithWindowSize(width, height), tea.WithoutSignalHandler())
	_, err = program.Run()
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return nil
		}
		return errs.NewIO("the watch display could not be run", err)
	}
	return nil
}
