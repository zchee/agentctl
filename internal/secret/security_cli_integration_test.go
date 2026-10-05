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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/fixtures"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/testutil"
)

// fakeToken is the item payload the redaction assertions hunt for. The
// distinctive prefix is what must never surface outside the sealed Secret.
const fakeToken = "sk-ant-oat01-FAKE-NOT-A-REAL-TOKEN"

// fakeKeychain installs the stand-in security(1) through the shared fixture
// and wires its knobs into this process's environment, which is where the
// tagged child-environment builder reads them from. The reader must be built
// after every knob is set, because the child environment is snapshotted at
// construction.
func fakeKeychain(t *testing.T) *testutil.Fixture {
	t.Helper()
	f := testutil.New(t).WithKeychain()
	t.Setenv("AGCTL_FAKE_SECURITY_LOG", f.SecurityLogPath())
	t.Setenv("AGCTL_FAKE_SECURITY_ITEMS", f.ItemsDir())
	t.Setenv("AGCTL_FAKE_SECURITY_DUMP", f.KeychainDumpPath())
	return f
}

// standInReader builds the transport over the installed stand-in, the same
// way the tagged factory would wire it.
func standInReader(f *testutil.Fixture) *securityCLI {
	return newSecurityCLI(f.SecurityBin(), testutil.KeychainAccount, testingChildEnv())
}

func TestStandInPreflight(t *testing.T) {
	tests := map[string]struct {
		exit     string
		stderr   string
		want     KeychainState
		inReason string
	}{
		"success: a default preflight is unlocked": {
			want: KeychainStateUnlocked,
		},
		"error: exit 36 is locked": {
			exit: "36",
			want: KeychainStateLocked,
		},
		"error: a locked message without exit 36 still reads locked": {
			exit:   "1",
			stderr: "please unlock the keychain first",
			want:   KeychainStateLocked,
		},
		"error: a preflight failure is unavailable with the class named": {
			exit:     "1",
			stderr:   "security: unable to open the keychain",
			want:     KeychainStateUnavailable,
			inReason: "KeychainUnavailable",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := fakeKeychain(t)
			if tt.exit != "" {
				t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT", tt.exit)
			}
			if tt.stderr != "" {
				t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_STDERR", tt.stderr)
			}
			status := standInReader(f).Preflight(t.Context())
			if status.State != tt.want {
				t.Fatalf("Preflight() = %+v, want state %v", status, tt.want)
			}
			if tt.inReason != "" && !strings.Contains(status.Reason, tt.inReason) {
				t.Errorf("Preflight() reason = %q, want it to contain %q", status.Reason, tt.inReason)
			}
		})
	}
}

func TestStandInReadMissingItemIsNotFoundNotAFailure(t *testing.T) {
	f := fakeKeychain(t)
	sec, err := standInReader(f).Read(t.Context(), "Claude Code-credentials")
	if sec != nil {
		t.Errorf("Read() secret = %v, want nil", sec)
	}
	if !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("Read() error = %v, want %v", err, ErrItemNotFound)
	}
	if got := KeychainItem("Claude Code-credentials").Classify(err); got.Kind != OutcomeAbsent {
		t.Errorf("Classify() = %+v, want the absent outcome: a missing item means needs-login", got)
	}
}

func TestStandInReadSealsTheItemWithoutItsTrailingNewline(t *testing.T) {
	f := fakeKeychain(t)
	f.KeychainItem("Claude Code-credentials", `{"a":1}`+"\n")

	sec, err := standInReader(f).Read(t.Context(), "Claude Code-credentials")
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	var got []byte
	if err := sec.WithPlaintext(func(b []byte) error {
		got = append(got, b...)
		return nil
	}); err != nil {
		t.Fatalf("WithPlaintext() error = %v", err)
	}
	if diff := gocmp.Diff([]byte(`{"a":1}`), got); diff != "" {
		t.Errorf("payload mismatch (-want +got):\n%s", diff)
	}
}

func TestStandInListServicesParsesTheFixtureDumpAndFiltersByPrefix(t *testing.T) {
	f := fakeKeychain(t)
	dump, err := fixtures.FS.ReadFile("claude/security-dump.txt")
	if err != nil {
		t.Fatalf("read the dump fixture: %v", err)
	}
	if err := os.WriteFile(f.KeychainDumpPath(), dump, 0o644); err != nil {
		t.Fatalf("install the dump fixture: %v", err)
	}
	reader := standInReader(f)

	claude, err := reader.ListServices(t.Context(), "Claude Code")
	if err != nil {
		t.Fatalf("ListServices(Claude Code) error = %v", err)
	}
	if len(claude) != 4 {
		t.Errorf("ListServices(Claude Code) = %d entries, want 4: three credential items plus the legacy key", len(claude))
	}

	switcher, err := reader.ListServices(t.Context(), "claude-switcher:")
	if err != nil {
		t.Fatalf("ListServices(claude-switcher:) error = %v", err)
	}
	if len(switcher) != 2 {
		t.Errorf("ListServices(claude-switcher:) = %d entries, want 2", len(switcher))
	}

	var dumps int
	for _, line := range f.SecurityLog() {
		if strings.HasPrefix(line, "dump-keychain") {
			dumps++
		}
	}
	if dumps != 1 {
		t.Errorf("two prefixes took %d dumps, want one ten-second dump and a memo", dumps)
	}
}

func TestStandInListServicesUncachedTakesASecondDump(t *testing.T) {
	f := fakeKeychain(t)
	reader := standInReader(f)

	first, err := reader.ListServices(t.Context(), "Codex Auth")
	if err != nil {
		t.Fatalf("listing #1 error = %v", err)
	}
	if len(first) != 0 {
		t.Fatalf("listing #1 = %v, want nothing before the item appears", first)
	}

	// Another process "creates" an item between the two listings; a memo
	// would hide it and void any before/after comparison.
	f.Dump("Codex Auth")

	second, err := reader.ListServicesUncached(t.Context(), "Codex Auth")
	if err != nil {
		t.Fatalf("listing #2 error = %v", err)
	}
	want := []ServiceEntry{{
		Service:    "Codex Auth",
		Account:    testutil.KeychainAccount,
		CreatedAt:  "20260908012005Z",
		ModifiedAt: "20260908012005Z",
	}}
	if diff := gocmp.Diff(want, second); diff != "" {
		t.Errorf("listing #2 mismatch (-want +got):\n%s", diff)
	}

	var dumps int
	for _, line := range f.SecurityLog() {
		if strings.HasPrefix(line, "dump-keychain") {
			dumps++
		}
	}
	if dumps != 2 {
		t.Errorf("recorded %d dump-keychain invocations, want 2, not one and a memo", dumps)
	}
}

func TestStandInDumpFailureIsClassifiedNotEmpty(t *testing.T) {
	f := fakeKeychain(t)
	t.Setenv("AGCTL_FAKE_SECURITY_DUMP_EXIT", "36")

	entries, err := standInReader(f).ListServices(t.Context(), "Claude Code")
	if entries != nil {
		t.Errorf("ListServices() = %v, want nothing on a failed dump", entries)
	}
	if !errors.Is(err, ErrKeychainLocked) {
		t.Fatalf("ListServices() error = %v, want %v", err, ErrKeychainLocked)
	}
}

func TestStandInArgvMatchesTheReadVocabulary(t *testing.T) {
	f := fakeKeychain(t)
	f.KeychainItem("Claude Code-credentials", fakeToken)
	reader := standInReader(f)
	ctx := t.Context()

	reader.Preflight(ctx)
	if _, err := reader.ListServices(ctx, "Claude Code"); err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	if _, err := reader.Read(ctx, "Claude Code-credentials"); err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	want := []string{
		"show-keychain-info",
		"dump-keychain",
		"find-generic-password -a example -w -s Claude Code-credentials",
	}
	if diff := gocmp.Diff(want, f.SecurityLog()); diff != "" {
		t.Errorf("argv log mismatch (-want +got):\n%s", diff)
	}
	f.AssertKeychainReadOnly()
}

func TestStandInHangingCallsAreKilledAtTheirBudget(t *testing.T) {
	f := fakeKeychain(t)
	t.Setenv("AGCTL_FAKE_SECURITY_SLEEP", "2")
	reader := standInReader(f)
	// Shrunk budgets so the test proves the boundary without waiting out the
	// production two seconds.
	reader.readBudget = 150 * time.Millisecond
	reader.dumpBudget = 150 * time.Millisecond

	start := time.Now()
	status := reader.Preflight(t.Context())
	elapsed := time.Since(start)
	if status.State != KeychainStateTimeout {
		t.Fatalf("Preflight() = %+v, want the timeout state", status)
	}
	if elapsed < reader.readBudget {
		t.Errorf("preflight returned after %v, before its %v budget", elapsed, reader.readBudget)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("preflight overran its budget: %v", elapsed)
	}

	sec, err := reader.Read(t.Context(), "Claude Code-credentials")
	if sec != nil {
		t.Errorf("Read() secret = %v, want nil on a timeout", sec)
	}
	if !errors.Is(err, ErrKeychainTimeout) {
		t.Fatalf("Read() error = %v, want %v: a hanging read is a timeout, not an absent item", err, ErrKeychainTimeout)
	}
	var kerr *KeychainError
	if !errors.As(err, &kerr) || !kerr.Transient {
		t.Errorf("Read() error = %#v, want a transient failure", err)
	}
	if got := KeychainItem("Claude Code-credentials").Classify(err); got.Kind != OutcomeTransient {
		t.Errorf("Classify() = %+v, want transient: a timeout never falls through to the file", got)
	}
}

func TestStandInMissingBinaryIsUnavailableAndNotTransient(t *testing.T) {
	fakeKeychain(t)
	reader := newSecurityCLI("/nonexistent/security", testutil.KeychainAccount, testingChildEnv())

	sec, err := reader.Read(t.Context(), "anything")
	if sec != nil {
		t.Errorf("Read() secret = %v, want nil", sec)
	}
	if !errors.Is(err, ErrKeychainUnavailable) {
		t.Fatalf("Read() error = %v, want %v", err, ErrKeychainUnavailable)
	}
	var kerr *KeychainError
	if !errors.As(err, &kerr) || kerr.Transient {
		t.Errorf("Read() error = %#v; a missing binary will not fix itself", err)
	}
}

func TestStandInClassifiedReadFailures(t *testing.T) {
	tests := map[string]struct {
		exit   string
		stderr string
		target *KeychainError
	}{
		"error: exit 36 on a read is locked": {
			exit:   "36",
			target: ErrKeychainLocked,
		},
		"error: an interaction refusal carries its class verbatim": {
			exit:   "1",
			stderr: "errSecInteractionNotAllowed",
			target: &KeychainError{Class: errs.KeychainClass("InteractionNotAllowed")},
		},
		"error: an unopenable keychain is unavailable": {
			exit:   "1",
			stderr: "security: unable to open the keychain",
			target: ErrKeychainUnavailable,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := fakeKeychain(t)
			t.Setenv("AGCTL_FAKE_SECURITY_FIND_EXIT", tt.exit)
			if tt.stderr != "" {
				t.Setenv("AGCTL_FAKE_SECURITY_STDERR", tt.stderr)
			}
			_, err := standInReader(f).Read(t.Context(), "Claude Code-credentials")
			if !errors.Is(err, tt.target) {
				t.Fatalf("Read() error = %v, want class %q", err, tt.target.Class)
			}
		})
	}
}

func TestStandInNoTokenBytesEscapeTheSealedSecret(t *testing.T) {
	f := fakeKeychain(t)
	f.KeychainItem("Claude Code-credentials", fakeToken)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reader := standInReader(f)
	ctx := t.Context()

	// A successful read seals the token; everything the operation leaves
	// behind — renderings of the secret, the argv log, diagnostics — must be
	// free of it.
	sec, err := reader.Read(ctx, "Claude Code-credentials")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	logger.Info("read one item", slog.Any("secret", sec), slog.Int("len", sec.Len()))
	renderings := []string{
		fmt.Sprintf("%v %s %#v %+v", sec, sec, sec, sec),
		fmt.Sprint(sec.String(), sec.GoString()),
	}

	// A failed read after the same item exists: the classified error, its
	// renderings and the log line built from it must be free of it too.
	t.Setenv("AGCTL_FAKE_SECURITY_FIND_EXIT", "1")
	t.Setenv("AGCTL_FAKE_SECURITY_STDERR", "errSecAuthFailed")
	failing := standInReader(f)
	_, rerr := failing.Read(ctx, "Claude Code-credentials")
	if rerr == nil {
		t.Fatal("Read() error = nil, want a classified failure")
	}
	logger.Error("read failed", slog.Any("err", rerr))
	renderings = append(renderings, rerr.Error(), fmt.Sprintf("%v %+v %#v", rerr, rerr, rerr))

	// A timed-out read, whose partial stdout was wiped before the error was
	// built.
	t.Setenv("AGCTL_FAKE_SECURITY_FIND_EXIT", "")
	t.Setenv("AGCTL_FAKE_SECURITY_STDERR", "")
	t.Setenv("AGCTL_FAKE_SECURITY_SLEEP", "2")
	timing := standInReader(f)
	timing.readBudget = 150 * time.Millisecond
	_, terr := timing.Read(ctx, "Claude Code-credentials")
	if terr == nil {
		t.Fatal("Read() error = nil, want a timeout")
	}
	logger.Error("read timed out", slog.Any("err", terr))
	renderings = append(renderings, terr.Error())

	needles := []string{fakeToken, "sk-ant-"}
	for _, needle := range needles {
		for _, rendering := range renderings {
			if strings.Contains(rendering, needle) {
				t.Errorf("a rendering carries %q: %q", needle, rendering)
			}
		}
		if strings.Contains(logBuf.String(), needle) {
			t.Errorf("the slog output carries %q:\n%s", needle, logBuf.String())
		}
		for _, line := range f.SecurityLog() {
			if strings.Contains(line, needle) {
				t.Errorf("the argv log carries %q: %s", needle, line)
			}
		}
	}
	if !strings.Contains(logBuf.String(), "[REDACTED]") {
		t.Errorf("the logged secret did not render redacted:\n%s", logBuf.String())
	}
	f.AssertKeychainReadOnly()
}
