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

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/coordinator"
	"github.com/zchee/agentctl/internal/runtime/signals"
)

// KeychainWriteTimeout bounds the child while the caller holds the peer locks.
const KeychainWriteTimeout = 1200 * time.Millisecond

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
	// unregistered lets a writer built without a controller write anyway,
	// with its children reachable from the write's own wait only.
	unregistered bool
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

// NewKeychainWriterWithoutSignals builds a writer whose children are not
// registered with any termination-signal controller; the write's own bounded
// wait is their only bound.
//
// It is kept only for the callers that do not reach the run's controller
// yet; new code uses [NewKeychainWriter] with that controller.
func NewKeychainWriterWithoutSignals() *KeychainWriter {
	w := NewKeychainWriter(nil)
	w.unregistered = true
	return w
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
	if w.bin == "" || (w.controller == nil && !w.unregistered) {
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
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: "a shutdown is in progress; no keychain update was issued", Transient: true, kind: writeNotStarted}
	}

	// No CommandContext: the pass owns the kill, and a second killer would
	// race its reap.
	cmd := exec.Command(w.bin, writeSubcommand, updateFlag, accountFlag, update.Account(), serviceFlag, update.Service(), payloadFlag, string(payload))
	cmd.Env = w.env
	cmd.Stdout = io.Discard
	stderr := &boundedBuffer{limit: stderrCapture}
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

	if err := classifyWrite(ctx, state, waitErr, w.budget, stderr); err != nil {
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
func classifyWrite(ctx context.Context, state *os.ProcessState, waitErr error, budget time.Duration, stderr *boundedBuffer) error {
	switch {
	case ctx.Err() != nil:
		return &KeychainError{Class: errs.KeychainTimeout, Detail: "the run was cancelled while security was running", Transient: true, kind: writeOutcomeUnknown}
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
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: "security was ended by a signal", Transient: true, kind: writeOutcomeUnknown}
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

// boundedBuffer keeps the first limit bytes written to it and discards the
// rest, so a child that floods stderr cannot grow this process. The writes
// come from the command's copying goroutine, so access is locked.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

// Write keeps what fits and reports the whole write as consumed, so the
// child is never blocked on a full pipe.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// classify reads the captured stderr once, for its class only.
func (b *boundedBuffer) classify() StderrClass {
	b.mu.Lock()
	defer b.mu.Unlock()
	return ClassifyStderr(string(b.buf))
}

// wipe zeroes the captured stderr. It is never copied anywhere else, so
// zeroing it here leaves the class as the only trace of what it said.
func (b *boundedBuffer) wipe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	memguard.WipeBytes(b.buf)
	b.buf = nil
}
