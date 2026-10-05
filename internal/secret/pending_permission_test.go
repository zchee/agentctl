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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func testPendingWithoutPrivileges(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPendingAnUnopenableFileKeepsEverythingUnderTheStrictRuleOnly$")
	cmd.Env = append(os.Environ(), "AGENTCTL_TEST_PENDING_UNPRIVILEGED=1", "GORACE=atexit_sleep_ms=0")
	if os.Geteuid() == 0 {
		// A root-owned build directory may not be traversable after setuid.
		// Use the public temporary parent, not root's private TMPDIR.
		root, err := os.MkdirTemp("/tmp", "agentctl-pending-permissions-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(root); err != nil {
				t.Error(err)
			}
		})
		binary := filepath.Join(root, "pending.test")
		source, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = source.Close() }()
		destination, err := os.OpenFile(binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(destination, source)
		closeErr := destination.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("copy child executable: %v, %v", copyErr, closeErr)
		}
		if err := os.Chmod(binary, 0o755); err != nil {
			t.Fatal(err)
		}
		const unprivilegedID = 65534
		if err := os.Chown(root, unprivilegedID, unprivilegedID); err != nil {
			t.Fatal(err)
		}
		cmd.Path = binary
		cmd.Args[0] = binary
		cmd.Dir = root
		cmd.Env = append(cmd.Env, "TMPDIR="+root)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: unprivilegedID, Gid: unprivilegedID}}
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged permission assertions failed: %v\n%s", err, output)
	}
}
