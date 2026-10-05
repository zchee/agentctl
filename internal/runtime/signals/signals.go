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

// Package signals handles the termination signals.
//
// The process can be holding a namespace lock and a half-written credential
// temporary file when the user hits Ctrl-C, and can have the terminal in raw
// mode on the alternate screen when the watch display is killed. Dying on
// the default disposition would leave all of that behind. So TERM, HUP and
// INT are handled: the first one cancels the command context, takes down
// every registered child process, and runs the cleanup registry; the caller
// then exits with the conventional 128 plus the signal's number.
//
// # Registered children die with the process
//
// A child registered with the controller — the agent a command runs, a
// keychain helper mid-write — is meant to die with this process. The waits
// that own those children do kill them on cancellation, but only on their
// next poll, and an exit racing ahead of that poll would leave a child with
// live credential material reparented to the init daemon. So before the
// cleanup registry runs, the controller ends every registered child itself:
// TERM first, a bounded wait, then KILL for whatever is still there. The
// kill-first order matters: the cleanup registry releases lock directories,
// and a keychain writer still working under one of them must be gone first.
//
// A signal can also land between a child's spawn and its registration.
// Controller.BeginSpawn closes that window: while a spawn is in flight the
// teardown keeps looking for newly registered children, within the same
// budget.
//
// Every signal is sent only after checking that the process id still names
// the process that was registered — same start identity, not yet dead. A
// child that has already exited, or whose id now belongs to some other
// process, is skipped. The residual window is the one any kill by process
// id has, between the check and the send.
package signals

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/proc"
)

const (
	// childPollInterval is how often the teardown re-checks the children it
	// has signalled.
	childPollInterval = 5 * time.Millisecond

	// killSettle is how long the teardown waits for a KILL it sent to land.
	// Short, because nothing can block KILL; it exists so the cleanup
	// registry — which releases lock directories — does not run while a
	// keychain writer this process started is still being torn down.
	killSettle = 50 * time.Millisecond

	// childTermBudget is how long a signalled child gets to exit on TERM
	// before it is killed. It equals the grace a cancelled worker pass
	// gives its children, so a child sees the same bound whichever path
	// tears it down.
	childTermBudget = 500 * time.Millisecond

	// exitDeferralLimit bounds command completion and signal teardown from
	// receipt of the first signal. Both waits share this deadline.
	exitDeferralLimit = 10 * time.Second
)

// ChildToken identifies one registered child so it can be withdrawn again.
type ChildToken uint64

// childEntry is one registered child: a process id and the start identity
// that tells the original process apart from an unrelated one that has
// since been given the same id. The identity is compared, never parsed.
type childEntry struct {
	token    ChildToken
	pid      int
	identity string
}

type termination struct {
	signal   os.Signal
	deadline time.Time
}

// Controller owns the termination-signal disposition for the process.
type Controller struct {
	// base never cancels: the teardown reads the process table after the
	// command context is already cancelled, and those reads must not be
	// refused by the very cancellation that started them.
	base   context.Context
	ctx    context.Context
	cancel context.CancelFunc
	notify chan os.Signal
	fired  atomic.Pointer[termination]
	done   chan struct{}

	spawns atomic.Int64

	mu       sync.Mutex
	nextID   uint64
	children []childEntry

	stopOnce sync.Once
}

// Install registers the TERM, HUP and INT handler and returns the command
// context it will cancel. On the first of those signals the controller
// cancels the context, takes down every registered child, and runs the
// process-wide cleanup registry; the caller observes the signal through
// Fired and must call Wait before exiting so the exit cannot race the
// teardown.
func Install(ctx context.Context) (context.Context, *Controller) {
	cmdCtx, cancel := context.WithCancel(ctx)
	c := &Controller{
		base:   context.WithoutCancel(ctx),
		ctx:    cmdCtx,
		cancel: cancel,
		notify: make(chan os.Signal, 1),
		done:   make(chan struct{}),
	}
	signal.Notify(c.notify, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go c.run()
	return cmdCtx, c
}

// run acts on the first signal only: handling it ends the process, so there
// is no second iteration to write.
func (c *Controller) run() {
	sig, ok := <-c.notify
	if !ok {
		return
	}
	// Record before cancelling, so a caller woken by the cancellation
	// already sees which signal ended the run.
	c.fired.Store(&termination{signal: sig, deadline: time.Now().Add(deferralLimit())})
	c.cancel()
	// Children first, cleanup second: the cleanup registry releases lock
	// directories, and a child still writing under one of them must be
	// gone before the locks are.
	c.terminateChildren()
	cleanup.Run()
	close(c.done)
}

// Fired returns the signal that ended the run, if one has.
func (c *Controller) Fired() (os.Signal, bool) {
	if sig := c.fired.Load(); sig != nil {
		return sig.signal, true
	}
	return nil, false
}

// Wait blocks until the signal teardown — child takedown and the cleanup
// registry — has finished, or the exit deferral expires. The deadline starts
// at the first signal, not when Wait is called. Without a signal it returns
// immediately.
func (c *Controller) Wait() {
	fired := c.fired.Load()
	if fired == nil {
		return
	}
	timer := time.NewTimer(time.Until(fired.deadline))
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
	}
}

// Stop withdraws the signal registration and releases the command context.
// After Stop a termination signal reverts to its default disposition. It
// exists for tests and for callers that finished without a signal; it does
// not undo a teardown already in progress.
func (c *Controller) Stop() {
	c.stopOnce.Do(func() {
		signal.Stop(c.notify)
		close(c.notify)
		c.cancel()
	})
}

// RegisterChild records a child process the teardown must kill, keyed by
// its start identity so a recycled process id is never signalled. It
// reports false — and records nothing — when the identity cannot be read:
// without it a recycled id could not be told apart from the child, and
// killing an unrelated process is worse than leaving this one to the wait
// that owns it.
//
// Withdraw the entry with UnregisterChild once the child has been reaped.
// Until then its process id cannot be reused, because an unreaped child
// still occupies it; after that the start identity is what keeps a
// recycled id safe.
func (c *Controller) RegisterChild(pid int) (ChildToken, bool) {
	current, err := proc.Lookup(c.base, pid)
	if err != nil {
		slog.Debug("a child's start identity is unreadable; a signal will not kill it", "pid", pid, "error", err)
		return 0, false
	}
	identity := current.StartIdentity()
	if identity == "" {
		slog.Debug("a child's start time is unrenderable; a signal will not kill it", "pid", pid)
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	token := ChildToken(c.nextID)
	c.nextID++
	c.children = append(c.children, childEntry{token: token, pid: pid, identity: identity})
	return token, true
}

// UnregisterChild withdraws a previously registered child and reports
// whether an entry was actually removed.
func (c *Controller) UnregisterChild(token ChildToken) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := len(c.children)
	c.children = slices.DeleteFunc(c.children, func(e childEntry) bool { return e.token == token })
	return len(c.children) != before
}

// BeginSpawn opens a spawn window unless the run is already cancelled,
// closing the gap between a spawn and its child's registration in which a
// terminating signal would otherwise find no child to kill. A false return
// means "do not spawn". On true, call release once the child is registered.
//
// The order inside matters: the teardown cancels and then reads the open
// windows, and this opens the window and then reads the cancellation, so
// at least one side sees the other. Either the spawner finds the run
// cancelled and does not spawn, or the teardown finds the window open and
// keeps looking until the child is registered. Checking cancellation first
// would reopen the gap between the check and the increment.
func (c *Controller) BeginSpawn() (release func(), ok bool) {
	c.spawns.Add(1)
	if c.ctx.Err() != nil {
		c.spawns.Add(-1)
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { c.spawns.Add(-1) }) }, true
}

// takeChildren drains the registered children. Draining rather than
// copying, so no child is signalled twice by two overlapping sweeps; a
// child registered after this returns is picked up by the next call, which
// is why the teardown calls it again while it waits.
func (c *Controller) takeChildren() []childEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := c.children
	c.children = nil
	return taken
}

// stillOurs reports whether an entry's process id still names the child
// that was registered, and that child has not yet exited. Both halves read
// the process table afresh; an unreadable record counts as "not ours",
// because a process this one cannot identify is not one it may signal.
func (c *Controller) stillOurs(entry childEntry) bool {
	current, err := proc.Lookup(c.base, entry.pid)
	if err != nil {
		return false
	}
	return current.StartIdentity() == entry.identity && current.Holder != proc.HolderDead
}

// terminateChildren ends every registered child: TERM, a bounded wait that
// keeps collecting newly registered children while any spawn window is
// open, then KILL for whatever is still there, with a short settle so the
// cleanup registry does not run over a child still being torn down.
func (c *Controller) terminateChildren() {
	var pending []childEntry
	deadline := time.Now().Add(childTermBudget)
	for {
		spawning := c.spawns.Load() != 0
		for _, entry := range c.takeChildren() {
			if c.stillOurs(entry) {
				// A failed send means the process is already gone, which
				// is the state this was trying to reach.
				_ = syscall.Kill(entry.pid, syscall.SIGTERM)
				pending = append(pending, entry)
			}
		}
		pending = slices.DeleteFunc(pending, func(e childEntry) bool { return !c.stillOurs(e) })
		if (len(pending) == 0 && !spawning) || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(childPollInterval)
	}

	if len(pending) == 0 {
		return
	}
	for _, entry := range pending {
		if c.stillOurs(entry) {
			_ = syscall.Kill(entry.pid, syscall.SIGKILL)
		}
	}
	settle := time.Now().Add(killSettle)
	for time.Now().Before(settle) {
		pending = slices.DeleteFunc(pending, func(e childEntry) bool { return !c.stillOurs(e) })
		if len(pending) == 0 {
			break
		}
		time.Sleep(childPollInterval)
	}
}

// ChildExitCode translates a finished child's state into the exit status to
// forward: the child's own code when it exited, 128 plus the signal number
// when a signal killed it, and the fatal status 1 when the state carries
// neither — a child this process cannot account for produced nothing
// useful.
func ChildExitCode(state *os.ProcessState) int {
	if state == nil {
		return 1
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return 1
}
