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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/errs"
)

// KeychainWriteTimeout bounds the child while the caller holds the peer locks.
const KeychainWriteTimeout = 1200 * time.Millisecond

// KeychainVerifyTimeout bounds the under-lock read before a keychain write.
const KeychainVerifyTimeout = 800 * time.Millisecond

const keychainWriteArg = "-i"

// KeychainTargetMismatchError reports a line built for a different item.
// The request is refused before a child is started.
type KeychainTargetMismatchError struct {
	Expected string
	Found    string
}

// Error names the mismatched account or service, never the credential.
func (e *KeychainTargetMismatchError) Error() string {
	return fmt.Sprintf("this keychain line was built for `%s`, not for `%s`", e.Found, e.Expected)
}

// KeychainWriter updates items through a bounded, stdin-only child process.
// Callers must derive the service from an owned namespace or the validated live
// environment and hold the corresponding peer locks before calling Write.
// The zero value refuses every write.
type KeychainWriter struct {
	bin     string
	account string
	env     []string
	budget  time.Duration
}

// NewKeychainWriter selects the fixed production binary or the tagged test seam.
// Construction does not start a process. A testing build without a stand-in
// refuses writes rather than falling back to the user's keychain.
func NewKeychainWriter() *KeychainWriter {
	reader, ok := newReader().(*securityCLI)
	if !ok {
		return &KeychainWriter{account: CurrentAccount()}
	}
	return &KeychainWriter{bin: reader.bin, account: reader.account, env: reader.env, budget: KeychainWriteTimeout}
}

// Write updates service in place with line, creating the item when absent.
// Account, service and length are checked before spawning. It does not acquire
// locks, derive write authority, retry failures or remove an existing item.
// Transport failures return a classified KeychainError; a timeout leaves the
// write outcome unknown. Plaintext is exposed only while writing the stdin pipe.
func (w *KeychainWriter) Write(ctx context.Context, service string, line *KeychainStdinLine) error {
	if line == nil || line.line == nil || line.size <= 0 {
		return errors.New("the keychain update line is empty")
	}
	if line.Account() != w.account {
		return &KeychainTargetMismatchError{Expected: w.account, Found: line.Account()}
	}
	if line.Service() != service {
		return &KeychainTargetMismatchError{Expected: service, Found: line.Service()}
	}
	if line.Len() > KeychainLineLimit {
		return &LineTooLongError{Len: line.Len()}
	}
	if w.bin == "" {
		return ErrKeychainUnsupported
	}

	callCtx, cancel := context.WithTimeout(ctx, w.budget)
	defer cancel()
	cmd := exec.CommandContext(callCtx, w.bin, keychainWriteArg)
	cmd.Env = w.env
	cmd.WaitDelay = childWaitDelay
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: "security stdin could not be opened"}
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return &KeychainError{Class: errs.KeychainUnavailable, Detail: fmt.Sprintf("security could not be started: %s: %v", w.bin, err)}
	}

	// The validated line is smaller than the pipe capacity, so even a child
	// that never reads cannot block this handoff. Closing supplies the EOF
	// required by the interactive command reader.
	n, handoffErr := line.WriteTo(stdin)
	if handoffErr == nil && n != int64(line.Len()) {
		handoffErr = io.ErrShortWrite
	}
	closeErr := stdin.Close()
	runErr := cmd.Wait()
	if callCtx.Err() != nil {
		return &KeychainError{Class: errs.KeychainTimeout, Detail: fmt.Sprintf("security timed out after %d ms", w.budget.Milliseconds()), Transient: true}
	}
	if err := errors.Join(handoffErr, closeErr); err != nil {
		return &KeychainError{Class: StderrOther.KeychainClass(), Detail: fmt.Sprintf("the line could not be handed to security: %v", err), Transient: true}
	}
	if runErr != nil && cmd.ProcessState == nil {
		return &KeychainError{Class: StderrOther.KeychainClass(), Detail: "security did not report an exit status", Transient: true}
	}
	code := cmd.ProcessState.ExitCode()
	if code == 0 {
		if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
			return &KeychainError{Class: StderrOther.KeychainClass(), Detail: "security output could not be collected", Transient: true}
		}
		return nil
	}
	if code == exitNotFound {
		// Unlike a read, an upsert cannot legitimately answer absent.
		return &KeychainError{Class: errs.KeychainNotFound, Detail: strings.TrimSpace(stderr.String()), Transient: true}
	}
	return (runOutput{code: code, stderr: stderr.String()}).failure()
}
