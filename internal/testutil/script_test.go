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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runHelper runs one helper program function with captured streams.
func runHelper(run func(args []string, stdout, stderr io.Writer) int, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestScriptCommandsRegistersEveryHelper(t *testing.T) {
	t.Parallel()

	want := []string{"drainpipes", "flockhold", "golden", "mtime", "schema", "sigterm", "waitfor"}
	commands := ScriptCommands()
	got := make([]string, 0, len(commands))
	for name := range commands {
		got = append(got, name)
	}
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Fatalf("ScriptCommands() = %v, want %v", got, want)
	}
}

func TestFlockholdMain(t *testing.T) {
	t.Parallel()

	t.Run("success: a bounded hold takes and releases the lock", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "locks", "ns.lock")
		code, stdout, stderr := runHelper(flockholdMain, path, "50ms")
		if code != 0 {
			t.Fatalf("flockhold exited %d; stderr:\n%s", code, stderr)
		}
		if stdout != "held\n" {
			t.Fatalf("flockhold printed %q, want the readiness line", stdout)
		}
		// Observed within a bound for the same reason as the lock tests: a
		// concurrent fork+exec in this binary can briefly inherit the lock's
		// descriptor, delaying when the release becomes visible.
		if !WaitUntil(5*time.Second, func() bool { return !LockIsHeld(path) }) {
			t.Fatalf("the lock is still held after the bounded hold returned")
		}
	})

	t.Run("error: a lock somebody holds is refused", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "ns.lock")
		holder := HoldLock(t, path)
		defer func() { _ = holder.Close() }()

		code, _, stderr := runHelper(flockholdMain, path, "50ms")
		if code != 1 || !strings.Contains(stderr, "already held") {
			t.Fatalf("flockhold on a held lock = %d %q, want a refusal", code, stderr)
		}
	})

	t.Run("error: a malformed duration is a usage error", func(t *testing.T) {
		t.Parallel()

		code, _, _ := runHelper(flockholdMain, filepath.Join(t.TempDir(), "l"), "soon")
		if code != 2 {
			t.Fatalf("flockhold with a bad duration = %d, want 2", code)
		}
	})
}

func TestSigtermMain(t *testing.T) {
	t.Parallel()

	t.Run("success: the named process receives the signal", func(t *testing.T) {
		t.Parallel()

		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 30")
		stdout, stderr := Capture(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start the child: %v", err)
		}
		code, _, helperStderr := runHelper(sigtermMain, strconv.Itoa(cmd.Process.Pid))
		if code != 0 {
			t.Fatalf("sigterm exited %d; stderr:\n%s", code, helperStderr)
		}
		if out := Finish(t, cmd, stdout, stderr); !out.Signaled {
			t.Fatalf("the child was not ended by the signal: exit %d", out.Code)
		}
	})

	t.Run("error: a non-numeric pid is a usage error", func(t *testing.T) {
		t.Parallel()

		if code, _, _ := runHelper(sigtermMain, "nope"); code != 2 {
			t.Fatalf("sigterm nope did not report usage")
		}
	})
}

func TestDrainpipesMain(t *testing.T) {
	t.Parallel()

	script := `i=0; while [ $i -lt 2000 ]; do printf '%0100d\n' "$i" >&2; i=$((i+1)); done; echo done; exit 7`
	code, stdout, _ := runHelper(drainpipesMain, "/bin/sh", "-c", script)
	if code != 7 {
		t.Fatalf("drainpipes propagated exit %d, want the child's 7", code)
	}
	if !strings.HasPrefix(stdout, "stdout:5 stderr:202000") {
		t.Fatalf("drainpipes reported %q, want the drained byte counts", stdout)
	}
}

func TestMtimeMain(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	code, stdout, stderr := runHelper(mtimeMain, path)
	if code != 0 {
		t.Fatalf("mtime exited %d; stderr:\n%s", code, stderr)
	}
	printed, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("mtime printed %q, want nanoseconds: %v", stdout, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if printed != info.ModTime().UnixNano() {
		t.Fatalf("mtime printed %d, stat says %d", printed, info.ModTime().UnixNano())
	}

	if code, _, _ := runHelper(mtimeMain, filepath.Join(t.TempDir(), "absent")); code != 1 {
		t.Fatalf("mtime on an absent file did not fail")
	}
}

func TestWaitforMain(t *testing.T) {
	t.Parallel()

	t.Run("success: an appearing file is observed", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "flag")
		go func() {
			time.Sleep(60 * time.Millisecond)
			_ = os.WriteFile(path, nil, 0o600)
		}()
		if code, _, stderr := runHelper(waitforMain, path, "2s"); code != 0 {
			t.Fatalf("waitfor exited %d; stderr:\n%s", code, stderr)
		}
	})

	t.Run("success: a removed file is observed gone", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "flag")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		go func() {
			time.Sleep(60 * time.Millisecond)
			_ = os.Remove(path)
		}()
		if code, _, stderr := runHelper(waitforMain, "-gone", path, "2s"); code != 0 {
			t.Fatalf("waitfor -gone exited %d; stderr:\n%s", code, stderr)
		}
	})

	t.Run("error: a file that never appears times out", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "never")
		if code, _, stderr := runHelper(waitforMain, path, "80ms"); code != 1 || !strings.Contains(stderr, "did not appear") {
			t.Fatalf("waitfor on a missing file = %d %q, want a bounded timeout", code, stderr)
		}
	})
}

func TestSchemaMain(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	conforming := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(conforming, []byte(`{"version": 1, "generated_at": "2026-09-08T00:00:00Z", "rows": [], "hidden": 0}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	violating := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(violating, []byte(`{"version": 999}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if code, _, stderr := runHelper(schemaMain, "status.v1.json", conforming); code != 0 {
		t.Fatalf("schema on a conforming document = %d; stderr:\n%s", code, stderr)
	}
	if code, _, _ := runHelper(schemaMain, "status.v1.json", violating); code != 1 {
		t.Fatalf("schema on a violating document = %d, want 1", code)
	}
}

func TestGoldenMain(t *testing.T) {
	t.Parallel()

	const name = "render__table__tests__all_rows"
	dir := t.TempDir()

	exact := filepath.Join(dir, "exact.txt")
	if err := os.WriteFile(exact, ReadGolden(t, name), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	padded := filepath.Join(dir, "padded.txt")
	if err := os.WriteFile(padded, append(ReadGolden(t, name), []byte("  \n")...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if code, _, stderr := runHelper(goldenMain, name, exact); code != 0 {
		t.Fatalf("golden on identical bytes = %d; stderr:\n%s", code, stderr)
	}
	if code, _, _ := runHelper(goldenMain, name, padded); code != 1 {
		t.Fatalf("golden on padded bytes = %d, want the exact mismatch", code)
	}
	if code, _, stderr := runHelper(goldenMain, "-trim", name, padded); code != 0 {
		t.Fatalf("golden -trim on padded bytes = %d; stderr:\n%s", code, stderr)
	}
	if code, _, _ := runHelper(goldenMain, "no-such-golden", exact); code != 1 {
		t.Fatalf("golden with an unknown oracle did not fail")
	}
}
