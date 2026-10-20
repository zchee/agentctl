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

	out := f.SecurityWriteArgv(KeychainAccount, "svc with spaces/and slash", "ff")
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

// installedShells returns every POSIX shell on this host that the fake can
// be run under, so its arithmetic and parameter expansion are checked under
// dash (Linux's /bin/sh) as well as bash and macOS's /bin/sh.
func installedShells(t *testing.T) []string {
	t.Helper()
	var shells []string
	for _, shell := range []string{"/bin/sh", "/bin/dash", "/bin/bash"} {
		if _, err := os.Stat(shell); err == nil {
			shells = append(shells, shell)
		}
	}
	if len(shells) == 0 {
		t.Fatalf("no POSIX shell found to run the fake under")
	}
	return shells
}

// securityUnder returns an unstarted command that runs the installed fake
// through shell with args, in the fixture's environment.
func securityUnder(t *testing.T, f *Fixture, shell string, args ...string) *exec.Cmd {
	t.Helper()
	return f.Apply(exec.CommandContext(t.Context(), shell, append([]string{f.SecurityBin()}, args...)...))
}

// traceSleep puts a sleep stand-in first on the fixture's PATH that records
// its argument and returns at once, and returns the trace file. It turns
// the fake's delays into an exact record of what it asked to sleep, instead
// of a lower bound measured on a clock.
func traceSleep(t *testing.T, f *Fixture) string {
	t.Helper()
	dir := filepath.Join(f.Root(), "sleep-trace-bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(f.Root(), "sleep-trace")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SLEEP_TRACE\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.Set("SLEEP_TRACE", trace)
	f.Set("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return trace
}

// traceLines returns the lines of a trace file, nil when it is absent.
func traceLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestFakeSecurityReadsDelayCountsAsDecimal(t *testing.T) {
	tests := map[string]struct {
		knob      string
		value     string
		pidfile   bool
		wantSleep []string
		wantBeats int
	}{
		"success: a zero-padded delay is eight milliseconds, not an octal error": {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "08", wantSleep: []string{"0.008"}},
		"success: a zero-padded ten is ten, not octal eight":                     {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "010", wantSleep: []string{"0.010"}},
		"success: an all-zero delay is zero":                                     {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "000", wantSleep: []string{"0.000"}},
		"success: a zero-padded delay after storing is decimal":                  {knob: "AGCTL_FAKE_WRITE_DELAY_AFTER_MS", value: "0019", wantSleep: []string{"0.019"}},
		"success: a zero-padded hold is decimal seconds":                         {knob: "AGCTL_FAKE_SECURITY_HOLD_PIPES", value: "08", wantSleep: []string{"8"}},
		"success: a zero-padded delay with a heartbeat is decimal":               {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "08", pidfile: true, wantSleep: []string{"0.008"}, wantBeats: 1},
		"success: a zero-padded ten with a heartbeat is one whole step":          {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "010", pidfile: true, wantSleep: []string{"0.01"}, wantBeats: 1},
		"success: an all-zero delay with a heartbeat neither beats nor sleeps":   {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "000", pidfile: true},
		"success: one millisecond still beats once and sleeps it":                {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "1", pidfile: true, wantSleep: []string{"0.001"}, wantBeats: 1},
		"success: nine milliseconds are one beat and the remainder":              {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "9", pidfile: true, wantSleep: []string{"0.009"}, wantBeats: 1},
		"success: ten milliseconds are one whole step":                           {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "10", pidfile: true, wantSleep: []string{"0.01"}, wantBeats: 1},
		"success: nineteen milliseconds are a step and a nine millisecond rest":  {knob: "AGCTL_FAKE_WRITE_DELAY_MS", value: "19", pidfile: true, wantSleep: []string{"0.01", "0.009"}, wantBeats: 2},
		"success: a remainder after storing is slept too":                        {knob: "AGCTL_FAKE_WRITE_DELAY_AFTER_MS", value: "25", pidfile: true, wantSleep: []string{"0.01", "0.01", "0.005"}, wantBeats: 3},
	}

	for _, shell := range installedShells(t) {
		for name, tt := range tests {
			t.Run(filepath.Base(shell)+"/"+name, func(t *testing.T) {
				f := New(t).WithKeychain()
				f.AllowWrite("svc-1")
				trace := traceSleep(t, f)
				f.Set(tt.knob, tt.value)
				pidfile := filepath.Join(f.Root(), "security.pid")
				if tt.pidfile {
					f.Set("AGCTL_FAKE_SECURITY_PIDFILE", pidfile)
				}

				out := Run(t, securityUnder(t, f, shell, "add-generic-password", "-U", "-a", KeychainAccount, "-s", "svc-1", "-X", helloHex))
				if code := out.ExitCode(t); code != 0 {
					t.Fatalf("the write exited %d; stderr:\n%s", code, out.Stderr)
				}
				if out.Stderr != "" {
					t.Fatalf("the write printed diagnostics, so a count was not plain arithmetic:\n%s", out.Stderr)
				}
				if diff := gocmp.Diff(tt.wantSleep, traceLines(t, trace)); diff != "" {
					t.Fatalf("sleeps the fake asked for (-want +got):\n%s", diff)
				}
				beats, ok := readCount(pidfile + ".heartbeat")
				if tt.wantBeats == 0 && ok {
					t.Fatalf("the heartbeat holds %d, want no heartbeat file", beats)
				}
				if tt.wantBeats != 0 && (!ok || beats != tt.wantBeats) {
					t.Fatalf("the heartbeat holds %d (readable %t), want %d", beats, ok, tt.wantBeats)
				}
				if !itemHolds(f, "svc-1", "hello") {
					t.Fatalf("the write did not store the decoded payload")
				}
			})
		}
	}
}

func TestFakeSecurityConfinesEveryPathKnobToTheFixtureRoot(t *testing.T) {
	tests := map[string]struct {
		// knob is the variable to point elsewhere, or "" for none.
		knob string
		// escape returns its value, given the fixture root and a directory
		// outside it.
		escape func(root, outside string) string
		// link, when set, is a symbolic link the case plants: its name
		// under the root and its target, given the root and the outside
		// directory.
		link func(root, outside string) (name, target string)
		// root overrides the exported fixture root when set.
		root     *string
		wantExit int
	}{
		"success: every path knob under the root": {wantExit: 0},
		"error: the log outside the root": {
			knob: "AGCTL_FAKE_SECURITY_LOG", escape: func(_, outside string) string { return filepath.Join(outside, "security.log") }, wantExit: 1,
		},
		"error: the items outside the root": {
			knob: "AGCTL_FAKE_SECURITY_ITEMS", escape: func(_, outside string) string { return filepath.Join(outside, "items") }, wantExit: 1,
		},
		"error: the dump outside the root": {
			knob: "AGCTL_FAKE_SECURITY_DUMP", escape: func(_, outside string) string { return filepath.Join(outside, "dump") }, wantExit: 1,
		},
		"error: the dump marker outside the root": {
			knob: "AGCTL_FAKE_SECURITY_DUMP_EXIT_IF", escape: func(_, outside string) string { return filepath.Join(outside, "marker") }, wantExit: 1,
		},
		"error: the find marker outside the root": {
			knob: "AGCTL_FAKE_SECURITY_FIND_EXIT_IF", escape: func(_, outside string) string { return filepath.Join(outside, "marker") }, wantExit: 1,
		},
		"error: the pidfile outside the root": {
			knob: "AGCTL_FAKE_SECURITY_PIDFILE", escape: func(_, outside string) string { return filepath.Join(outside, "security.pid") }, wantExit: 1,
		},
		"error: the log climbs out of the root through a dot-dot segment": {
			knob: "AGCTL_FAKE_SECURITY_LOG", escape: func(root, outside string) string {
				return root + "/../" + filepath.Base(filepath.Dir(outside)) + "/" + filepath.Base(outside) + "/security.log"
			}, wantExit: 1,
		},
		"error: the log is the root's sibling sharing its prefix": {
			knob: "AGCTL_FAKE_SECURITY_LOG", escape: func(root, _ string) string { return root + "-sibling/security.log" }, wantExit: 1,
		},
		"error: the log under a directory link that leads out of the root": {
			knob: "AGCTL_FAKE_SECURITY_LOG", escape: func(root, _ string) string { return filepath.Join(root, "linked", "escaped.log") },
			link:     func(root, outside string) (string, string) { return filepath.Join(root, "linked"), outside },
			wantExit: 1,
		},
		"error: the items under a directory link that leads out of the root": {
			knob: "AGCTL_FAKE_SECURITY_ITEMS", escape: func(root, _ string) string { return filepath.Join(root, "linked", "items") },
			link:     func(root, outside string) (string, string) { return filepath.Join(root, "linked"), outside },
			wantExit: 1,
		},
		"error: the log is itself a link to a file outside the root": {
			knob: "AGCTL_FAKE_SECURITY_LOG", escape: func(root, _ string) string { return filepath.Join(root, "security-link.log") },
			link: func(root, outside string) (string, string) {
				return filepath.Join(root, "security-link.log"), filepath.Join(outside, "security.log")
			},
			wantExit: 1,
		},
		"error: the pidfile under a dangling link": {
			knob: "AGCTL_FAKE_SECURITY_PIDFILE", escape: func(root, _ string) string { return filepath.Join(root, "dangling", "run", "security.pid") },
			link: func(root, outside string) (string, string) {
				return filepath.Join(root, "dangling"), filepath.Join(outside, "missing")
			},
			wantExit: 1,
		},
		"success: a link that stays inside the root": {
			knob: "AGCTL_FAKE_SECURITY_LOG", escape: func(root, _ string) string { return filepath.Join(root, "inner-link", "security.log") },
			link:     func(root, _ string) (string, string) { return filepath.Join(root, "inner-link"), root },
			wantExit: 0,
		},
		"error: a path knob while no root is exported": {root: new(""), wantExit: 1},
		"error: a relative root":                       {root: new("relative/root"), wantExit: 1},
	}

	for _, shell := range installedShells(t) {
		for name, tt := range tests {
			t.Run(filepath.Base(shell)+"/"+name, func(t *testing.T) {
				f := New(t).WithKeychain()
				f.AllowWrite("svc-1")
				outside := t.TempDir()
				if tt.link != nil {
					name, target := tt.link(f.Root(), outside)
					if err := os.Symlink(target, name); err != nil {
						t.Fatal(err)
					}
				}
				if tt.knob != "" {
					f.Set(tt.knob, tt.escape(f.Root(), outside))
				}
				if tt.root != nil {
					f.Set("AGCTL_FAKE_SECURITY_ROOT", *tt.root)
				}

				out := Run(t, securityUnder(t, f, shell, "add-generic-password", "-U", "-a", KeychainAccount, "-s", "svc-1", "-X", helloHex))
				if code := out.ExitCode(t); code != tt.wantExit {
					t.Fatalf("the write exited %d, want %d; stderr:\n%s", code, tt.wantExit, out.Stderr)
				}
				entries, err := os.ReadDir(outside)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("the fake created %d entries outside the fixture root", len(entries))
				}
				if _, err := os.Stat(f.Root() + "-sibling"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the fake created the root's prefix sibling: %v", err)
				}
				if tt.wantExit == 0 {
					if !itemHolds(f, "svc-1", "hello") {
						t.Fatalf("a confined write did not store its item")
					}
					logPath := f.SecurityLogPath()
					if tt.knob == "AGCTL_FAKE_SECURITY_LOG" {
						logPath = tt.escape(f.Root(), outside)
					}
					if data, err := os.ReadFile(logPath); err != nil || strings.Count(string(data), "\n") != 1 {
						t.Fatalf("a confined write did not log one line to %s: %q, %v", logPath, data, err)
					}
					return
				}
				if log := f.SecurityLog(); len(log) != 0 {
					t.Fatalf("a refused knob still logged %d lines", len(log))
				}
				if items := f.KeychainItems(); len(items) != 0 {
					t.Fatalf("a refused knob still stored %d items", len(items))
				}
				if !strings.Contains(out.Stderr, "AGCTL_FAKE_SECURITY_") {
					t.Fatalf("the refusal does not name the knob; stderr:\n%s", out.Stderr)
				}
			})
		}
	}
}

func TestFakeSecurityFailsReadsOnceTheMarkerExists(t *testing.T) {
	tests := map[string]struct {
		code     string
		marked   bool
		wantExit int
	}{
		"success: reads pass while the marker is absent":   {wantExit: 0},
		"error: a read fails with 36 once it exists":       {marked: true, wantExit: 36},
		"error: a read fails with the chosen code instead": {marked: true, code: "1", wantExit: 1},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := New(t).WithKeychain()
			f.KeychainItem("svc-1", "the item bytes")
			marker := filepath.Join(f.Root(), "read-fails")
			f.Set("AGCTL_FAKE_SECURITY_FIND_EXIT_IF", marker)
			if tt.code != "" {
				f.Set("AGCTL_FAKE_SECURITY_FIND_EXIT_IF_CODE", tt.code)
			}
			if tt.marked {
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			cmd := f.Apply(exec.CommandContext(t.Context(), f.SecurityBin(), "find-generic-password", "-s", "svc-1", "-a", KeychainAccount, "-w"))
			out := Run(t, cmd)
			if code := out.ExitCode(t); code != tt.wantExit {
				t.Fatalf("the read exited %d, want %d", code, tt.wantExit)
			}
			if tt.wantExit == 0 && out.Stdout != "the item bytes" {
				t.Fatalf("the read served %d bytes, want the planted item", len(out.Stdout))
			}
			if tt.wantExit != 0 && out.Stdout != "" {
				t.Fatalf("a failed read still served %d bytes", len(out.Stdout))
			}
		})
	}
}
