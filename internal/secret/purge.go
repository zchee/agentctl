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

package secret

import "github.com/awnumar/memguard"

// Purge destroys every open plaintext buffer and rotates the session key, so
// no previously created Secret can be decrypted again. It is idempotent and
// safe to call more than once.
//
// It must run on every process exit path — the normal return from the command
// tree, the error return, and the signal path before the deferred exit —
// because locked plaintext pages are otherwise released to the kernel unwiped.
// This package deliberately never calls memguard.CatchInterrupt or
// memguard.CatchSignal: both call signal.Reset, which would silently remove
// the program's own handlers, and both terminate with their own exit status.
// The program's signal handler owns signal disposition and calls Purge itself.
// Code review must keep those two calls out of the whole module; the test for
// this package asserts the default signal disposition survives importing it.
func Purge() {
	memguard.Purge()
}
