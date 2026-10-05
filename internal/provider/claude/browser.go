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

package claude

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"
)

// OpenBrowser starts the desktop opener without waiting for the browser to exit.
// Failure is optional: the caller must print the authorization URL first.
func OpenBrowser(ctx context.Context, url string) {
	if suppressBrowser() {
		return
	}
	program := "open"
	if runtime.GOOS != "darwin" {
		program = "xdg-open"
	}
	if err := launchBrowser(ctx, program, url); err != nil {
		slog.Debug("could not open a browser; the URL was printed instead")
	}
}

func launchBrowser(ctx context.Context, program, url string) error {
	cmd := exec.CommandContext(ctx, program, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
