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

//go:build agentctl_testing

package signals_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
)

func TestSignalExitDeferral(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		signal      syscall.Signal
		cooperative bool
	}{
		"success: TERM forces exit without purge": {signal: syscall.SIGTERM},
		"success: HUP forces exit without purge":  {signal: syscall.SIGHUP},
		"success: INT forces exit without purge":  {signal: syscall.SIGINT},
		"success: cooperative TERM purges once":   {signal: syscall.SIGTERM, cooperative: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			mode := "forced"
			if tt.cooperative {
				mode = "cooperative"
			}
			events := filepath.Join(t.TempDir(), "events")
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSignalExitProcessHelper$")
			cmd.Env = append(os.Environ(),
				"AGENTCTL_TEST_SIGNAL_HELPER="+mode,
				"AGENTCTL_TEST_SIGNAL_EVENTS="+events,
				"AGENTCTL_TEST_EXIT_DEFERRAL=200ms",
				"GORACE=atexit_sleep_ms=0",
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			ready := make([]byte, len("ready\n"))
			if _, err := io.ReadFull(stdout, ready); err != nil {
				_ = cmd.Wait()
				t.Fatalf("child did not reach its command: %v; stderr: %s", err, stderr.String())
			}
			if string(ready) != "ready\n" {
				t.Fatalf("readiness = %q, want ready", ready)
			}
			if err := cmd.Process.Signal(tt.signal); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			if _, ok := errors.AsType[*exec.ExitError](err); !ok {
				t.Fatalf("Wait = %v, want signalled exit; stderr: %s", err, stderr.String())
			}
			if got, want := cmd.ProcessState.ExitCode(), cli.SignalExitCode(tt.signal); got != want {
				t.Fatalf("exit = %d, want %d; stderr: %s", got, want, stderr.String())
			}
			output, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			wantEvents := "cleanup\n"
			wantWarnings := 1
			if tt.cooperative {
				wantEvents += "purged\n"
				wantWarnings = 0
			}
			if diff := gocmp.Diff(wantEvents, string(output)); diff != "" {
				t.Errorf("exit events (-want +got):\n%s", diff)
			}
			warning := "locked-memory purge was skipped because a command goroutine was still running"
			if got := strings.Count(stderr.String(), warning); got != wantWarnings {
				t.Errorf("warning count = %d, want %d; stderr: %s", got, wantWarnings, stderr.String())
			}
			if !tt.cooperative && strings.Count(stderr.String(), "level=WARN") != 1 {
				t.Errorf("want exactly one slog warning line; stderr: %s", stderr.String())
			}
		})
	}
}

func TestSignalExitProcessHelper(t *testing.T) {
	mode := os.Getenv("AGENTCTL_TEST_SIGNAL_HELPER")
	if mode == "" {
		return
	}
	os.Exit(runSignalExitProcess(t, mode))
}

func runSignalExitProcess(t *testing.T, mode string) int {
	t.Helper()
	if err := secret.EnsureLockedMemoryBudget(); err != nil {
		t.Fatal(err)
	}
	events, err := os.OpenFile(os.Getenv("AGENTCTL_TEST_SIGNAL_EVENTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := events.Close(); err != nil {
			t.Error(err)
		}
	}()
	defer func() {
		secret.Purge()
		if _, err := events.WriteString("purged\n"); err != nil {
			t.Error(err)
		}
	}()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	_, controller := signals.Install(t.Context())
	defer controller.Stop()
	cleanup.Register(func() {
		if _, err := events.WriteString("cleanup\n"); err != nil {
			t.Error(err)
		}
	})
	command := cli.New(cli.Handlers{
		ClaudeStatus: func(ctx context.Context, _ cli.Globals, _ cli.ClaudeStatusOptions) error {
			if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
				return err
			}
			if mode == "cooperative" {
				<-ctx.Done()
				return ctx.Err()
			}
			// A real child-free handler that never observes cancellation.
			select {}
		},
	})
	command.Root().SetArgs([]string{"claude", "status"})
	_ = controller.Execute(command.Root().ExecuteContext, func(sig os.Signal) {
		os.Exit(cli.SignalExitCode(sig))
	})
	if sig, ok := controller.Fired(); ok {
		controller.Wait()
		return cli.SignalExitCode(sig)
	}
	return 1
}
