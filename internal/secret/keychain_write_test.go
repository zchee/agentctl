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

//go:build agentctl_testing

package secret

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/testutil"
)

// stderrCanary is planted in the stand-in's stderr; no returned error may
// carry it. It matches none of the classifier's phrases, so it never
// changes the class.
const stderrCanary = "STDERR-CANARY-8d41"

// testController installs a termination-signal controller for one test and
// withdraws it when the test ends.
func testController(t *testing.T) *signals.Controller {
	t.Helper()
	_, controller := signals.Install(t.Context())
	t.Cleanup(controller.Stop)
	return controller
}

// standInWriter wires the production writer to the installed stand-in, the
// way the tagged factory does. Knobs the test sets with t.Setenv before the
// call reach the child, because the writer copies the AGCTL_FAKE_
// environment when it is built.
func standInWriter(t *testing.T) (*KeychainWriter, *testutil.Fixture) {
	t.Helper()
	f := fakeKeychain(t)
	t.Setenv(keychainBackendEnv, "")
	t.Setenv(securityBinEnv, f.SecurityBin())
	t.Setenv("USER", testutil.KeychainAccount)
	return NewKeychainWriter(testController(t)), f
}

func newUpdate(t *testing.T, service, blob string) *KeychainUpdate {
	t.Helper()
	update, err := NewKeychainUpdate(testutil.KeychainAccount, service, []byte(blob))
	if err != nil {
		t.Fatal(err)
	}
	return update
}

// writeKinds names every write-kind sentinel, so a test can check that an
// error matches its own kind and none of the others.
var writeKinds = map[string]error{
	"not started":     ErrKeychainNotStarted,
	"outcome unknown": ErrKeychainOutcomeUnknown,
	"failed":          ErrKeychainWriteFailed,
}

// assertKind fails unless err matches the kind sentinel named want and no
// other kind sentinel.
func assertKind(t *testing.T, err error, want string) {
	t.Helper()
	for name, sentinel := range writeKinds {
		if got := errors.Is(err, sentinel); got != (name == want) {
			t.Errorf("errors.Is(%v, the %s kind) = %t, want %t", err, name, got, name == want)
		}
	}
}

// assertNoStderrText fails when err carries any part of the stand-in's
// stderr.
func assertNoStderrText(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), stderrCanary) {
		t.Errorf("the error carries the child's stderr: %v", err)
	}
}

// lookupPID waits up to five seconds for the stand-in's leader to write its
// pid to path, and reports whether it did. It never fails the test itself,
// so a goroutine other than the test's own may call it.
func lookupPID(path string) (int, bool) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid, true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0, false
}

// waitForPID waits until the stand-in's leader has written its pid there,
// and fails the test when it never does. Call it from the test goroutine.
func waitForPID(t *testing.T, path string) int {
	t.Helper()
	pid, ok := lookupPID(path)
	if !ok {
		t.Fatalf("the stand-in never wrote its pid to %s", path)
	}
	return pid
}

// countNonZero counts the bytes of b that are not zero.
func countNonZero(b []byte) int {
	n := 0
	for _, c := range b {
		if c != 0 {
			n++
		}
	}
	return n
}

// assertGroupGone fails when any process is still in the process group the
// leader pid led a second after the write returned. A member killed by the
// writer can take a moment to be reaped by its new parent, and macOS
// answers EPERM rather than ESRCH while it is on the way out, so the check
// polls; the stand-in's own delays last five seconds, so a member the
// writer failed to kill is still there when the second is up.
func assertGroupGone(t *testing.T, pgid int) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if err = syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	t.Fatalf("process group %d outlived the write (signal 0 answered %v)", pgid, err)
}

func TestKeychainWriteUpsertsThroughArgvOnly(t *testing.T) {
	tests := map[string]struct {
		service  string
		existing bool
	}{
		"success: creates an absent item":                    {service: "Claude Code-credentials-12345678"},
		"success: updates without removing an existing item": {service: "Claude Code-credentials-12345678", existing: true},
		"success: the live service travels as one argument":  {service: "Claude Code-credentials", existing: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			writer, f := standInWriter(t)
			const blob = `{"claudeAiOauth":{"accessToken":"synthetic-write-marker"},"mcpOAuth":{"s":{"accessToken":"synthetic-mcp-marker"}}}`
			f.AllowWrite(tt.service)
			if tt.existing {
				f.KeychainItem(tt.service, "old grant")
			}
			update := newUpdate(t, tt.service, blob)
			if err := writer.Write(t.Context(), tt.service, update); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(f.KeychainItemPath(tt.service))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(blob, string(got)); diff != "" {
				t.Errorf("stored document (-want +got):\n%s", diff)
			}
			want := []string{fmt.Sprintf(`add-generic-password -U -a "%s" -s "%s" -X <REDACTED:%d>`, testutil.KeychainAccount, tt.service, 2*len(blob))}
			if diff := gocmp.Diff(want, f.SecurityLog()); diff != "" {
				t.Errorf("argv log (-want +got):\n%s", diff)
			}
			for _, logged := range f.SecurityLog() {
				if strings.Contains(logged, "synthetic-") || strings.Contains(logged, hex.EncodeToString([]byte(blob))) {
					t.Fatal("the credential escaped into the argv log")
				}
			}
			if update.hex != nil {
				t.Errorf("Write did not consume the update")
			}
		})
	}
}

func TestKeychainWriteRefusesBeforeSpawn(t *testing.T) {
	tests := map[string]struct {
		mutate func(*KeychainUpdate)
		writer func(*KeychainWriter) *KeychainWriter
		target error
	}{
		"error: account mismatch": {mutate: func(u *KeychainUpdate) { u.account = "other" }},
		"error: service mismatch": {mutate: func(u *KeychainUpdate) { u.service = "other" }},
		"error: oversized update": {mutate: func(u *KeychainUpdate) { u.size = 2*KeychainBlobLimit + 2 }, target: ErrCredentialTooLarge},
		"error: empty update":     {mutate: func(u *KeychainUpdate) { u.hex = nil }},
		"error: a nil controller": {
			writer: func(w *KeychainWriter) *KeychainWriter { w.controller = nil; return w },
			target: ErrKeychainUnsupported,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			writer, f := standInWriter(t)
			f.AllowWrite("target")
			if tt.writer != nil {
				writer = tt.writer(writer)
			}
			update := newUpdate(t, "target", "document")
			if tt.mutate != nil {
				tt.mutate(update)
			}
			err := writer.Write(t.Context(), "target", update)
			if err == nil {
				t.Fatal("the invalid write was accepted")
			}
			if tt.target != nil && !errors.Is(err, tt.target) {
				t.Errorf("error = %v, want %v", err, tt.target)
			}
			for kind, sentinel := range writeKinds {
				if errors.Is(err, sentinel) {
					t.Errorf("a refusal before any spawn matches the %s kind: %v", kind, err)
				}
			}
			if got := f.SecurityLog(); len(got) != 0 {
				t.Fatalf("the refused write spawned a child: %v", got)
			}
		})
	}
}

func TestNewKeychainWriterWithANilControllerRefuses(t *testing.T) {
	f := fakeKeychain(t)
	t.Setenv(keychainBackendEnv, "")
	t.Setenv(securityBinEnv, f.SecurityBin())
	t.Setenv("USER", testutil.KeychainAccount)
	f.AllowWrite("target")

	err := NewKeychainWriter(nil).Write(t.Context(), "target", newUpdate(t, "target", "document"))
	if !errors.Is(err, ErrKeychainUnsupported) {
		t.Fatalf("write = %v, want unsupported", err)
	}
	if got := f.SecurityLog(); len(got) != 0 {
		t.Fatalf("a writer without a controller spawned a child: %v", got)
	}
}

func TestWriterWithoutSignalsStillWrites(t *testing.T) {
	f := fakeKeychain(t)
	t.Setenv(keychainBackendEnv, "")
	t.Setenv(securityBinEnv, f.SecurityBin())
	t.Setenv("USER", testutil.KeychainAccount)
	f.AllowWrite("target")

	if err := NewKeychainWriterWithoutSignals().Write(t.Context(), "target", newUpdate(t, "target", "document")); err != nil {
		t.Fatalf("write = %v", err)
	}
	if got, err := os.ReadFile(f.KeychainItemPath("target")); err != nil || string(got) != "document" {
		t.Fatalf("stored %q, %v", got, err)
	}
}

func TestKeychainWriteClassifiesExitsAndPreservesOldGrant(t *testing.T) {
	tests := map[string]struct {
		exit   string
		stderr string
		class  errs.KeychainClass
	}{
		"error: 36 is a locked keychain":     {exit: "36", class: errs.KeychainLocked},
		"error: 44 is a failure on upsert":   {exit: "44", class: errs.KeychainNotFound},
		"error: an unknown code, no stderr":  {exit: "7", class: StderrEmpty.KeychainClass()},
		"error: stderr class DuplicateItem":  {exit: "1", stderr: "errSecDuplicateItem", class: StderrDuplicateItem.KeychainClass()},
		"error: stderr class Unavailable":    {exit: "1", stderr: "unable to open the keychain", class: StderrKeychainUnavailable.KeychainClass()},
		"error: stderr class NoKeychain":     {exit: "1", stderr: "errSecNoDefaultKeychain", class: StderrNoKeychain.KeychainClass()},
		"error: stderr class ItemNotFound":   {exit: "1", stderr: "errSecItemNotFound", class: StderrItemNotFound.KeychainClass()},
		"error: stderr class Interaction":    {exit: "1", stderr: "errSecInteractionNotAllowed", class: StderrInteractionNotAllowed.KeychainClass()},
		"error: stderr class UserCanceled":   {exit: "1", stderr: "errSecUserCanceled", class: StderrUserCanceled.KeychainClass()},
		"error: stderr class AuthFailed":     {exit: "1", stderr: "errSecAuthFailed", class: StderrAuthFailed.KeychainClass()},
		"error: stderr class KeychainLocked": {exit: "1", stderr: "the keychain is locked", class: StderrKeychainLocked.KeychainClass()},
		"error: stderr class Other":          {exit: "1", stderr: "unrecognised failure", class: StderrOther.KeychainClass()},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGCTL_FAKE_SECURITY_WRITE_EXIT", tt.exit)
			stderr := stderrCanary
			if tt.stderr != "" {
				stderr = tt.stderr + " " + stderrCanary
			}
			if tt.class == StderrEmpty.KeychainClass() {
				stderr = ""
			}
			t.Setenv("AGCTL_FAKE_SECURITY_STDERR", stderr)
			writer, f := standInWriter(t)
			f.AllowWrite("target").KeychainItem("target", "old grant")

			err := writer.Write(t.Context(), "target", newUpdate(t, "target", "new grant"))
			ke, ok := errors.AsType[*KeychainError](err)
			if !ok || ke.Class != tt.class || !ke.Transient {
				t.Fatalf("error = %v, want transient %s", err, tt.class)
			}
			assertKind(t, err, "failed")
			assertNoStderrText(t, err)
			if want := "security exited " + tt.exit + " "; !strings.Contains(ke.Detail, want) {
				t.Errorf("detail %q does not name the exit status", ke.Detail)
			}
			if errors.Is(err, ErrKeychainTimeout) {
				t.Errorf("a definite failure matches the timeout class: %v", err)
			}
			got, readErr := os.ReadFile(f.KeychainItemPath("target"))
			if readErr != nil || string(got) != "old grant" {
				t.Fatalf("previous credential changed: %q, %v", got, readErr)
			}
		})
	}
}

func TestKeychainWriteExitZeroNeedsACleanWait(t *testing.T) {
	tests := map[string]struct {
		holdPipes bool
		wantErr   bool
	}{
		"success: exit 0 with a clean wait":                 {},
		"error: exit 0 while the output is still held open": {holdPipes: true, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "security.pid")
			t.Setenv("AGCTL_FAKE_SECURITY_PIDFILE", pidfile)
			if tt.holdPipes {
				t.Setenv("AGCTL_FAKE_SECURITY_HOLD_PIPES", "5")
			}
			writer, f := standInWriter(t)
			f.AllowWrite("target")

			err := writer.Write(t.Context(), "target", newUpdate(t, "target", "new grant"))
			pid := waitForPID(t, pidfile)
			// The holder is a background sleep in the leader's group; the
			// writer's group kill after the reap is what removes it.
			assertGroupGone(t, pid)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("write = %v", err)
				}
				return
			}
			ke, ok := errors.AsType[*KeychainError](err)
			if !ok || ke.Class != errs.KeychainUnavailable || !ke.Transient {
				t.Fatalf("error = %v, want transient unavailable", err)
			}
			assertKind(t, err, "outcome unknown")
			if errors.Is(err, ErrKeychainTimeout) {
				t.Errorf("a held pipe is not a timeout: %v", err)
			}
			// The item was stored before the exit, which is why this is an
			// unknown outcome for a readback to settle, not a failure.
			if got, readErr := os.ReadFile(f.KeychainItemPath("target")); readErr != nil || string(got) != "new grant" {
				t.Fatalf("the stand-in did not store before exiting: %q, %v", got, readErr)
			}
		})
	}
}

func TestKeychainWriteDeadlineKillsAndReapsTheGroup(t *testing.T) {
	tests := map[string]struct {
		cancelled  bool
		afterStore bool
	}{
		"error: write budget, before the store":      {},
		"error: write budget, after the store":       {afterStore: true},
		"error: caller cancellation, before storing": {cancelled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.afterStore {
				t.Setenv("AGCTL_FAKE_WRITE_DELAY_AFTER_MS", "5000")
			} else {
				t.Setenv("AGCTL_FAKE_WRITE_DELAY_MS", "5000")
			}
			writer, f := standInWriter(t)
			f.AllowWrite("target").KeychainItem("target", "old grant")
			// A wrapper that records the leader's pid and then becomes the
			// stand-in, so the pid is the group the writer must clear. The
			// stand-in's delay is a foreground sleep: a grandchild that
			// outlives a killed leader unless the group is killed too.
			dir := t.TempDir()
			pidfile := filepath.Join(dir, "leader.pid")
			wrapper := filepath.Join(dir, "security")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$$\" > %q\nexec %q \"$@\"\n", pidfile, f.SecurityBin())
			if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			writer.bin = wrapper
			writer.budget = 300 * time.Millisecond

			ctx := t.Context()
			if tt.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			err := writer.Write(ctx, "target", newUpdate(t, "target", "new grant"))
			if !errors.Is(err, ErrKeychainTimeout) {
				t.Fatalf("write = %v, want timeout", err)
			}
			assertKind(t, err, "outcome unknown")
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("the write waited for the grandchild: %v", elapsed)
			}
			assertGroupGone(t, waitForPID(t, pidfile))
			want := "old grant"
			if tt.afterStore {
				want = "new grant"
			}
			if got, readErr := os.ReadFile(f.KeychainItemPath("target")); readErr != nil || string(got) != want {
				t.Fatalf("item holds %q (%v), want %q", got, readErr, want)
			}
		})
	}
}

func TestKeychainWriteSignalEndedLeaderIsAnUnknownOutcome(t *testing.T) {
	tests := map[string]struct {
		cancelFirst bool
		signal      syscall.Signal
		class       errs.KeychainClass
	}{
		"error: SIGTERM after the context is cancelled": {cancelFirst: true, signal: syscall.SIGTERM, class: errs.KeychainTimeout},
		"error: SIGKILL from outside, context live":     {signal: syscall.SIGKILL, class: errs.KeychainUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "security.pid")
			t.Setenv("AGCTL_FAKE_SECURITY_PIDFILE", pidfile)
			t.Setenv("AGCTL_FAKE_WRITE_DELAY_MS", "5000")
			writer, f := standInWriter(t)
			f.AllowWrite("target").KeychainItem("target", "old grant")
			writer.budget = 10 * time.Second

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// The goroutine only reports; the test goroutine fails. Without a
			// pid it signals nothing: kill(0, sig) would reach the test
			// binary's own process group.
			done := make(chan int, 1)
			go func() {
				pid, ok := lookupPID(pidfile)
				if !ok {
					done <- 0
					return
				}
				if tt.cancelFirst {
					cancel()
				}
				_ = syscall.Kill(pid, tt.signal)
				done <- pid
			}()
			err := writer.Write(ctx, "target", newUpdate(t, "target", "new grant"))
			pid := <-done
			if pid == 0 {
				t.Fatalf("the stand-in never wrote its pid to %s", pidfile)
			}

			ke, ok := errors.AsType[*KeychainError](err)
			if !ok || ke.Class != tt.class || !ke.Transient {
				t.Fatalf("error = %v, want transient %s", err, tt.class)
			}
			assertKind(t, err, "outcome unknown")
			if got := errors.Is(err, ErrKeychainTimeout); got != tt.cancelFirst {
				t.Errorf("errors.Is(ErrKeychainTimeout) = %t, want %t: only a timeout or a cancellation is one", got, tt.cancelFirst)
			}
			assertGroupGone(t, pid)
		})
	}
}

// TestKeychainWriteWipesEveryStderrByte floods the child's stderr past the
// capture bound and checks, after Write returns, that the capture stayed in
// the one array it was made with and that every byte it held is zero.
func TestKeychainWriteWipesEveryStderrByte(t *testing.T) {
	// A phrase the classifier knows, then canaries well past the bound, so
	// the capture fills and the rest is read through the drain.
	flood := "errSecAuthFailed " + strings.Repeat(stderrCanary+" ", stderrCapture/len(stderrCanary)+64)
	t.Setenv("AGCTL_FAKE_SECURITY_WRITE_EXIT", "1")
	t.Setenv("AGCTL_FAKE_SECURITY_STDERR", flood)
	writer, f := standInWriter(t)
	f.AllowWrite("target").KeychainItem("target", "old grant")
	var (
		captures []*boundedBuffer
		backings [][]byte
	)
	writer.captureHook = func(b *boundedBuffer) {
		captures = append(captures, b)
		backings = append(backings, b.buf[:cap(b.buf)])
	}

	err := writer.Write(t.Context(), "target", newUpdate(t, "target", "new grant"))
	ke, ok := errors.AsType[*KeychainError](err)
	if !ok || ke.Class != StderrAuthFailed.KeychainClass() {
		t.Fatalf("error = %v, want the AuthFailed class read from the capture", err)
	}
	assertKind(t, err, "failed")
	assertNoStderrText(t, err)
	if len(captures) != 1 {
		t.Fatalf("the write made %d stderr captures, want 1", len(captures))
	}
	b, backing := captures[0], backings[0]
	if cap(b.buf) != stderrCapture || &b.buf[:cap(b.buf)][0] != &backing[0] {
		t.Fatalf("the capture left the array it was made with (cap %d, want %d)", cap(b.buf), stderrCapture)
	}
	// The stand-in ends its stderr with a newline.
	if want := len(flood) + 1 - stderrCapture; b.dropped != want {
		t.Errorf("dropped %d bytes past the bound, want %d", b.dropped, want)
	}
	if len(b.buf) != 0 {
		t.Errorf("the capture still holds %d bytes", len(b.buf))
	}
	if n := countNonZero(backing); n != 0 {
		t.Errorf("%d of the capture array's %d bytes are not zero after Write", n, len(backing))
	}
	if n := countNonZero(b.drain[:]); n != 0 {
		t.Errorf("%d of the drain's %d bytes are not zero after Write", n, len(b.drain))
	}
}

func TestBoundedBufferStaysInItsArraysAndWipesThem(t *testing.T) {
	over := strings.Repeat(stderrCanary, stderrCapture/len(stderrCanary)+100)
	tests := map[string]struct {
		fill    func(*boundedBuffer) error
		wantErr error
		kept    int
		dropped int
	}{
		"success: chunked writes past the bound": {
			fill: func(b *boundedBuffer) error {
				for chunk := range slices.Chunk([]byte(over), 1000) {
					if n, err := b.Write(chunk); err != nil || n != len(chunk) {
						return fmt.Errorf("Write consumed %d of %d: %w", n, len(chunk), err)
					}
				}
				return nil
			},
			kept:    stderrCapture,
			dropped: len(over) - stderrCapture,
		},
		"success: short reads to the end past the bound": {
			fill: func(b *boundedBuffer) error {
				n, err := b.ReadFrom(iotest.HalfReader(strings.NewReader(over)))
				if err == nil && n != int64(len(over)) {
					return fmt.Errorf("ReadFrom consumed %d of %d", n, len(over))
				}
				return err
			},
			kept:    stderrCapture,
			dropped: len(over) - stderrCapture,
		},
		"error: a read failure is returned, not swallowed": {
			fill: func(b *boundedBuffer) error {
				_, err := b.ReadFrom(iotest.TimeoutReader(strings.NewReader(over)))
				return err
			},
			wantErr: iotest.ErrTimeout,
			kept:    stderrCapture,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b := newBoundedBuffer()
			backing := b.buf[:cap(b.buf)]
			if err := tt.fill(b); !errors.Is(err, tt.wantErr) {
				t.Fatalf("fill = %v, want %v", err, tt.wantErr)
			}
			if cap(b.buf) != stderrCapture || &b.buf[:cap(b.buf)][0] != &backing[0] {
				t.Fatalf("the capture left the array it was made with (cap %d, want %d)", cap(b.buf), stderrCapture)
			}
			if len(b.buf) != tt.kept || b.dropped != tt.dropped {
				t.Errorf("kept %d and dropped %d, want %d and %d", len(b.buf), b.dropped, tt.kept, tt.dropped)
			}
			if got := b.classify(); got != StderrOther {
				t.Errorf("class = %v, want %v", got, StderrOther)
			}
			b.wipe()
			if n := countNonZero(backing); n != 0 {
				t.Errorf("%d capture bytes are not zero after wipe", n)
			}
			if n := countNonZero(b.drain[:]); n != 0 {
				t.Errorf("%d drain bytes are not zero after wipe", n)
			}
		})
	}
}

func TestKeychainWriteNotStarted(t *testing.T) {
	t.Run("error: a missing binary is permanent", func(t *testing.T) {
		writer, _ := standInWriter(t)
		writer.bin = "/nonexistent/security"
		err := writer.Write(t.Context(), "target", newUpdate(t, "target", "blob"))
		ke, ok := errors.AsType[*KeychainError](err)
		if !ok || ke.Class != errs.KeychainUnavailable || ke.Transient {
			t.Fatalf("write = %v, want permanent unavailable", err)
		}
		assertKind(t, err, "not started")
		if errors.Is(err, ErrCredentialTooLarge) {
			t.Errorf("a missing binary is not a size refusal: %v", err)
		}
	})

	t.Run("error: a shutdown already in progress starts nothing", func(t *testing.T) {
		writer, f := standInWriter(t)
		f.AllowWrite("target")
		writer.controller.Stop()
		err := writer.Write(t.Context(), "target", newUpdate(t, "target", "blob"))
		assertKind(t, err, "not started")
		if ke, ok := errors.AsType[*KeychainError](err); !ok || !strings.Contains(ke.Detail, "a shutdown is in progress") {
			t.Fatalf("write = %v, want the shutdown refusal", err)
		}
		if got := f.SecurityLog(); len(got) != 0 {
			t.Fatalf("a cancelled run spawned a child: %v", got)
		}
	})
}

// TestKeychainWriteE2BIG execs the stand-in with a payload argument larger
// than any host's argument space, which only the spawn path can be handed:
// the constructor refuses a document that size long before.
func TestKeychainWriteE2BIG(t *testing.T) {
	writer, f := standInWriter(t)
	f.AllowWrite("target").KeychainItem("target", "old grant")
	update := newUpdate(t, "target", "small")
	payload := bytes.Repeat([]byte{'a'}, 4<<20)

	err := writer.spawn(t.Context(), update, payload)
	ke, ok := errors.AsType[*KeychainError](err)
	if !ok || ke.Class != errs.KeychainCredentialTooLarge || ke.Transient {
		t.Fatalf("spawn = %v, want the permanent credential-too-large class", err)
	}
	assertKind(t, err, "not started")
	if !errors.Is(err, ErrCredentialTooLarge) {
		t.Errorf("E2BIG must match ErrCredentialTooLarge: %v", err)
	}
	if !strings.Contains(ke.Detail, "E2BIG") {
		t.Errorf("detail %q does not name E2BIG", ke.Detail)
	}
	if code := errs.ExitCode(err); code != cli.SwapExitCredentialTooLarge {
		t.Errorf("ExitCode = %d, want %d", code, cli.SwapExitCredentialTooLarge)
	}
	if got := f.SecurityLog(); len(got) != 0 {
		t.Fatalf("the stand-in ran, so the host did not refuse the list: %v", got)
	}
	if got, readErr := os.ReadFile(f.KeychainItemPath("target")); readErr != nil || string(got) != "old grant" {
		t.Fatalf("the item changed: %q, %v", got, readErr)
	}
}
