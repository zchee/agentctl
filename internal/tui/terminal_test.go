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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/zchee/agentctl/internal/runtime/logbuf"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/runtime/tty"
)

type terminalTestModel struct{ *Model[fixtureRow] }

func (m terminalTestModel) Init() tea.Cmd {
	slog.Warn("held log marker")
	return nil
}

func (m terminalTestModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.Code == 'p' {
		panic("terminal panic marker")
	}
	_, cmd := m.Model.Update(msg)
	return m, cmd
}

func TestTerminalProcess(t *testing.T) {
	if os.Getenv("AGENTCTL_TEST_TERMINAL_CHILD") != "1" {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logbuf.Writer{}, nil)))
	ctx, controller := signals.Install(t.Context())
	defer controller.Stop()
	err := Run(ctx, terminalTestModel{fixtureModel()}, os.Stdin, os.Stdout)
	controller.Wait()
	if sig, ok := controller.Fired(); ok {
		os.Exit(128 + int(sig.(syscall.Signal)))
	}
	if err != nil {
		if errors.Is(err, tea.ErrProgramPanic) {
			os.Exit(2)
		}
		t.Fatal(err)
	}
}

func TestTerminalRestoration(t *testing.T) {
	tests := map[string]struct {
		key       string
		signal    syscall.Signal
		exit      int
		panicText bool
	}{
		"success: quit":          {key: "q"},
		"success: raw control c": {key: "\x03"},
		"success: raw control d": {key: "\x04"},
		"success: panic":         {key: "p", exit: 2, panicText: true},
		"success: term":          {signal: syscall.SIGTERM, exit: 143},
		"success: hup":           {signal: syscall.SIGHUP, exit: 129},
		"success: int":           {signal: syscall.SIGINT, exit: 130},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = master.Close(); _ = slave.Close() }()
			if err := pty.Setsize(master, &pty.Winsize{Rows: 20, Cols: 84}); err != nil {
				t.Fatal(err)
			}
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTerminalProcess$")
			cmd.Env = append(os.Environ(), "AGENTCTL_TEST_TERMINAL_CHILD=1", "TERM=xterm-256color")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() { _ = cmd.Process.Kill() }()
			var output bytes.Buffer
			var waiter tty.Readiness
			read := func() {
				ready, err := waiter.WaitReadable(master, 20*time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				if !ready {
					return
				}
				buf := make([]byte, 8192)
				n, err := master.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				output.Write(buf[:n])
			}
			for !strings.Contains(output.String(), HelpLine) && ctx.Err() == nil {
				read()
			}
			if ctx.Err() != nil {
				t.Fatalf("first frame: %v\n%s", ctx.Err(), output.String())
			}
			if strings.Contains(output.String(), "held log marker") {
				t.Fatal("log escaped onto the active screen")
			}
			raw, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(before, raw) {
				t.Fatal("terminal never entered raw mode")
			}
			if tt.signal != 0 {
				err = cmd.Process.Signal(tt.signal)
			} else {
				_, err = master.Write([]byte(tt.key))
			}
			if err != nil {
				t.Fatal(err)
			}
			var waitErr error
			finished := false
			for !finished && ctx.Err() == nil {
				read()
				select {
				case waitErr = <-done:
					finished = true
				default:
				}
			}
			if !finished {
				t.Fatalf("child did not exit: %s", output.String())
			}
			for {
				ready, err := waiter.WaitReadable(master, 0)
				if err != nil {
					t.Fatal(err)
				}
				if !ready {
					break
				}
				read()
			}
			exit := 0
			if waitErr != nil {
				ee, ok := errors.AsType[*exec.ExitError](waitErr)
				if !ok {
					t.Fatal(waitErr)
				}
				exit = ee.ExitCode()
			}
			if exit != tt.exit {
				t.Fatalf("exit=%d want=%d\n%s", exit, tt.exit, output.String())
			}
			after, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("terminal state was not restored")
			}
			text := output.String()
			leave := strings.Index(text, "\x1b[?1049l")
			logAt := strings.Index(text, "held log marker")
			if leave < 0 || logAt < leave {
				t.Fatalf("log did not follow alternate-screen restore:\n%s", text)
			}
			if tt.panicText && strings.Index(text, "terminal panic marker") < leave {
				t.Fatalf("panic preceded restore:\n%s", text)
			}
		})
	}
}

func TestTerminalRefusesRedirectedStreams(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := Run(t.Context(), fixtureModel(), f, f); err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("error=%v", err)
	}
}
