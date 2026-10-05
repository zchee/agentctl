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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"
)

// ScriptSetup prepares one script's isolated world: an owned
// config/home/bin tree beside the extracted script files, the fake
// executables installed with their knobs wired, every endpoint override
// closed, and the identity pinned. The binary under test is already on
// PATH, registered by the same TestMain that passes this to the runner.
func ScriptSetup(env *testscript.Env) error {
	root := filepath.Join(env.WorkDir, ".harness")
	for _, relative := range []string{"config", "home", "bin", "keychain-items"} {
		if err := os.MkdirAll(filepath.Join(root, relative), 0o755); err != nil {
			return err
		}
	}

	securityBin := filepath.Join(root, "bin", "security")
	if err := installScriptFile("fake-security.sh", securityBin); err != nil {
		return err
	}
	codexBin := filepath.Join(root, "bin", "fake-codex.sh")
	if err := installScriptFile("fake-codex.sh", codexBin); err != nil {
		return err
	}
	dumpPath := filepath.Join(root, "keychain-dump.txt")
	if err := os.WriteFile(dumpPath, []byte(dumpListing()), 0o644); err != nil {
		return err
	}

	env.Setenv("AGENTCTL_CONFIG_DIR", filepath.Join(root, "config"))
	env.Setenv("HOME", filepath.Join(root, "home"))
	env.Setenv("USER", KeychainAccount)
	env.Setenv("LOGNAME", KeychainAccount)
	env.Setenv("AGENTCTL_CLAUDE_USAGE_URL", ClosedEndpoint)
	env.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", ClosedEndpoint+"/token")
	env.Setenv("AGENTCTL_CLAUDE_AUTHORIZE_URL", ClosedEndpoint+"/authorize")
	env.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", ClosedEndpoint+ProfilePath)
	env.Setenv("AGENTCTL_CODEX_TOKEN_URL", ClosedEndpoint+"/oauth/token")
	env.Setenv("AGENTCTL_CODEX_USAGE_URL", ClosedEndpoint)
	env.Setenv("AGENTCTL_NO_BROWSER", "1")
	env.Setenv("AGENTCTL_SECURITY_BIN", securityBin)
	env.Setenv("AGENTCTL_CODEX_BIN", codexBin)
	env.Setenv("AGCTL_FAKE_SECURITY_LOG", filepath.Join(root, "security-argv.log"))
	env.Setenv("AGCTL_FAKE_SECURITY_ITEMS", filepath.Join(root, "keychain-items"))
	env.Setenv("AGCTL_FAKE_SECURITY_DUMP", dumpPath)
	return nil
}

// say writes formatted text to a helper's stream. A helper program has
// nowhere to report a failed write about a failed write, so the error is
// consciously dropped here, once, instead of at every call site.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// ScriptCommands returns the helper programs the e2e harness registers
// beside the binary under test. Each runs inside the re-executed test
// binary, whose working directory is the script's own, so every path
// argument is script-relative or absolute.
func ScriptCommands() map[string]func() {
	wrap := func(run func(args []string, stdout, stderr io.Writer) int) func() {
		return func() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
	}
	return map[string]func(){
		"flockhold":  wrap(flockholdMain),
		"sigterm":    wrap(sigtermMain),
		"drainpipes": wrap(drainpipesMain),
		"mtime":      wrap(mtimeMain),
		"waitfor":    wrap(waitforMain),
		"schema":     wrap(schemaMain),
		"golden":     wrap(goldenMain),
	}
}

// flockholdMain takes the exclusive lock on args[0] and holds it: for
// args[1] (a duration) when given, forever otherwise. It prints "held"
// once the lock is taken, so a script can wait for the line instead of
// sleeping, and refuses a lock somebody already holds.
func flockholdMain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || len(args) > 2 {
		say(stderr, "usage: flockhold <path> [duration]\n")
		return 2
	}
	hold := time.Duration(-1)
	if len(args) == 2 {
		parsed, err := time.ParseDuration(args[1])
		if err != nil {
			say(stderr, "flockhold: %v\n", err)
			return 2
		}
		hold = parsed
	}
	if err := os.MkdirAll(filepath.Dir(args[0]), 0o755); err != nil {
		say(stderr, "flockhold: %v\n", err)
		return 1
	}
	file, err := os.OpenFile(args[0], os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		say(stderr, "flockhold: %v\n", err)
		return 1
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		say(stderr, "flockhold: %s is already held: %v\n", args[0], err)
		return 1
	}
	say(stdout, "held\n")
	if hold < 0 {
		select {}
	}
	time.Sleep(hold)
	return 0
}

// sigtermMain sends SIGTERM to the process id in args[0].
func sigtermMain(args []string, _, stderr io.Writer) int {
	if len(args) != 1 {
		say(stderr, "usage: sigterm <pid>\n")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		say(stderr, "sigterm: %v\n", err)
		return 2
	}
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		say(stderr, "sigterm: %v\n", err)
		return 1
	}
	return 0
}

// drainpipesMain runs the command in args with both of its pipes drained
// concurrently and reports how much each stream carried, so a script can
// assert a chatty child stayed live past the kernel's pipe buffer.
func drainpipesMain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		say(stderr, "usage: drainpipes <command> [args...]\n")
		return 2
	}
	cmd := exec.Command(args[0], args[1:]...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	say(stdout, "stdout:%d stderr:%d\n", outBuf.Len(), errBuf.Len())
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode()
	}
	say(stderr, "drainpipes: %v\n", err)
	return 1
}

// mtimeMain prints args[0]'s modification time in nanoseconds since the
// epoch, so a script can record it and prove it later unchanged.
func mtimeMain(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		say(stderr, "usage: mtime <path>\n")
		return 2
	}
	info, err := os.Stat(args[0])
	if err != nil {
		say(stderr, "mtime: %v\n", err)
		return 1
	}
	say(stdout, "%d\n", info.ModTime().UnixNano())
	return 0
}

// waitforMain polls until args[0] exists - or, with -gone, until it does
// not - within a bounded budget (default 10s), the e2e counterpart of
// the bounded polling the in-process harness uses.
func waitforMain(args []string, _, stderr io.Writer) int {
	gone := false
	if len(args) > 0 && args[0] == "-gone" {
		gone = true
		args = args[1:]
	}
	if len(args) < 1 || len(args) > 2 {
		say(stderr, "usage: waitfor [-gone] <path> [budget]\n")
		return 2
	}
	budget := 10 * time.Second
	if len(args) == 2 {
		parsed, err := time.ParseDuration(args[1])
		if err != nil {
			say(stderr, "waitfor: %v\n", err)
			return 2
		}
		budget = parsed
	}
	observed := WaitUntil(budget, func() bool {
		_, err := os.Lstat(args[0])
		if gone {
			return err != nil
		}
		return err == nil
	})
	if !observed {
		state := "appear"
		if gone {
			state = "disappear"
		}
		say(stderr, "waitfor: %s did not %s within %s\n", args[0], state, budget)
		return 1
	}
	return 0
}

// schemaMain validates the document in args[1] against the embedded
// schema named by args[0].
func schemaMain(args []string, _, stderr io.Writer) int {
	if len(args) != 2 {
		say(stderr, "usage: schema <schema-name> <document>\n")
		return 2
	}
	document, err := os.ReadFile(args[1])
	if err != nil {
		say(stderr, "schema: %v\n", err)
		return 1
	}
	if err := SchemaError(args[0], document); err != nil {
		say(stderr, "schema: %v\n", err)
		return 1
	}
	return 0
}

// goldenMain compares the file in args[1] against the named golden
// oracle, exactly by default and under the end-of-file trim with -trim.
func goldenMain(args []string, _, stderr io.Writer) int {
	trim := false
	if len(args) > 0 && args[0] == "-trim" {
		trim = true
		args = args[1:]
	}
	if len(args) != 2 {
		say(stderr, "usage: golden [-trim] <name> <file>\n")
		return 2
	}
	root, err := repoRoot()
	if err != nil {
		say(stderr, "golden: %v\n", err)
		return 1
	}
	want, err := os.ReadFile(filepath.Join(root, "testdata", "golden", args[0]+".golden"))
	if err != nil {
		say(stderr, "golden: %v\n", err)
		return 1
	}
	got, err := os.ReadFile(args[1])
	if err != nil {
		say(stderr, "golden: %v\n", err)
		return 1
	}
	wantCmp, gotCmp := want, got
	if trim {
		wantCmp, gotCmp = normalizeEOF(want), normalizeEOF(got)
	}
	if !bytes.Equal(wantCmp, gotCmp) {
		say(stderr, "golden: %s does not match %s.golden\n-- want --\n%s\n-- got --\n%s\n", args[1], args[0], wantCmp, gotCmp)
		return 1
	}
	return 0
}
