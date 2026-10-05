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

// Command agentctl manages accounts and credentials for AI coding agents.
//
// This binary is deliberately thin: it brings up logging, installs signal
// handling, executes the command tree and turns the result into a process
// exit status. Everything else lives in the internal packages.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zchee/agentctl/internal/app"
	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/logbuf"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
)

func main() {
	os.Exit(run())
}

// run executes the command tree and returns the process exit status. Normal
// and cooperative signal exits return through the single deferred purge;
// the controller force-exits without purging if the command stays active.
func run() int {
	initLogging()

	// Refuse to start under a locked-memory limit the secret store cannot
	// live within: an allocation failure later would be a deadlock in the
	// middle of credential handling, where this is a one-line refusal
	// before anything was read.
	if err := secret.EnsureLockedMemoryBudget(); err != nil {
		fmt.Fprintf(os.Stderr, "agentctl: %v\n", err)
		return errs.ExitFatal
	}
	// Purge only after the command returns and its plaintext users have
	// stopped. The forced signal exit bypasses defers rather than destroy
	// memory a cancellation-ignoring command could still be reading.
	defer secret.Purge()

	// A write to a closed stdout must come back as an error the writer
	// can classify — a completion script piped into a pager that quits is
	// a normal run — rather than kill the process outright.
	signal.Ignore(syscall.SIGPIPE)

	// The controller owns the TERM/HUP/INT disposition: it cancels the
	// command context, takes down registered children, and runs the
	// cleanup registry. The exit status depends on which signal fired
	// (143 for TERM, 129 for HUP, 130 for INT), so the controller records
	// the signal and the mapping below stays the one source of the code.
	_, controller := signals.Install(context.Background())
	defer controller.Stop()

	c := cli.New(app.Handlers(app.Dependencies{Stdout: os.Stdout}))
	err := controller.Execute(c.Root().ExecuteContext, func(sig os.Signal) {
		os.Exit(cli.SignalExitCode(sig))
	})

	// A run that was cancelled by a signal exits with the signal's own
	// status, whatever the command returned on its way out. Join teardown
	// before purging, bounded by the deadline established at the signal.
	if sig, ok := controller.Fired(); ok {
		controller.Wait()
		return cli.SignalExitCode(sig)
	}
	if err == nil {
		return errs.ExitOK
	}

	fmt.Fprintf(os.Stderr, "agentctl: %v\n", err)
	if !c.Dispatched() {
		// The command line itself was refused, which is the usage-error
		// contract: exit 2, no stack, nothing ran.
		return errs.ExitPartial
	}
	return errs.ExitCode(err)
}

// initLogging brings up a text slog handler on stderr, filtered by
// AGENTCTL_LOG.
//
// stderr, not stdout: the JSON-emitting commands write a machine-readable
// document to stdout and log lines interleaved into it would corrupt it.
// The writer is terminal-aware: while the watch display holds the
// terminal, log lines are buffered and flushed to stderr after the
// terminal is restored, instead of being painted over inside a frame.
func initLogging() {
	handler := slog.NewTextHandler(logbuf.Writer{}, &slog.HandlerOptions{Level: cli.LogLevel(os.Getenv("AGENTCTL_LOG"))})
	slog.SetDefault(slog.New(handler))
}
