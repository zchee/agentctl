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

package signals

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Execute runs command synchronously with the controller's context. If a
// signal arrives and command does not return within the exit deferral,
// Execute calls forceExit on a separate goroutine after logging one warning.
// forceExit must terminate the process without unwinding defers, using the
// supplied signal's exit status. The caller still calls Wait after a
// cooperative return, before purging locked memory.
//
// Purge must not run while a plaintext window is open, so an uncooperative
// command cannot safely unwind through the caller's deferred purge. Enclave
// contents are ciphertext, and process exit returns their pages to the
// kernel. A cooperative command instead returns through the single deferred
// purge. The deferral starts when the first signal arrives, sharing Wait's
// deadline rather than adding another grace period.
func (c *Controller) Execute(command func(context.Context) error, forceExit func(os.Signal)) error {
	returned := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-returned:
			return
		case <-c.ctx.Done():
		}
		fired := c.fired.Load()
		if fired == nil {
			return
		}
		timer := time.NewTimer(time.Until(fired.deadline))
		defer timer.Stop()
		select {
		case <-returned:
			return
		case <-timer.C:
		}
		// Prefer an observed return when completion and expiry are ready
		// together, so a cooperative command retains its deferred purge.
		select {
		case <-returned:
			return
		default:
		}
		slog.Warn("locked-memory purge was skipped because a command goroutine was still running")
		forceExit(fired.signal)
	}()
	defer func() {
		close(returned)
		<-watcherDone
	}()
	return command(c.ctx)
}
