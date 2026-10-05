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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/signals"
)

func TestSecretFileSignalRemovesOnlyDiscardableTemporary(t *testing.T) {
	tests := map[string]struct {
		mode             string
		replaceDirectory bool
		kept             bool
	}{
		"success: staged bytes removed on TERM":             {mode: "discard"},
		"success: cleanup stays bound to renamed directory": {mode: "discard", replaceDirectory: true},
		"success: rotated grant survives TERM":              {mode: "complete", kept: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ns := filepath.Join(root, "namespace")
			if err := os.Mkdir(ns, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ns, testAuth), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSecretFileSignalHelper$")
			cmd.Env = append(os.Environ(), "AGENTCTL_TEST_SECRET_SIGNAL="+tt.mode, "AGENTCTL_TEST_SECRET_ROOT="+root, "GORACE=atexit_sleep_ms=0")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			staged, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil {
				t.Fatalf("child did not stage a temporary: %v; %s", err, &stderr)
			}
			staged = strings.TrimSpace(staged)
			if !strings.HasPrefix(staged, testAuth+".tmp.") || filepath.Base(staged) != staged {
				t.Fatalf("unexpected readiness line: %q", staged)
			}
			retained := ns
			var decoy string
			if tt.replaceDirectory {
				retained = filepath.Join(root, "retained")
				if err := os.Rename(ns, retained); err != nil {
					t.Fatal(err)
				}
				decoy = filepath.Join(root, "decoy")
				if err := os.Mkdir(decoy, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(decoy, staged), []byte("unrelated"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(decoy, ns); err != nil {
					t.Fatal(err)
				}
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			exit, ok := errors.AsType[*exec.ExitError](err)
			if !ok || exit.ExitCode() != 128+int(syscall.SIGTERM) {
				t.Fatalf("child exit = %v; stderr=%s", err, &stderr)
			}
			if got := readText(t, filepath.Join(retained, testAuth)); got != "old" {
				t.Fatalf("old file changed: %q", got)
			}
			_, err = os.Stat(filepath.Join(retained, staged))
			if tt.kept {
				if err != nil || readText(t, filepath.Join(retained, staged)) != "new grant" {
					t.Fatalf("rotated grant was not retained: %v", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("discardable temporary remains: %v", err)
			}
			if decoy != "" && readText(t, filepath.Join(decoy, staged)) != "unrelated" {
				t.Fatal("cleanup touched replacement directory")
			}
		})
	}
}

func TestSecretFileSignalHelper(t *testing.T) {
	mode := os.Getenv("AGENTCTL_TEST_SECRET_SIGNAL")
	if mode == "" {
		return
	}
	root := os.Getenv("AGENTCTL_TEST_SECRET_ROOT")
	ns := filepath.Join(root, "namespace")
	fd, err := unix.Open(ns, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := NewSecretFile(root, fd, testAuth, filepath.Join(ns, testAuth))
	ctx, controller := signals.Install(t.Context())
	defer controller.Stop()
	file.faults = &writeFaults{beforeRename: func() {
		staged := strayTmps(t, ns, testAuth)
		if len(staged) != 1 {
			t.Fatalf("staged = %v", staged)
		}
		// Cleanup owns a duplicate descriptor, not this caller's lifetime.
		if err := unix.Close(fd); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, staged[0]); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		controller.Wait()
		os.Exit(128 + int(syscall.SIGTERM))
	}}
	stop := StopDiscardStaged
	if mode == "complete" {
		stop = StopComplete
	}
	_, _ = file.Write(ctx, []byte("new grant"), nil, stop)
	t.Fatal("write unexpectedly returned")
}

func TestSecretFileWithdrawsTemporaryCleanupOnReturn(t *testing.T) {
	tests := map[string]struct {
		cancel bool
		fail   bool
	}{
		"success: committed write withdraws cleanup": {},
		"success: cancelled write withdraws cleanup": {cancel: true},
		"success: failed rename withdraws cleanup":   {fail: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			file := store.codexFile()
			var temporary string
			file.faults = &writeFaults{beforeRename: func() { temporary = strayTmps(t, store.codexNS, testAuth)[0] }}
			if tt.fail {
				file.faults.renameErr = errors.New("rename refused")
			}
			ctx := t.Context()
			if tt.cancel {
				ctx = cancelledContext(t)
			}
			_, err := file.Write(ctx, []byte("new"), nil, StopDiscardStaged)
			if (err != nil) != (tt.cancel || tt.fail) {
				t.Fatalf("write error = %v", err)
			}
			path := filepath.Join(store.codexNS, temporary)
			if err := os.WriteFile(path, []byte("next owner's file"), 0o600); err != nil {
				t.Fatal(err)
			}
			cleanup.Run()
			if got := readText(t, path); got != "next owner's file" {
				t.Fatal("withdrawn cleanup changed a later file")
			}
		})
	}
}
