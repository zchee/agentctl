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

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/coordinator"
	"github.com/zchee/agentctl/internal/runtime/signals"
)

// KeychainWriteTimeout bounds the child while the caller holds the peer locks.
const KeychainWriteTimeout = 1200 * time.Millisecond

// KeychainWriteWaitDelay is how long a write may still take after its
// timeout to reap the killed child, so a caller's hold budget can count the
// whole write.
const KeychainWriteWaitDelay = childWaitDelay

// KeychainVerifyTimeout bounds the under-lock read before a keychain write.
const KeychainVerifyTimeout = 800 * time.Millisecond

// writeSubcommand, updateFlag and payloadFlag complete the security(1) write
// vocabulary beside the read constants: `add-generic-password -U -a
// <account> -s <service> -X <hex>`, the command and arguments the vendor
// tooling writes its own item with. No keychain argument is passed, as the
// vendor tooling passes none, and no -T, -A or -l: an update never changes
// an item's access control.
const (
	writeSubcommand = "add-generic-password"
	updateFlag      = "-U"
	payloadFlag     = "-X"
)

// stderrCapture bounds how much of a write child's stderr is kept for
// classification. The classifier matches short fixed phrases, so anything
// past this is noise.
const stderrCapture = 64 << 10

// stderrDrain sizes the scratch the rest of a child's stderr is read
// through, and discarded from, once the capture is full.
const stderrDrain = 512

// The three kinds of keychain write failure, as errors.Is targets. Each
// failed write matches exactly one of them, beside the class sentinel its
// class names.
var (
	// ErrKeychainNotStarted matches a write whose security(1) never ran: the
	// start failed, the host refused the argument list (E2BIG, which also
	// matches [ErrCredentialTooLarge]), or a shutdown was already in
	// progress. The item is unchanged.
	ErrKeychainNotStarted = &KeychainError{kind: writeNotStarted}
	// ErrKeychainOutcomeUnknown matches a write whose security(1) ran and
	// may or may not have committed: a timeout, a cancellation, a child
	// ended by a signal, or an exit whose output was not released or could
	// not be collected. Only a readback settles it.
	ErrKeychainOutcomeUnknown = &KeychainError{kind: writeOutcomeUnknown}
	// ErrKeychainWriteFailed matches a write whose security(1) exited on its
	// own with a non-zero status while the run was live.
	ErrKeychainWriteFailed = &KeychainError{kind: writeFailed}
	// ErrKeychainShutdown matches the not-started write the signal
	// controller refused because a shutdown was already in progress. It
	// narrows [ErrKeychainNotStarted]; it is not a fourth kind.
	ErrKeychainShutdown = &KeychainError{kind: writeNotStarted, shutdown: true}
)

// KeychainTargetMismatchError reports an update built for a different item.
// The request is refused before a child is started.
type KeychainTargetMismatchError struct {
	Expected string
	Found    string
}

// Error names the mismatched account or service, never the credential.
func (e *KeychainTargetMismatchError) Error() string {
	return fmt.Sprintf("this keychain update was built for `%s`, not for `%s`", e.Found, e.Expected)
}

// KeychainWriter updates items through one bounded security(1) child per
// write, with the payload in argv and nothing on stdin.
//
// Callers must derive the service from an owned namespace or the validated
// live environment and hold the corresponding peer locks before calling
// Write. The zero value refuses every write.
type KeychainWriter struct {
	bin     string
	account string
	env     []string
	budget  time.Duration
	// controller is the termination-signal controller the child is
	// registered with, so a signal exit terminates it before the process is
	// gone.
	controller *signals.Controller
	// captureHook, when set, is handed each stderr capture as it is made,
	// so a test can check that the capture's memory is zeroed after Write.
	captureHook func(*boundedBuffer)
}

// NewKeychainWriter selects the fixed production binary or the tagged test
// seam, and registers every child it starts with controller. Construction
// does not start a process. A testing build without a stand-in refuses
// writes rather than falling back to the user's keychain, and a nil
// controller refuses every write with [ErrKeychainUnsupported]: a child the
// signal path cannot see could outlive the locks its caller holds.
func NewKeychainWriter(controller *signals.Controller) *KeychainWriter {
	reader, ok := newReader().(*securityCLI)
	if !ok {
		return &KeychainWriter{account: CurrentAccount(), controller: controller}
	}
	return &KeychainWriter{bin: reader.bin, account: reader.account, env: reader.env, budget: KeychainWriteTimeout, controller: controller}
}

// Write updates service in place with update, creating the item when absent.
//
// Account, service and size are checked before anything is spawned. Write
// does not acquire locks, derive write authority, retry failures or remove
// an existing item. It consumes update: the payload is forgotten when Write
// returns, whatever the outcome.
//
// A failure is a [KeychainError] matching exactly one of
// [ErrKeychainNotStarted], [ErrKeychainOutcomeUnknown] and
// [ErrKeychainWriteFailed]; the refusals before any spawn (an empty update,
// a target mismatch, an unsupported writer) match none of them. Neither
// security(1)'s stdout nor its stderr text reaches the error: the detail
// carries the exit status and the stderr class only.
//
// The plaintext hex exists only in a locked buffer opened for the write, and
// in the argument copies the runtime makes for the child, which cannot be
// wiped; the buffer is destroyed after the child has been reaped, never while
// it runs.
func (w *KeychainWriter) Write(ctx context.Context, service string, update *KeychainUpdate) error {
	if update == nil || update.hex == nil || update.size <= 0 {
		return errors.New("the keychain update is empty")
	}
	defer update.drop()
	if update.Account() != w.account {
		return &KeychainTargetMismatchError{Expected: w.account, Found: update.Account()}
	}
	if update.Service() != service {
		return &KeychainTargetMismatchError{Expected: service, Found: update.Service()}
	}
	if update.Len() > 2*KeychainBlobLimit {
		return &BlobTooLargeError{Len: update.Len() / 2}
	}
	if w.bin == "" || w.controller == nil {
		return ErrKeychainUnsupported
	}

	var result error
	if err := update.hex.WithPlaintext(func(payload []byte) error {
		result = w.spawn(ctx, update, payload)
		return nil
	}); err != nil {
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: "the keychain update could not be opened", kind: writeNotStarted}
	}
	return result
}

// spawn runs one security(1) child for the update whose hex is payload, and
// returns once that child has been reaped.
func (w *KeychainWriter) spawn(ctx context.Context, update *KeychainUpdate, payload []byte) error {
	// The pass deadline and the wait bound are the same figure, so the
	// kill-on-arrival check in RegisterChild and the wait agree on how long
	// the child may run.
	pass := coordinator.Standalone(ctx, w.controller, time.Now().Add(w.budget))
	release, ok := pass.BeginSpawn()
	if !ok {
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: "a shutdown is in progress; no keychain update was issued", Transient: true, kind: writeNotStarted, shutdown: true}
	}

	// No CommandContext: the pass owns the kill, and a second killer would
	// race its reap.
	cmd := exec.Command(w.bin, writeSubcommand, updateFlag, accountFlag, update.Account(), serviceFlag, update.Service(), payloadFlag, string(payload))
	cmd.Env = w.env
	cmd.Stdout = io.Discard
	stderr := newBoundedBuffer()
	if w.captureHook != nil {
		w.captureHook(stderr)
	}
	cmd.Stderr = stderr
	cmd.WaitDelay = childWaitDelay
	// Its own process group, so a terminal's Ctrl-C reaches agentctl, which
	// decides, rather than security(1) directly; and so the group can be
	// killed as a whole after the leader is reaped.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		release()
		if errors.Is(err, syscall.E2BIG) {
			return &KeychainError{Class: errs.KeychainCredentialTooLarge, Detail: "the host refused the argument list (E2BIG); security was not started; the keychain item is unchanged", kind: writeNotStarted}
		}
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: fmt.Sprintf("security could not be started: %s: %v", w.bin, err), kind: writeNotStarted}
	}
	token := pass.RegisterChild(cmd)
	release()
	pgid := cmd.Process.Pid
	state, waitErr := pass.WaitChildTimeout(token, w.budget)
	defer stderr.wipe()

	// Read after the wait: a signal that arrived during the write is the
	// one that cancelled it.
	fired, _ := w.controller.Fired()
	if err := classifyWrite(ctx, fired, state, waitErr, w.budget, stderr); err != nil {
		if errors.Is(err, ErrKeychainOutcomeUnknown) {
			// The leader is reaped by now. Whatever it left in its group
			// must not outlive Write; an empty group answers ESRCH, which
			// is the state this was trying to reach.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		return err
	}
	return nil
}

// classifyWrite turns a reaped write child's wait result into the write's
// outcome. The order is load-bearing: a cancelled run is a cancellation
// whatever the wait handed back, because after a signal the wait usually
// returns the leader's reaped, signal-ended state rather than nothing.
//
// fired is the termination signal the run's controller received, if any. A
// cancelled run records it as the error's signal; a child ended by a signal
// of its own records that one. A budget expiry, a cancellation no signal
// caused, and an output held past the wait delay record none: the kill that
// ended those children was this process's own.
func classifyWrite(ctx context.Context, fired os.Signal, state *os.ProcessState, waitErr error, budget time.Duration, stderr *boundedBuffer) error {
	switch {
	case ctx.Err() != nil:
		return &KeychainError{Class: errs.KeychainTimeout, Detail: "the run was cancelled while security was running", Transient: true, kind: writeOutcomeUnknown, signal: signalName(fired)}
	case state == nil && waitErr == nil:
		// The wait gave up at its bound and killed and reaped the child.
		return &KeychainError{Class: errs.KeychainTimeout, Detail: fmt.Sprintf("security timed out after %d ms", budget.Milliseconds()), Transient: true, kind: writeOutcomeUnknown}
	case errors.Is(waitErr, exec.ErrWaitDelay):
		// The child exited, but something still held its output past the
		// wait delay, so its exit status is not the whole story.
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: "security exited but did not release its output", Transient: true, kind: writeOutcomeUnknown}
	case waitErr != nil:
		// The reap's own failure. coordinator.ErrChildKilled lands here in
		// principle, but cannot occur: a Standalone pass has no watchdog
		// and no sweep that kills its children, and this writer is the
		// child's only waiter.
		return &KeychainError{Class: StderrOther.KeychainClass(), Detail: "security output could not be collected", Transient: true, kind: writeOutcomeUnknown}
	case !state.Exited():
		// A child reaped after a signal reports exit code -1; it may have
		// committed first, so the readback decides the outcome.
		var ended os.Signal
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			ended = status.Signal()
		}
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: "security was ended by a signal", Transient: true, kind: writeOutcomeUnknown, signal: signalName(ended)}
	}

	code := state.ExitCode()
	if code == 0 {
		return nil
	}
	class := stderr.classify()
	detail := fmt.Sprintf("security exited %d (stderr class %s)", code, class)
	switch code {
	case exitNotFound:
		// Unlike a read, an upsert cannot legitimately answer absent.
		return &KeychainError{Class: errs.KeychainNotFound, Detail: detail, Transient: true, kind: writeFailed}
	case exitLocked:
		return &KeychainError{Class: errs.KeychainLocked, Detail: detail, Transient: true, kind: writeFailed}
	default:
		return &KeychainError{Class: class.KeychainClass(), Detail: detail, Transient: true, kind: writeFailed}
	}
}

// signalName spells sig the way the audit log records it, such as
// `SIGTERM`, or returns "" for no signal or one the host does not name.
func signalName(sig os.Signal) string {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return ""
	}
	return unix.SignalName(s)
}

// KeychainWriteSignal names the signal that ended a keychain write whose
// outcome is unknown, such as `SIGTERM`: the one this process received when
// it cancelled the write, or the one that ended security(1) itself. It
// returns "" for any other error, and for a write ended by a timeout or by a
// cancellation no signal caused.
func KeychainWriteSignal(err error) string {
	ke, ok := errors.AsType[*KeychainError](err)
	if !ok {
		return ""
	}
	return ke.signal
}

// boundedBuffer keeps the first stderrCapture bytes written to it and
// discards the rest, so a child that floods stderr cannot grow this process.
//
// Its memory is fixed when it is made: the capture array is allocated once at
// full size and never grows, and the overflow passes through a fixed drain
// array, so wipe reaches every byte of child stderr this process held. The
// command's copying goroutine fills it through ReadFrom, which reads straight
// into those arrays instead of through io.Copy's own buffer.
//
// The lock is held across ReadFrom's blocking reads. classify and wipe run
// only after the child has been reaped, and the reap joins the copying
// goroutine, so neither waits on a live pipe.
type boundedBuffer struct {
	mu      sync.Mutex
	buf     []byte
	drain   [stderrDrain]byte
	dropped int
}

var _ io.ReaderFrom = (*boundedBuffer)(nil)

// newBoundedBuffer allocates the whole capture up front.
func newBoundedBuffer() *boundedBuffer {
	return &boundedBuffer{buf: make([]byte, 0, stderrCapture)}
}

// Write keeps what fits and reports the whole write as consumed, so the
// child is never blocked on a full pipe.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := copy(b.buf[len(b.buf):cap(b.buf)], p)
	b.buf = b.buf[:len(b.buf)+n]
	b.dropped += len(p) - n
	return len(p), nil
}

// ReadFrom reads r to its end into the free part of the capture and, once
// that is full, through the drain, so the child is never blocked on a full
// pipe. Reaching the end of r is success.
func (b *boundedBuffer) ReadFrom(r io.Reader) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var total int64
	for {
		dst := b.buf[len(b.buf):cap(b.buf)]
		full := len(dst) == 0
		if full {
			dst = b.drain[:]
		}
		n, err := r.Read(dst)
		total += int64(n)
		if full {
			b.dropped += n
		} else {
			b.buf = b.buf[:len(b.buf)+n]
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// classify reads the captured stderr once, in place, for its class only.
func (b *boundedBuffer) classify() StderrClass {
	b.mu.Lock()
	defer b.mu.Unlock()
	return classifyStderrBytes(b.buf)
}

// wipe zeroes the whole capture array and the drain. Neither is ever copied
// anywhere else, so zeroing them here leaves the class as the only trace of
// what the child said.
func (b *boundedBuffer) wipe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	memguard.WipeBytes(b.buf[:cap(b.buf)])
	memguard.WipeBytes(b.drain[:])
	b.buf = b.buf[:0]
}
