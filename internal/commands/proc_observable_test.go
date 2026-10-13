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
	"errors"
	"os"
	"testing"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

// processesObservable reports whether this platform can look up a running
// process, by looking up the test binary itself. Off macOS proc.Lookup
// refuses with *proc.UnsupportedPlatformError, so a lock holder that is
// running is still labelled dead there, and a test must not expect
// "(alive)". Any other lookup failure of this very process is a fault, not
// a platform gap, and fails the test.
func processesObservable(t *testing.T) bool {
	t.Helper()
	_, err := proc.Lookup(t.Context(), os.Getpid())
	if unsupported, ok := errors.AsType[*proc.UnsupportedPlatformError](err); ok {
		t.Logf("%v: proc.Lookup refuses, so the lock holder's liveness is not asserted", unsupported)
		return false
	}
	if err != nil {
		t.Fatalf("proc.Lookup(%d) of the test process: %v", os.Getpid(), err)
	}
	return true
}
