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
	"sync/atomic"
	"syscall"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
)

func main() {
	os.Exit(run())
}

// run executes the command tree and returns the process exit status. It is
// separate from main so every exit flows through one return path instead
// of scattered os.Exit calls.
func run() int {
	initLogging()

	// A write to a closed stdout must come back as an error the writer
	// can classify — a completion script piped into a pager that quits is
	// a normal run — rather than kill the process outright.
	signal.Ignore(syscall.SIGPIPE)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The exit status depends on which signal fired (143 for TERM, 129
	// for HUP, 130 for INT), and a NotifyContext cannot say which one it
	// saw, so the signal is recorded here and the context cancelled by
	// hand.
	var fired atomic.Int32
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		sig, ok := <-signals
		if !ok {
			return
		}
		if number, isPosix := sig.(syscall.Signal); isPosix {
			fired.Store(int32(number))
		}
		cancel()
	}()

	c := cli.New(cli.Handlers{})
	err := c.Root().ExecuteContext(ctx)

	// A run that was cancelled by a signal exits with the signal's own
	// status, whatever the command returned on its way out.
	if number := fired.Load(); number != 0 {
		return 128 + int(number)
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
func initLogging() {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cli.LogLevel(os.Getenv("AGENTCTL_LOG"))})
	slog.SetDefault(slog.New(handler))
}
