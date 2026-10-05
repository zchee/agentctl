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

// Package coordinator runs one bounded fan-out over the accounts and owns
// every child process that fan-out spawns.
//
// A pass is one sweep over the accounts. RunPass fans the jobs out across a
// bounded set of workers and streams each result back over a channel as it
// lands, so a caller can render early rows while later ones are still in
// flight.
//
// The reason this is a package rather than a handful of go statements is
// ownership of child processes. Workers shell out to the keychain helper,
// which can block indefinitely on a locked keychain. If each worker owned
// its own child, a cancelled pass would have no way to reach in and kill
// it, and the process would hang on exit holding a keychain prompt open. So
// the split is: the child table owns every started process and is reachable
// from the pass watchdog, while the job observes its child only through
// PassCtx.WaitChild and PassCtx.WaitChildTimeout, which poll so the
// watchdog can always take the table.
//
// On cancellation or deadline the watchdog kills every live child, which
// unblocks the waits within one poll interval; the workers then finish,
// the pass joins them, and the cleanup registry runs. Not every child has
// a watchdog behind it — Standalone has none — so both waits also treat
// cancellation as their own deadline and reap what they were waiting on
// themselves.
//
// Each registered child is also handed to the termination-signal
// controller, so a signal exit kills it before the process is gone; the
// entry is withdrawn when the child is reaped.
package coordinator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/signals"
)

const (
	// DefaultMaxWorkers is the most workers a single pass runs
	// concurrently.
	DefaultMaxWorkers = 4

	// WorkerJoinBudget is how long the pass waits for workers to drain
	// after killing their children before it stops waiting and runs the
	// cleanup registry anyway.
	WorkerJoinBudget = 500 * time.Millisecond

	// childPollInterval is how often a wait re-checks a child.
	childPollInterval = 10 * time.Millisecond

	// watchdogPollInterval is how often the watchdog re-checks the
	// deadline and the cancellation.
	watchdogPollInterval = 25 * time.Millisecond
)

// ErrChildKilled reports that the pass killed a child out from under the
// job — the watchdog took it on cancellation or deadline — which is how a
// job learns the pass is over.
var ErrChildKilled = errors.New("the pass coordinator killed this child process")

// ErrPassCancelled reports that the pass was cancelled while the job was
// waiting on its child; the child was killed and reaped before the wait
// returned, so the caller sees the same outcome the watchdog would have
// produced.
var ErrPassCancelled = errors.New("the pass was cancelled while this child process was running")

// Job is one unit of work in a pass.
type Job[T any] func(pass *PassCtx) T

// ChildToken identifies a child process registered with the pass.
type ChildToken uint64

// registered is one child the pass owns: the started command, the reap
// notification, and the child's entry with the signal controller.
type registered struct {
	cmd *exec.Cmd
	// done closes after the reaper's Wait returned; cmd.ProcessState and
	// waitErr are readable only after it.
	done     chan struct{}
	waitErr  error
	sigToken signals.ChildToken
	sigOK    bool
}

// childTable owns every child the pass has spawned and not yet handed back
// or killed. All access goes through the mutex so the watchdog can always
// take it.
type childTable struct {
	mu     sync.Mutex
	nextID uint64
	live   map[ChildToken]*registered
}

// PassCtx is what a job is given so it can cooperate with cancellation and
// hand its child processes to the pass.
//
// The context is shared and deliberate: every holder observes the same
// cancellation and the same child table, so a long-lived helper a job
// builds for itself can carry the PassCtx and still have its children
// killed by the watchdog.
type PassCtx struct {
	ctx        context.Context
	deadline   time.Time
	controller *signals.Controller
	children   *childTable
}

// Standalone builds a context that belongs to no pass.
//
// Login-style commands and tests need a context to spawn the keychain
// helper through without a pass running behind them. Nothing watches the
// returned context's children: a caller must reach every child it
// registers through WaitChild or WaitChildTimeout, both of which reap what
// they wait on — and both of which treat cancellation as a deadline, so a
// context with no watchdog behind it still lets go of its children when
// the run is cancelled. controller may be nil, in which case children are
// not reachable from a signal exit either.
func Standalone(ctx context.Context, controller *signals.Controller, deadline time.Time) *PassCtx {
	return &PassCtx{
		ctx:        ctx,
		deadline:   deadline,
		controller: controller,
		children:   &childTable{live: make(map[ChildToken]*registered)},
	}
}

// Context returns the pass-wide cancellation context.
func (p *PassCtx) Context() context.Context {
	return p.ctx
}

// Deadline returns the instant after which the pass must stop doing new
// work.
func (p *PassCtx) Deadline() time.Time {
	return p.deadline
}

// Remaining returns how long is left before the deadline, saturating at
// zero.
func (p *PassCtx) Remaining() time.Duration {
	return max(time.Until(p.deadline), 0)
}

// ShouldStop reports whether the job should stop, because the pass was
// cancelled or the deadline has passed.
func (p *PassCtx) ShouldStop() bool {
	return p.ctx.Err() != nil || !time.Now().Before(p.deadline)
}

// BeginSpawn opens a spawn window with the signal controller unless the
// pass is already cancelled, so a terminating signal keeps looking for a
// child that is between its spawn and its registration. Without a
// controller only the cancellation check remains.
func (p *PassCtx) BeginSpawn() (release func(), ok bool) {
	if p.controller != nil {
		return p.controller.BeginSpawn()
	}
	if p.ctx.Err() != nil {
		return nil, false
	}
	return func() {}, true
}

// RegisterChild hands a freshly started child process to the pass.
//
// From here on the pass owns the process: it may kill it at any moment,
// the job's only sanctioned interactions are WaitChild and
// WaitChildTimeout, and nothing else may call the command's own Wait —
// a second reap of the same process is a programming error. A job that
// needs the child's output wires an io.Writer into the command before
// starting it, never a pipe file it reads after the child can exit,
// because the reap closes pipe files.
//
// The child is also recorded with the termination-signal controller, so a
// signal exit kills it before the process is gone; a child whose start
// identity cannot be read is not recorded there, and the waits above
// remain its only bound. A child registered after the pass has stopped is
// killed on arrival rather than left to a watchdog sweep that has already
// happened.
func (p *PassCtx) RegisterChild(cmd *exec.Cmd) ChildToken {
	entry := &registered{cmd: cmd, done: make(chan struct{})}
	if p.controller != nil {
		entry.sigToken, entry.sigOK = p.controller.RegisterChild(cmd.Process.Pid)
	}

	p.children.mu.Lock()
	token := ChildToken(p.children.nextID)
	p.children.nextID++
	p.children.live[token] = entry
	p.children.mu.Unlock()

	// The reaper is the one caller of Wait, so the reap happens exactly
	// once however many paths asked for the kill. The signal entry is
	// withdrawn only after the reap: until then the process id is still
	// this child's, because an unreaped child keeps it.
	go func() {
		entry.waitErr = cmd.Wait()
		if entry.sigOK && p.controller != nil {
			p.controller.UnregisterChild(entry.sigToken)
		}
		close(entry.done)
	}()

	// After the insert, not before: a watchdog sweep between the check
	// and the insert would miss the child, where a kill after the insert
	// is just the sweep this check stands in for.
	if p.ShouldStop() {
		_ = cmd.Process.Kill()
	}
	return token
}

// reaped reports whether the entry's reaper has finished, without
// blocking.
func (e *registered) reaped() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// settle blocks until the entry's reaper has finished, so a caller that
// killed the child hands back a reaped process, never a zombie.
func (e *registered) settle() (*os.ProcessState, error) {
	<-e.done
	return e.cmd.ProcessState, e.waitErr
}

// finish translates a reaped child's wait result for the caller: an exit
// with a nonzero code is an answer, not a wait failure, so it travels in
// the state rather than in the error.
func finish(state *os.ProcessState, waitErr error) (*os.ProcessState, error) {
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		return exitErr.ProcessState, nil
	}
	return state, waitErr
}

// WaitChild waits for a registered child to exit, polling so the watchdog
// can always take the child table.
//
// Cancellation ends the wait: this has no budget of its own, so without
// that clause a job waiting here would hold a child open for as long as
// the child felt like living, watchdog or no watchdog. The child is killed
// and reaped before returning, exactly as the watchdog would have done.
//
// Returns ErrChildKilled when the pass killed the child out from under the
// job, ErrPassCancelled when this call killed it because the pass was
// cancelled, and otherwise the reap's own failure, if any.
func (p *PassCtx) WaitChild(token ChildToken) (*os.ProcessState, error) {
	for {
		p.children.mu.Lock()
		entry, ok := p.children.live[token]
		if !ok {
			p.children.mu.Unlock()
			return nil, ErrChildKilled
		}
		if entry.reaped() {
			delete(p.children.live, token)
			p.children.mu.Unlock()
			return finish(entry.cmd.ProcessState, entry.waitErr)
		}
		// After the harvest above, so a child that exited on its own in
		// the same instant is still reported as having exited rather
		// than as having been killed.
		if p.ctx.Err() != nil {
			delete(p.children.live, token)
			p.children.mu.Unlock()
			_ = entry.cmd.Process.Kill()
			_, _ = entry.settle()
			return nil, ErrPassCancelled
		}
		p.children.mu.Unlock()
		time.Sleep(childPollInterval)
	}
}

// WaitChildTimeout waits for a registered child to exit, giving up after
// timeout — or as soon as the pass is cancelled, whichever comes first.
//
// On either kind of giving up the child is killed and reaped and a nil
// state is returned with a nil error, which is what bounds the keychain
// helper's budgets: a prompt that never gets an answer cannot hold the
// pass open. The two reasons are deliberately not distinguished: every
// caller treats "no answer" one way. Returns ErrChildKilled when the pass
// killed the child first.
func (p *PassCtx) WaitChildTimeout(token ChildToken, timeout time.Duration) (*os.ProcessState, error) {
	deadline := time.Now().Add(timeout)
	for {
		p.children.mu.Lock()
		entry, ok := p.children.live[token]
		if !ok {
			p.children.mu.Unlock()
			return nil, ErrChildKilled
		}
		if entry.reaped() {
			delete(p.children.live, token)
			p.children.mu.Unlock()
			return finish(entry.cmd.ProcessState, entry.waitErr)
		}
		if p.ctx.Err() != nil || !time.Now().Before(deadline) {
			delete(p.children.live, token)
			p.children.mu.Unlock()
			_ = entry.cmd.Process.Kill()
			_, _ = entry.settle()
			return nil, nil
		}
		p.children.mu.Unlock()
		time.Sleep(childPollInterval)
	}
}

// killAll kills every live child and waits for each reap, so no caller
// after it can see a zombie the pass still owned. Both the kill and the
// reap are allowed to have already happened: a child that exited on its
// own is the state this was trying to reach.
func (t *childTable) killAll() {
	t.mu.Lock()
	taken := t.live
	t.live = make(map[ChildToken]*registered)
	t.mu.Unlock()
	for _, entry := range taken {
		_ = entry.cmd.Process.Kill()
	}
	for _, entry := range taken {
		_, _ = entry.settle()
	}
}

// RunPass runs jobs on a detached coordinator goroutine and streams their
// results. controller may be nil, in which case a signal exit cannot reach
// the pass's children.
//
// At most maxWorkers jobs run at once (clamped to at least one). Results
// arrive on the returned channel in completion order, not submission
// order. The channel closes when the last worker has finished, which is
// the caller's signal that the pass is over — successfully or not.
//
// The caller must drain the channel or cancel the context: the pass
// bounds how long it waits for a cooperative worker, but a result nobody
// reads from an uncancelled pass blocks its worker, and a job that
// ignores ShouldStop and blocks forever cannot be forced to return.
func RunPass[T any](ctx context.Context, controller *signals.Controller, jobs []Job[T], deadline time.Time, maxWorkers int) <-chan T {
	results := make(chan T)
	go coordinate(ctx, controller, jobs, deadline, maxWorkers, results)
	return results
}

// coordinate is the body of the pass goroutine.
func coordinate[T any](ctx context.Context, controller *signals.Controller, jobs []Job[T], deadline time.Time, maxWorkers int, results chan<- T) {
	defer close(results)
	if len(jobs) == 0 {
		return
	}

	pass := Standalone(ctx, controller, deadline)
	queue := make(chan Job[T], len(jobs))
	for _, job := range jobs {
		queue <- job
	}
	close(queue)

	workerCount := min(max(maxWorkers, 1), len(jobs))
	var workersRunning atomic.Bool
	workersRunning.Store(true)

	var watchdogDone sync.WaitGroup
	watchdogDone.Go(func() {
		watchdog(ctx, deadline, pass.children, &workersRunning)
	})

	var workers sync.WaitGroup
	for range workerCount {
		workers.Go(func() {
			for job := range queue {
				if pass.ShouldStop() {
					return
				}
				select {
				case results <- job(pass):
				case <-ctx.Done():
					// The caller gave the pass up; nothing downstream
					// will read further results, so stop early.
					return
				}
			}
		})
	}
	workers.Wait()
	workersRunning.Store(false)
	watchdogDone.Wait()

	// The watchdog only kills children on the abnormal path, so a pass
	// that finished normally can still be holding a child whose job
	// leaked it.
	pass.children.killAll()

	if ctx.Err() != nil || !time.Now().Before(deadline) {
		cleanup.Run()
	}
}

// watchdog watches for cancellation or the deadline and kills every live
// child when either fires. It returns as soon as the workers are done on
// the normal path, so the pass is not held open by the watchdog itself.
func watchdog(ctx context.Context, deadline time.Time, children *childTable, workersRunning *atomic.Bool) {
	var killDeadline time.Time
	for {
		if !workersRunning.Load() {
			return
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			killDeadline = time.Now().Add(WorkerJoinBudget)
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(watchdogPollInterval):
		}
	}

	children.killAll()

	// Give the workers their bounded window to notice their children are
	// gone and return. Anything still running after this is a job that
	// ignored ShouldStop; the join will wait for it, but nothing further
	// is gained by this goroutine staying to watch. A job may also have
	// spawned another child after the first sweep, so the sweep repeats.
	for workersRunning.Load() && time.Now().Before(killDeadline) {
		time.Sleep(childPollInterval)
		children.killAll()
	}
}
