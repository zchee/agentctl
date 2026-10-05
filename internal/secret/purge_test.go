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
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"
)

// baseOpenBufferBudget is a conservative capacity floor, not an exact count
// of simultaneously open plaintext buffers in a credential operation.
const baseOpenBufferBudget = 8

func TestPurge(t *testing.T) {
	s, err := NewSecret([]byte("purge-test-token"))
	if err != nil {
		t.Fatal(err)
	}
	open := memguard.NewBufferFromBytes([]byte("open-purge-test-token"))
	Purge()
	if open.IsAlive() {
		t.Fatal("purge left an open plaintext buffer alive")
	}
	if err := s.WithPlaintext(func([]byte) error { return nil }); err == nil {
		t.Fatal("a purged secret must not decrypt")
	}
	// Repeated purge is safe once plaintext users have stopped, and a later
	// session can create fresh key material.
	Purge()
	after, err := NewSecret([]byte("post-purge-token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := after.WithPlaintext(func(b []byte) error {
		if string(b) != "post-purge-token" {
			t.Errorf("plaintext after purge = %q", b)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	Purge()
}

func TestPackageKeepsDefaultSignalDisposition(t *testing.T) {
	if os.Getenv("AGENTCTL_TEST_SIGNAL_DISPOSITION") == "1" {
		s, err := NewSecret([]byte("signal-disposition-token"))
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Len()
		Purge()
		// With the default disposition this kill terminates the process with
		// a SIGINT death. A handler registered by this package or memguard
		// would instead run and exit with status 1 (memguard.CatchSignal
		// documents that exit), which the parent below rejects.
		if err := unix.Kill(os.Getpid(), unix.SIGINT); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		t.Fatal("SIGINT default disposition did not terminate the process")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPackageKeepsDefaultSignalDisposition$", "-test.v")
	cmd.Env = append(os.Environ(), "AGENTCTL_TEST_SIGNAL_DISPOSITION=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child survived SIGINT, so something installed a handler:\n%s", output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child run: %v\n%s", err, output)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("child did not die from SIGINT: %v (status %#v)\n%s", err, exitErr.Sys(), output)
	}
}

// openUntilFailure opens one locked plaintext buffer per sealed secret until
// an open fails or the cap is reached, and reports how many were open at once
// together with the failure that stopped it. memguard converts an mlock
// failure into a panic, so the probe recovers to observe it. When progress
// is non-nil every successful open is reported through it immediately, so a
// child whose failure path aborts the process still leaves the count behind.
func openUntilFailure(maxOpen int, progress func(opened int)) (opened int, failure any) {
	buffers := make([]*memguard.LockedBuffer, 0, maxOpen)
	defer func() {
		for _, b := range buffers {
			b.Destroy()
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			failure = recovered
		}
	}()
	for range maxOpen {
		s, err := NewSecret([]byte("memlock-budget-probe-token-0123456789abcdef"))
		if err != nil {
			return len(buffers), err
		}
		b, err := s.enclave.Open()
		if err != nil {
			return len(buffers), err
		}
		buffers = append(buffers, b)
		opened = len(buffers)
		if progress != nil {
			progress(opened)
		}
	}
	return len(buffers), nil
}

func TestLockedBufferBudget(t *testing.T) {
	if os.Getenv("AGENTCTL_TEST_MEMLOCK_CONSTRAINED") == "1" {
		limit := unix.Rlimit{Cur: 256 << 10, Max: 256 << 10}
		if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &limit); err != nil {
			t.Fatalf("lowering RLIMIT_MEMLOCK: %v", err)
		}
		// Record each successful open before an allocation failure can abort
		// or hang the child, leaving the last completed count observable.
		opened, failure := openUntilFailure(1024, func(opened int) {
			fmt.Printf("constrained: %d buffers open under RLIMIT_MEMLOCK=%d, page size %d\n", opened, limit.Cur, unix.Getpagesize())
		})
		fmt.Printf("constrained: stopped at %d buffers, failure: %v\n", opened, failure)
		return
	}

	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &limit); err != nil {
		t.Fatalf("reading RLIMIT_MEMLOCK: %v", err)
	}
	pageSize := unix.Getpagesize()
	need := baseOpenBufferBudget * 2

	opened, failure := openUntilFailure(need, nil)
	t.Logf("measurement: RLIMIT_MEMLOCK cur=%d max=%d (%#x means unlimited), page size %d bytes", limit.Cur, limit.Max, uint64(unix.RLIM_INFINITY), pageSize)
	t.Logf("measurement: need %d simultaneous buffers (%d-buffer capacity floor with a 2x margin), opened %d, failure: %v", need, baseOpenBufferBudget, opened, failure)
	if failure != nil || opened < need {
		t.Fatalf("only %d of %d required locked buffers could be open at once: %v", opened, need, failure)
	}

	const probeCap = 1024
	probed, probeFailure := openUntilFailure(probeCap, nil)
	t.Logf("measurement: headroom probe opened %d buffers (cap %d), failure: %v", probed, probeCap, probeFailure)

	// The constrained probe runs in a deadline-bounded child because an
	// enforced mlock failure makes memguard panic through its purge path,
	// which has been observed to deadlock instead of exiting. Its output is
	// recorded, not asserted: the budget claim above is what must hold, and
	// the child's last progress line is the measured failure point.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockedBufferBudget$", "-test.v")
	cmd.Env = append(os.Environ(), "AGENTCTL_TEST_MEMLOCK_CONSTRAINED=1")
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	output, err := cmd.CombinedOutput()
	t.Logf("constrained child (err=%v):\n%s", err, output)
}
