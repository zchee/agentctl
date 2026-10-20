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

package testutil

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/fixtures"
)

func TestWithKeychainInstallsTheScriptVerbatim(t *testing.T) {
	f := New(t).WithKeychain()

	want, err := fixtures.FS.ReadFile("fake-security.sh")
	if err != nil {
		t.Fatalf("read the embedded script: %v", err)
	}
	got, err := os.ReadFile(f.SecurityBin())
	if err != nil {
		t.Fatalf("read the installed script: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("the installed script differs from the embedded one (%d vs %d bytes)", len(got), len(want))
	}
	if mode := ModeOf(t, f.SecurityBin()); mode != 0o755 {
		t.Fatalf("installed script mode = %04o, want 0755 (embedding drops the bit; the installer must restore it)", mode)
	}
	if _, wired := f.Lookup("AGENTCTL_KEYCHAIN_BACKEND"); wired {
		t.Fatalf("the disabled backend is still wired after WithKeychain")
	}
	if bin, _ := f.Lookup("AGENTCTL_SECURITY_BIN"); bin != f.SecurityBin() {
		t.Fatalf("AGENTCTL_SECURITY_BIN = %q, want %q", bin, f.SecurityBin())
	}
	if _, err := os.Stat(f.KeychainDumpPath()); err != nil {
		t.Fatalf("WithKeychain left no empty dump: %v", err)
	}
}

func TestInstalledScriptRunsAndLogs(t *testing.T) {
	f := New(t).WithKeychain()

	cmd := exec.CommandContext(t.Context(), f.SecurityBin(), "show-keychain-info", "login.keychain")
	f.Apply(cmd)
	out := Run(t, cmd)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("show-keychain-info exited %d; stderr:\n%s", code, out.Stderr)
	}

	log := f.SecurityLog()
	if len(log) != 1 || log[0] != "show-keychain-info login.keychain" {
		t.Fatalf("security log = %q, want exactly the one argv line", log)
	}
	f.AssertKeychainReadOnly()
}

func TestDumpListsServicesInTheToolsFormat(t *testing.T) {
	f := New(t).WithKeychain()
	f.Dump("Claude Code-credentials")

	cmd := exec.CommandContext(t.Context(), f.SecurityBin(), "dump-keychain", "login.keychain")
	f.Apply(cmd)
	out := Run(t, cmd)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("dump-keychain exited %d; stderr:\n%s", code, out.Stderr)
	}
	for _, needle := range []string{`"svce"<blob>="Claude Code-credentials"`, `"acct"<blob>="` + KeychainAccount + `"`, `class: "genp"`} {
		if !strings.Contains(out.Stdout, needle) {
			t.Fatalf("dump output lacks %q:\n%s", needle, out.Stdout)
		}
	}
}

func TestFindReadsAPlantedItem(t *testing.T) {
	f := New(t).WithKeychain()
	f.KeychainItem("svc-1", "the item bytes")

	cmd := exec.CommandContext(t.Context(), f.SecurityBin(), "find-generic-password", "-s", "svc-1", "-a", KeychainAccount, "-w")
	f.Apply(cmd)
	out := Run(t, cmd)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("find-generic-password exited %d; stderr:\n%s", code, out.Stderr)
	}
	if out.Stdout != "the item bytes" {
		t.Fatalf("find served %q, want the planted bytes", out.Stdout)
	}
}

func TestFindUnderAnotherAccountServesNothing(t *testing.T) {
	f := New(t).WithKeychain()
	f.KeychainItem("svc-1", "the item bytes")

	cmd := exec.CommandContext(t.Context(), f.SecurityBin(), "find-generic-password", "-s", "svc-1", "-a", "somebody-else", "-w")
	f.Apply(cmd)
	out := Run(t, cmd)
	if code := out.ExitCode(t); code != 44 {
		t.Fatalf("a find under the wrong account exited %d, want 44; stdout:\n%s", code, out.Stdout)
	}
}

func TestSecurityWriteStoresAnAllowedItem(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")

	// 68656c6c6f is "hello" in lowercase hexadecimal.
	out := f.SecurityWrite(`add-generic-password -U -a "` + KeychainAccount + `" -s "svc-1" -X "68656c6c6f"`)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("the write exited %d; stderr:\n%s", code, out.Stderr)
	}

	data, err := os.ReadFile(f.KeychainItemPath("svc-1"))
	if err != nil {
		t.Fatalf("read the stored item: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("the stored item holds %q, want the decoded payload", data)
	}

	// The stand-in logs its argv on entry and the parsed write line after,
	// with the payload redacted down to its length in both.
	log := f.SecurityLog()
	if len(log) != 2 || log[0] != "-i" || !strings.Contains(log[1], "<REDACTED:10>") {
		t.Fatalf("security log = %q, want the argv line and one redacted write line", log)
	}
	for _, line := range log {
		if strings.Contains(line, "68656c6c6f") {
			t.Fatalf("the payload reached the log unredacted: %q", line)
		}
	}
	items := f.KeychainItems()
	if len(items) != 1 || items[0].Name != KeychainAccount+"/svc-1" {
		t.Fatalf("keychain items = %v, want exactly the one written item", items)
	}
}

func TestSecurityWriteRefusesAnUnregisteredService(t *testing.T) {
	f := New(t).WithKeychain()

	out := f.SecurityWrite(`add-generic-password -U -a "` + KeychainAccount + `" -s "svc-unregistered" -X "ff"`)
	if code := out.ExitCode(t); code == 0 {
		t.Fatalf("a write to an unregistered service succeeded; stdout:\n%s", out.Stdout)
	}
	if _, err := os.Stat(f.KeychainItemPath("svc-unregistered")); err == nil {
		t.Fatalf("the refused write still stored an item")
	}
}

// helloHex is "hello" in lowercase hexadecimal, the payload the argv write
// tests store.
const helloHex = "68656c6c6f"

// startInOwnGroup starts cmd as the leader of a new process group and kills
// the whole group when the test ends. The fake's delays run a foreground
// sleep and the pipe holder runs a background one; either outlives a leader
// that is killed or exits first, and only a group kill removes it, which is
// what the production writer does after it reaps the leader.
func startInOwnGroup(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the stand-in: %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = unix.Kill(-pgid, unix.SIGKILL) })
}

// waitInBackground waits for a started cmd on its own goroutine and closes
// the returned channel once the wait returns, so a test can ask whether the
// child is still running without blocking on it.
func waitInBackground(cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

// readCount reads the decimal number one of the fake's pid or heartbeat
// files holds. It reports false while the file is absent or mid-write.
func readCount(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// itemHolds reports whether the fixture's own item for service holds want.
func itemHolds(f *Fixture, service, want string) bool {
	data, err := os.ReadFile(f.KeychainItemPath(service))
	return err == nil && string(data) == want
}

func TestSecurityWriteArgvStoresAnAllowedItem(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")

	out := f.SecurityWriteArgv(KeychainAccount, "svc-1", helloHex)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("the write exited %d; stderr:\n%s", code, out.Stderr)
	}
	if !itemHolds(f, "svc-1", "hello") {
		t.Fatalf("the stored item does not hold the decoded payload")
	}

	// The argv carries the payload, so the stand-in logs the redacted line in
	// place of its argv: one line, the payload reduced to its length.
	want := []string{`add-generic-password -U -a "` + KeychainAccount + `" -s "svc-1" -X <REDACTED:10>`}
	if diff := gocmp.Diff(want, f.SecurityLog()); diff != "" {
		t.Fatalf("security log mismatch (-want +got):\n%s", diff)
	}
	items := f.KeychainItems()
	if len(items) != 1 || items[0].Name != KeychainAccount+"/svc-1" {
		t.Fatalf("keychain items = %v, want exactly the one written item", items)
	}
}

func TestSecurityWriteArgvUnderAnotherAccountLandsInASibling(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")

	out := f.SecurityWriteArgv("somebody-else", "svc-1", helloHex)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("the write exited %d; stderr:\n%s", code, out.Stderr)
	}
	if _, err := os.Stat(f.KeychainItemPath("svc-1")); err == nil {
		t.Fatalf("a write under another account reached this fixture's own item")
	}
	data, err := os.ReadFile(f.KeychainItemPathFor("somebody-else", "svc-1"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("the sibling item holds %q (err %v), want the decoded payload", data, err)
	}
}

func TestSecurityWriteArgvRefuses(t *testing.T) {
	tests := map[string]struct {
		args []string
		env  map[string]string
	}{
		"error: no -U": {
			args: []string{"-a", KeychainAccount, "-s", "svc-1", "-X", helloHex},
		},
		"error: a keychain argument after the payload": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-1", "-X", helloHex, "login.keychain"},
		},
		"error: service before account": {
			args: []string{"-U", "-s", "svc-1", "-a", KeychainAccount, "-X", helloHex},
		},
		"error: empty account": {
			args: []string{"-U", "-a", "", "-s", "svc-1", "-X", helloHex},
		},
		"error: -w in place of -X": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-1", "-w", helloHex},
		},
		"error: unregistered service": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-unregistered", "-X", helloHex},
		},
		"error: uppercase hexadecimal": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-1", "-X", "68656C6C6F"},
		},
		"error: odd number of hexadecimal digits": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-1", "-X", helloHex + "0"},
		},
		"error: empty payload": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-1", "-X", ""},
		},
		"error: a delay that is not a count": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-1", "-X", helloHex},
			env:  map[string]string{"AGCTL_FAKE_WRITE_DELAY_MS": "1s"},
		},
		"error: a pipe hold that is not a count": {
			args: []string{"-U", "-a", KeychainAccount, "-s", "svc-1", "-X", helloHex},
			env:  map[string]string{"AGCTL_FAKE_SECURITY_HOLD_PIPES": "yes"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := New(t).WithKeychain()
			f.AllowWrite("svc-1")
			for key, value := range tt.env {
				f.Set(key, value)
			}

			cmd := exec.CommandContext(t.Context(), f.SecurityBin(), append([]string{"add-generic-password"}, tt.args...)...)
			f.Apply(cmd)
			out := Run(t, cmd)
			if code := out.ExitCode(t); code == 0 {
				t.Fatalf("the write succeeded; stdout:\n%s", out.Stdout)
			}
			if items := f.KeychainItems(); len(items) != 0 {
				t.Fatalf("the refused write stored %v", items)
			}
			log := f.SecurityLog()
			if len(log) != 1 || !strings.HasPrefix(log[0], "add-generic-password ") {
				t.Fatalf("security log = %q, want one add-generic-password line", log)
			}
			for _, secret := range []string{helloHex, strings.ToUpper(helloHex)} {
				if strings.Contains(log[0], secret) {
					t.Fatalf("the payload reached the log unredacted: %q", log[0])
				}
			}
		})
	}
}

func TestSecurityWriteArgvForcedExit(t *testing.T) {
	tests := map[string]struct {
		exit string
		want int
	}{
		"error: exit 1":  {exit: "1", want: 1},
		"error: exit 45": {exit: "45", want: 45},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			const canary = "stderr-canary-7f3a"
			f := New(t).WithKeychain()
			f.AllowWrite("svc-1")
			f.Set("AGCTL_FAKE_SECURITY_WRITE_EXIT", tt.exit)
			f.Set("AGCTL_FAKE_SECURITY_STDERR", canary)

			out := f.SecurityWriteArgv(KeychainAccount, "svc-1", helloHex)
			if code := out.ExitCode(t); code != tt.want {
				t.Fatalf("the write exited %d, want %d; stderr:\n%s", code, tt.want, out.Stderr)
			}
			if !strings.Contains(out.Stderr, canary) {
				t.Fatalf("stderr = %q, want the canary", out.Stderr)
			}
			if strings.Contains(out.Stdout, canary) {
				t.Fatalf("the canary reached stdout: %q", out.Stdout)
			}
			if items := f.KeychainItems(); len(items) != 0 {
				t.Fatalf("a forced failure stored %v", items)
			}
			// The write is logged before the forced exit, so a test counting
			// issued writes still sees it.
			if log := f.SecurityLog(); len(log) != 1 || !strings.HasSuffix(log[0], "-X <REDACTED:10>") {
				t.Fatalf("security log = %q, want the one redacted write line", log)
			}
		})
	}
}

func TestSecurityWriteArgvDelaysBeforeStoring(t *testing.T) {
	const delay = 400 * time.Millisecond
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")
	f.Set("AGCTL_FAKE_WRITE_DELAY_MS", strconv.Itoa(int(delay.Milliseconds())))

	cmd := f.SecurityWriteArgvCommand(KeychainAccount, "svc-1", helloHex)
	stdout, stderr := Capture(cmd)
	start := time.Now()
	startInOwnGroup(t, cmd)

	// The log line is written on entry, before the delay starts.
	if !WaitUntil(5*time.Second, func() bool { return len(f.SecurityLog()) == 1 }) {
		t.Fatalf("the stand-in never logged the write")
	}
	if _, err := os.Stat(f.KeychainItemPath("svc-1")); err == nil {
		t.Fatalf("the item was stored before the delay elapsed")
	}

	out := Finish(t, cmd, stdout, stderr)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("the delayed write exited %d; stderr:\n%s", code, out.Stderr)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("the delayed write returned after %v, want at least %v", elapsed, delay)
	}
	if !itemHolds(f, "svc-1", "hello") {
		t.Fatalf("the delayed write did not store the decoded payload")
	}
}

func TestSecurityWriteArgvKilledDuringTheDelayStoresNothing(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")
	f.Set("AGCTL_FAKE_WRITE_DELAY_MS", "30000")

	cmd := f.SecurityWriteArgvCommand(KeychainAccount, "svc-1", helloHex)
	startInOwnGroup(t, cmd)
	done := waitInBackground(cmd)
	if !WaitUntil(5*time.Second, func() bool { return len(f.SecurityLog()) == 1 }) {
		t.Fatalf("the stand-in never logged the write")
	}

	// The writer's timeout kills the leader, then the group; the item must be
	// untouched either way.
	if err := unix.Kill(-cmd.Process.Pid, unix.SIGKILL); err != nil {
		t.Fatalf("kill the stand-in's group: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the stand-in outlived a group kill")
	}
	if cmd.ProcessState.Exited() {
		t.Fatalf("the stand-in exited %d before the kill, want it still in its delay", cmd.ProcessState.ExitCode())
	}
	if _, err := os.Stat(f.KeychainItemPath("svc-1")); err == nil {
		t.Fatalf("a write killed during its delay stored the item")
	}
}

func TestSecurityWriteArgvDelaysAfterStoring(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")
	f.Set("AGCTL_FAKE_WRITE_DELAY_AFTER_MS", "30000")

	cmd := f.SecurityWriteArgvCommand(KeychainAccount, "svc-1", helloHex)
	startInOwnGroup(t, cmd)
	done := waitInBackground(cmd)

	// The item is committed while the stand-in is still running, which is
	// what lets a timeout land after the write.
	if !WaitUntil(5*time.Second, func() bool { return itemHolds(f, "svc-1", "hello") }) {
		t.Fatalf("the item was never stored")
	}
	select {
	case err := <-done:
		t.Fatalf("the stand-in returned (%v) right after storing, want it held in the delay", err)
	default:
	}

	if err := unix.Kill(-cmd.Process.Pid, unix.SIGKILL); err != nil {
		t.Fatalf("kill the stand-in's group: %v", err)
	}
	<-done
	if !itemHolds(f, "svc-1", "hello") {
		t.Fatalf("the stored item changed after the kill")
	}
}

func TestSecurityWriteArgvHoldPipesOutlivesTheLeader(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")
	f.Set("AGCTL_FAKE_SECURITY_HOLD_PIPES", "30")

	cmd := f.SecurityWriteArgvCommand(KeychainAccount, "svc-1", helloHex)
	Capture(cmd)
	cmd.WaitDelay = 300 * time.Millisecond
	startInOwnGroup(t, cmd)
	pgid := cmd.Process.Pid

	// The leader exits 0 at once, but the holder keeps both pipes open, so
	// the wait gives up on them after its delay.
	err := cmd.Wait()
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("wait = %v, want %v", err, exec.ErrWaitDelay)
	}
	if !cmd.ProcessState.Exited() || cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("the leader ended with %v, want exit 0", cmd.ProcessState)
	}
	if !itemHolds(f, "svc-1", "hello") {
		t.Fatalf("the item was not stored before the leader exited")
	}

	// The holder is still a member of the leader's group, so the group kill
	// that follows a reap removes it.
	if err := unix.Kill(-pgid, 0); err != nil {
		t.Fatalf("no member of the stand-in's group is left after the leader exited: %v", err)
	}
	if err := unix.Kill(-pgid, unix.SIGKILL); err != nil {
		t.Fatalf("kill the stand-in's group: %v", err)
	}
	if !WaitUntil(5*time.Second, func() bool { return errors.Is(unix.Kill(-pgid, 0), unix.ESRCH) }) {
		t.Fatalf("the pipe holder outlived a group kill")
	}
}

func TestSecurityWriteArgvPidfileHeartbeat(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")
	pidfile := filepath.Join(f.Root(), "security.pid")
	f.Set("AGCTL_FAKE_SECURITY_PIDFILE", pidfile)
	f.Set("AGCTL_FAKE_WRITE_DELAY_MS", "100")
	f.Set("AGCTL_FAKE_WRITE_DELAY_AFTER_MS", "50")

	cmd := f.SecurityWriteArgvCommand(KeychainAccount, "svc-1", helloHex)
	out := Run(t, cmd)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("the write exited %d; stderr:\n%s", code, out.Stderr)
	}
	if pid, ok := readCount(pidfile); !ok || pid != cmd.Process.Pid {
		t.Fatalf("pidfile holds %d (readable %t), want the leader's pid %d", pid, ok, cmd.Process.Pid)
	}
	// One beat per 10 ms step, counted across both delays.
	if beats, ok := readCount(pidfile + ".heartbeat"); !ok || beats != 15 {
		t.Fatalf("heartbeat holds %d (readable %t), want 15", beats, ok)
	}
	if !itemHolds(f, "svc-1", "hello") {
		t.Fatalf("the write did not store the decoded payload")
	}
}

func TestSecurityWriteArgvHeartbeatStopsWithTheLeader(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc-1")
	pidfile := filepath.Join(f.Root(), "security.pid")
	heartbeat := pidfile + ".heartbeat"
	f.Set("AGCTL_FAKE_SECURITY_PIDFILE", pidfile)
	f.Set("AGCTL_FAKE_WRITE_DELAY_MS", "30000")

	cmd := f.SecurityWriteArgvCommand(KeychainAccount, "svc-1", helloHex)
	startInOwnGroup(t, cmd)
	done := waitInBackground(cmd)

	if !WaitUntil(5*time.Second, func() bool { beats, ok := readCount(heartbeat); return ok && beats >= 3 }) {
		t.Fatalf("the heartbeat never advanced")
	}
	if pid, ok := readCount(pidfile); !ok || pid != cmd.Process.Pid {
		t.Fatalf("pidfile holds %d (readable %t), want the leader's pid %d", pid, ok, cmd.Process.Pid)
	}

	// The signal teardown terminates the leader only. A heartbeat written by
	// anything other than the leader's own loop would keep going after this.
	SendSIGTERM(t, cmd.Process.Pid)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the leader outlived SIGTERM")
	}
	if cmd.ProcessState.Exited() {
		t.Fatalf("the leader exited %d, want it ended by SIGTERM", cmd.ProcessState.ExitCode())
	}
	last, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatalf("read the heartbeat: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatalf("read the heartbeat again: %v", err)
	}
	if !bytes.Equal(last, after) {
		t.Fatalf("the heartbeat moved from %q to %q after the leader was killed", last, after)
	}
	if _, err := os.Stat(f.KeychainItemPath("svc-1")); err == nil {
		t.Fatalf("a write killed during its delay stored the item")
	}
}

func TestItemFileName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		service string
		want    string
	}{
		"success: safe characters survive":   {service: "Claude.Code_cred-1", want: "Claude.Code_cred-1"},
		"success: spaces fold to underscore": {service: "Claude Code-credentials", want: "Claude_Code-credentials"},
		"success: slashes fold":              {service: "a/b\\c", want: "a_b_c"},
		"success: multibyte folds per byte":  {service: "svc-\u00e9", want: "svc-_"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := ItemFileName(tt.service); got != tt.want {
				t.Fatalf("ItemFileName(%q) = %q, want %q", tt.service, got, tt.want)
			}
		})
	}
}

func TestItemFileNameMatchesTheScriptFold(t *testing.T) {
	f := New(t).WithKeychain()
	f.AllowWrite("svc with spaces/and slash")

	out := f.SecurityWrite(`add-generic-password -U -a "` + KeychainAccount + `" -s "svc with spaces/and slash" -X "ff"`)
	if code := out.ExitCode(t); code != 0 {
		t.Fatalf("the write exited %d; stderr:\n%s", code, out.Stderr)
	}
	if _, err := os.Stat(f.KeychainItemPath("svc with spaces/and slash")); err != nil {
		t.Fatalf("the fold disagrees with the script: %v", err)
	}
}

func TestWithCodexInstallsTheScript(t *testing.T) {
	f := New(t).WithCodex()

	want, err := fixtures.FS.ReadFile("fake-codex.sh")
	if err != nil {
		t.Fatalf("read the embedded script: %v", err)
	}
	got, err := os.ReadFile(f.CodexBin())
	if err != nil {
		t.Fatalf("read the installed script: %v", err)
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("installed script mismatch (-want +got):\n%s", diff)
	}
	if mode := ModeOf(t, f.CodexBin()); mode != 0o755 {
		t.Fatalf("installed script mode = %04o, want 0755", mode)
	}
	if bin, _ := f.Lookup("AGENTCTL_CODEX_BIN"); bin != f.CodexBin() {
		t.Fatalf("AGENTCTL_CODEX_BIN = %q, want %q", bin, f.CodexBin())
	}
	if dir := f.CodexHome("scratch"); !strings.HasPrefix(dir, f.Root()) {
		t.Fatalf("CodexHome(%q) = %q escapes the fixture", "scratch", dir)
	}
}
