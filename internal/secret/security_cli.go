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
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/errs"
)

// ReadTimeout is the budget for show-keychain-info and
// find-generic-password. A keychain prompt nobody answers must not hang the
// process, so every call is bounded and an overrun is killed.
const ReadTimeout = 2 * time.Second

// DumpTimeout is the budget for dump-keychain, which walks every item.
const DumpTimeout = 10 * time.Second

// childWaitDelay bounds how long a finished or killed child may hold its
// pipes open before they are forced closed. Killing a child does not close a
// pipe its own children inherited, and waiting for a grandchild to exit is
// exactly the unbounded wait the budgets exist to prevent.
const childWaitDelay = 100 * time.Millisecond

// The security(1) argv vocabulary. The three calls below are built from
// these constants rather than spelling them again, so what the child
// receives cannot drift from what the tests enumerate. Only three
// subcommands exist here, and all three read; the service name is an
// argument and the password comes back on stdout, so no secret ever appears
// in argv.
const (
	preflightSubcommand = "show-keychain-info"
	dumpSubcommand      = "dump-keychain"
	findSubcommand      = "find-generic-password"
	accountFlag         = "-a"
	passwordFlag        = "-w"
	serviceFlag         = "-s"
)

// exitLocked is security(1)'s exit status for a locked keychain.
const exitLocked = 36

// exitNotFound is security(1)'s exit status for "no such item".
const exitNotFound = 44

// NewReader builds the keychain reader this build selects.
//
// A release build always reads through /usr/bin/security — an absolute path,
// never resolved through PATH, because this process must not be talked into
// running some other program by an inherited environment — and refuses on a
// platform without the transport. A tagged build selects its backend through
// the testing seam, and with nothing wired it falls closed to
// [DisabledReader]: a test that has not decided to talk to a keychain must
// not be able to reach the developer's own by omission.
//
// Constructing a reader runs nothing; the first call does.
func NewReader() Reader {
	return newReader()
}

// minimalChildEnv is the entire environment a security(1) child receives:
// the home directory it locates the login keychain through, the identity
// variables, and a fixed system PATH. Everything else the parent inherited
// stays with the parent, so nothing accidental leaks into — or steers — the
// child. The tagged backend extends this with the stand-in's own knobs.
func minimalChildEnv() []string {
	env := []string{"PATH=/usr/bin:/bin"}
	for _, key := range [...]string{"HOME", "USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// currentAccount is the account attribute the vendor tooling stores its
// items under: $USER, with LOGNAME as the fallback. An empty account still
// produces a well-formed find-generic-password call, which simply finds
// nothing.
func currentAccount() string {
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return os.Getenv("LOGNAME")
}

// securityCLI is a [Reader] backed by the security(1) command-line tool.
type securityCLI struct {
	bin     string
	account string
	// env is the complete child environment; the child sees nothing the
	// build did not decide to show it.
	env []string
	// readBudget and dumpBudget are the per-call deadlines, fields rather
	// than the constants so a test can shrink them without waiting out a
	// real budget.
	readBudget time.Duration
	dumpBudget time.Duration

	// mu guards the dump memo. One reader answers more than one service
	// prefix from the same ten-second dump, so the parsed listing is kept
	// for the reader's lifetime. The memo holds attributes only; no
	// password material passes through it.
	mu     sync.Mutex
	dump   []ServiceEntry
	dumped bool
}

var _ Reader = (*securityCLI)(nil)

// newSecurityCLI builds a transport over bin, reading items stored under
// account, with env as the child's entire environment.
func newSecurityCLI(bin, account string, env []string) *securityCLI {
	return &securityCLI{
		bin:        bin,
		account:    account,
		env:        env,
		readBudget: ReadTimeout,
		dumpBudget: DumpTimeout,
	}
}

// unsupportedReader refuses every operation: this platform has no keychain
// transport, and no environment variable can conjure one in a release build.
type unsupportedReader struct{}

var _ Reader = unsupportedReader{}

// Preflight reports the platform has no transport.
func (unsupportedReader) Preflight(context.Context) KeychainStatus {
	return KeychainStatus{State: KeychainStateUnsupported}
}

// ListServices refuses: there is no transport to list through.
func (unsupportedReader) ListServices(context.Context, string) ([]ServiceEntry, error) {
	return nil, ErrKeychainUnsupported
}

// Read refuses: there is no transport to read through.
func (unsupportedReader) Read(context.Context, string) (*Secret, error) {
	return nil, ErrKeychainUnsupported
}

// runOutput is one completed security(1) invocation.
type runOutput struct {
	// code is the exit status, or -1 when the child died from a signal.
	code int
	// stdout holds the child's standard output. On the find path it is the
	// password, so the caller wipes it; everywhere else it is attributes.
	stdout []byte
	// stderr is captured for classification only. It never carries the
	// payload: that travels on stdout.
	stderr string
}

// failure turns a non-zero exit into the classified error it stands for.
func (o runOutput) failure() *KeychainError {
	if o.code == exitLocked {
		return &KeychainError{Class: errs.KeychainLocked, Transient: true}
	}
	class := ClassifyStderr(o.stderr)
	return &KeychainError{
		Class:     class.KeychainClass(),
		Detail:    strings.TrimSpace(o.stderr),
		Transient: true,
	}
}

// run executes one security(1) subcommand under budget and collects its
// output. The child gets the environment the build constructed and nothing
// else, an empty stdin, and a deadline: a child that overruns is killed and
// reaped, and reported as a timeout — which is transient, and never a reason
// to read the credential file instead.
func (c *securityCLI) run(ctx context.Context, args []string, budget time.Duration) (runOutput, *KeychainError) {
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	cmd := exec.CommandContext(callCtx, c.bin, args...)
	cmd.Env = c.env
	cmd.WaitDelay = childWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	code := 0
	switch {
	case runErr == nil:
	case callCtx.Err() != nil:
		// The budget expired, or the caller wound the pass down; both are
		// transient in exactly the same way, so there is no cancellation
		// class to tempt a caller into retrying differently. Whatever
		// reached stdout before the kill may be payload, so it is wiped.
		memguard.WipeBytes(stdout.Bytes())
		return runOutput{}, &KeychainError{
			Class:     errs.KeychainTimeout,
			Detail:    fmt.Sprintf("security timed out after %d ms", budget.Milliseconds()),
			Transient: true,
		}
	default:
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			code = exitErr.ExitCode()
		case errors.Is(runErr, exec.ErrWaitDelay) && cmd.ProcessState != nil:
			// The child exited but something it spawned still held a pipe;
			// the exit status is real, the output merely truncated.
			code = cmd.ProcessState.ExitCode()
		default:
			memguard.WipeBytes(stdout.Bytes())
			return runOutput{}, &KeychainError{
				Class:  errs.KeychainUnavailable,
				Detail: fmt.Sprintf("security could not be started: %s: %v", c.bin, runErr),
			}
		}
	}

	return runOutput{code: code, stdout: stdout.Bytes(), stderr: stderr.String()}, nil
}

// Preflight implements [Reader].
func (c *securityCLI) Preflight(ctx context.Context) KeychainStatus {
	out, kerr := c.run(ctx, []string{preflightSubcommand}, c.readBudget)
	if kerr != nil {
		if kerr.Class == errs.KeychainTimeout {
			return KeychainStatus{State: KeychainStateTimeout}
		}
		return KeychainStatus{State: KeychainStateUnavailable, Reason: kerr.Error()}
	}
	switch out.code {
	case 0:
		return KeychainStatus{State: KeychainStateUnlocked}
	case exitLocked:
		return KeychainStatus{State: KeychainStateLocked}
	}
	class := ClassifyStderr(out.stderr)
	if class == StderrKeychainLocked {
		return KeychainStatus{State: KeychainStateLocked}
	}
	return KeychainStatus{
		State:  KeychainStateUnavailable,
		Reason: class.String() + ": " + strings.TrimSpace(out.stderr),
	}
}

// ListServices implements [Reader], answering from a dump taken once and
// kept for this reader's lifetime.
func (c *securityCLI) ListServices(ctx context.Context, prefix string) ([]ServiceEntry, error) {
	c.mu.Lock()
	entries, cached := c.dump, c.dumped
	c.mu.Unlock()
	if !cached {
		out, kerr := c.run(ctx, []string{dumpSubcommand}, c.dumpBudget)
		if kerr != nil {
			return nil, kerr
		}
		if out.code != 0 {
			return nil, out.failure()
		}
		parsed, err := parseDump(string(out.stdout))
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.dump, c.dumped = parsed, true
		c.mu.Unlock()
		entries = parsed
	}

	var matched []ServiceEntry
	for _, entry := range entries {
		if strings.HasPrefix(entry.Service, prefix) {
			matched = append(matched, entry)
		}
	}
	return matched, nil
}

// ListServicesUncached re-runs dump-keychain, replacing this reader's memo
// with the result. A caller comparing the keychain before a child ran with
// the keychain after must not have the second question answered from the
// first question's dump. Deliberately a method on the concrete type: a
// [Reader] offers no way around the memo.
func (c *securityCLI) ListServicesUncached(ctx context.Context, prefix string) ([]ServiceEntry, error) {
	c.mu.Lock()
	c.dump, c.dumped = nil, false
	c.mu.Unlock()
	return c.ListServices(ctx, prefix)
}

// Read implements [Reader]. The password comes back on stdout, is sealed
// into a [Secret], and the raw buffer is wiped before this returns on every
// path.
func (c *securityCLI) Read(ctx context.Context, service string) (*Secret, error) {
	args := []string{findSubcommand, accountFlag, c.account, passwordFlag, serviceFlag, service}
	out, kerr := c.run(ctx, args, c.readBudget)
	if kerr != nil {
		return nil, kerr
	}
	defer memguard.WipeBytes(out.stdout)
	switch out.code {
	case 0:
		payload := trimTrailingNewline(out.stdout)
		if len(payload) == 0 {
			// Sealing nothing is refused by the Secret type, and an item
			// that exists but holds no bytes is not a credential; transient
			// leaves it for the user to look at rather than absent inviting
			// an overwrite.
			return nil, &KeychainError{
				Class:     errs.KeychainClass("EmptyItem"),
				Detail:    "the keychain item holds no bytes",
				Transient: true,
			}
		}
		sealed, err := NewSecret(payload)
		if err != nil {
			return nil, &KeychainError{
				Class:     errs.KeychainClass("SealFailed"),
				Detail:    err.Error(),
				Transient: true,
			}
		}
		return sealed, nil
	case exitNotFound:
		return nil, &KeychainError{
			Class:  errs.KeychainNotFound,
			Detail: strings.TrimSpace(out.stderr),
		}
	default:
		return nil, out.failure()
	}
}

// trimTrailingNewline drops the single trailing newline security -w prints
// after a password, without copying: the result aliases b, so wiping b wipes
// it too.
func trimTrailingNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}
