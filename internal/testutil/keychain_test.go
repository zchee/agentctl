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
	"os"
	"os/exec"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

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
