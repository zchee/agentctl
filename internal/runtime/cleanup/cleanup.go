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

// Package cleanup keeps a process-wide list of actions that must be undone
// when the process stops abruptly.
//
// A credential write can be holding a half-written temporary file, and the
// watch display can be holding the terminal in raw mode on the alternate
// screen, at the moment a terminating signal arrives. Dying without undoing
// either leaves token material at rest or an unusable shell behind. Each
// such action registers an undo function here; the signal path runs them all
// once on its way out.
//
// Entries run in reverse registration order, because later registrations
// depend on earlier state the way deferred calls do: a terminal restore
// registered after a temporary file must not run before that file is gone,
// and undoing in acquisition order would reverse that dependency.
//
// Failures inside an entry are deliberately contained. The run happens on
// the way out of the process with no caller left to handle an error, so a
// panicking entry is recovered and the remaining entries still run; skipping
// them because one failed would leave exactly the state this registry exists
// to remove.
package cleanup

import (
	"log/slog"
	"slices"
	"sync"
)

// Token identifies one registered entry so it can be withdrawn again.
type Token uint64

// entry pairs a registered function with the token that withdraws it.
type entry struct {
	token Token
	fn    func()
}

// Registry is an ordered set of undo functions. The zero value is ready to
// use. Every method is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	nextID  uint64
	entries []entry
}

// defaultRegistry is the process-wide registry the package-level functions
// operate on. Process-wide because the signal path that runs it has no
// handle to pass around: a terminating signal must find every entry,
// whichever package registered it.
var defaultRegistry Registry

// Register adds fn to the registry and returns the token that withdraws it.
// fn runs at most once, on the first Run after registration.
//
// Register panics on a nil fn: recovering it later inside Run would hide the
// programming error until the one moment the entry was needed.
func (r *Registry) Register(fn func()) Token {
	if fn == nil {
		panic("cleanup: Register requires a non-nil function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Wrapping is unreachable in practice and harmless if reached: tokens
	// are only ever compared for equality against live entries.
	token := Token(r.nextID)
	r.nextID++
	r.entries = append(r.entries, entry{token: token, fn: fn})
	return token
}

// Unregister withdraws a previously registered entry and reports whether an
// entry was actually removed, which lets a caller notice a double
// withdrawal in tests.
//
// Withdraw an entry as soon as its action has been completed or undone by
// the normal path, so a later Run cannot act on state some other part of
// the program has since rebuilt.
func (r *Registry) Unregister(token Token) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := len(r.entries)
	r.entries = slices.DeleteFunc(r.entries, func(e entry) bool { return e.token == token })
	return len(r.entries) != before
}

// Run executes every registered entry once, newest first, and empties the
// registry.
//
// Entries are taken out under the lock before any of them runs, so the lock
// is never held across a callback — a signal goroutine calling this cannot
// deadlock behind a slow entry — and a concurrent or subsequent Run finds
// nothing left to do rather than running an entry twice.
func (r *Registry) Run() {
	r.mu.Lock()
	taken := r.entries
	r.entries = nil
	r.mu.Unlock()
	for _, e := range slices.Backward(taken) {
		runRecovering(e.fn)
	}
}

// runRecovering runs one entry, containing a panic so the entries after it
// still run.
func runRecovering(fn func()) {
	defer func() {
		if v := recover(); v != nil {
			// Debug rather than error: this runs on the way out, often from
			// the signal path, and the remaining entries completing is the
			// outcome that matters.
			slog.Debug("a cleanup entry panicked; the remaining entries still run", "panic", v)
		}
	}()
	fn()
}

// Register adds fn to the process-wide registry. See Registry.Register.
func Register(fn func()) Token {
	return defaultRegistry.Register(fn)
}

// Unregister withdraws an entry from the process-wide registry. See
// Registry.Unregister.
func Unregister(token Token) bool {
	return defaultRegistry.Unregister(token)
}

// Run executes and empties the process-wide registry. See Registry.Run.
func Run() {
	defaultRegistry.Run()
}
