//go:build agentctl_testing

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

package codex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func armLoginChild(t *testing.T) (LoginChild, *LoginScratch, string, string) {
	t.Helper()
	source, err := filepath.Abs("../../../fixtures/fake-codex.sh")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixture := filepath.Join(root, "fake-codex.sh")
	if err := os.WriteFile(fixture, data, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "codex.log")
	doc := filepath.Join(root, "login.json")
	if err := os.WriteFile(doc, []byte("fixture login credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_CODEX_BIN", fixture)
	t.Setenv("AGENTCTL_FAKE_CODEX_LOG", log)
	t.Setenv("AGENTCTL_FAKE_CODEX_AUTH", doc)
	t.Setenv("AGENTCTL_FAKE_CODEX_SLEEP", "")
	t.Setenv("AGENTCTL_FAKE_CODEX_EXIT", "0")
	t.Setenv("AGENTCTL_FAKE_CODEX_HELD_LOCK", "")
	t.Setenv("AGENTCTL_FAKE_CODEX_DAEMON_DIR", "")
	t.Setenv("CODEX_HOME", filepath.Join(root, "live"))
	bin, err := ResolveLoginBinary()
	if err != nil {
		t.Fatal(err)
	}
	if bin != fixture {
		t.Fatal("the fixture binary was not selected")
	}
	return LoginChild{In: strings.NewReader(""), Out: new(bytes.Buffer), Err: new(bytes.Buffer)}, newTestScratch(t), bin, log
}

func TestLoginChildFixture(t *testing.T) {
	tests := map[string]struct {
		control string
		value   string
		exit    int
		daemon  bool
		held    bool
	}{
		"success: measured residue":    {},
		"success: child exit retained": {control: "AGENTCTL_FAKE_CODEX_EXIT", value: "3", exit: 3},
		"error: daemon evidence":       {control: "AGENTCTL_FAKE_CODEX_DAEMON_DIR", value: "1", daemon: true},
		"error: actual surviving lock": {control: "AGENTCTL_FAKE_CODEX_HELD_LOCK", value: "1", held: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			child, scratch, bin, log := armLoginChild(t)
			if tt.control != "" {
				t.Setenv(tt.control, tt.value)
			}
			t.Setenv("AWS_SECRET_ACCESS_KEY", "drop-this-decoy")
			t.Setenv("CODEX_API_KEY", "drop-this-decoy")
			t.Setenv("Https_Proxy", "drop-this-decoy")
			t.Setenv("https_proxy", "http://proxy.invalid:3128")
			t.Setenv("LC_CTYPE", "C")
			t.Setenv("AGENTCTL_FAKE_CODEX_MULTILINE", "first\nenv DECOY_FROM_A_VALUE")
			state, err := child.Run(t.Context(), scratch, bin)
			if err != nil {
				t.Fatal(err)
			}
			if state.ExitCode() != tt.exit {
				t.Fatalf("child exit = %d, want %d", state.ExitCode(), tt.exit)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			var args []string
			for line := range strings.SplitSeq(text, "\n") {
				if arg, ok := strings.CutPrefix(line, "arg "); ok {
					args = append(args, arg)
				}
			}
			if diff := gocmp.Diff([]string{"-c", "cli_auth_credentials_store=\"file\"", "login"}, args); diff != "" {
				t.Fatalf("child arguments (-want +got):\n%s", diff)
			}
			resolved, err := filepath.EvalSymlinks(scratch.Path())
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"cwd " + resolved + "\n", "value CODEX_HOME=" + scratch.Path() + "\n", "env https_proxy\n", "env LC_CTYPE\n", "env AGCTL_FAKE_CODEX_MULTILINE\n", "residue tmp/arg0/codex-arg0"} {
				if !strings.Contains(text, want) {
					t.Fatalf("fixture did not record %q", want)
				}
			}
			for _, absent := range []string{"AWS_SECRET_ACCESS_KEY", "CODEX_API_KEY", "Https_Proxy", "DECOY_FROM_A_VALUE", "drop-this-decoy"} {
				if strings.Contains(text, absent) {
					t.Fatalf("fixture recorded prohibited environment shape %q", absent)
				}
			}
			survey := SurveyLoginScratch(t.Context(), scratch.Path())
			if survey.DaemonDir != tt.daemon || (len(survey.HeldLocks) > 0) != tt.held || survey.Truncated || len(survey.OddLocks) > 0 {
				t.Fatalf("unexpected fixture survey: %+v", survey)
			}
			if got, err := os.ReadFile(filepath.Join(scratch.Path(), "auth.json")); err != nil || string(got) != "fixture login credential\n" {
				t.Fatal("fixture credential was not written")
			}
			scratch.UnlinkCredential()
			if _, err := os.Lstat(filepath.Join(scratch.Path(), "auth.json")); !os.IsNotExist(err) {
				t.Fatalf("credential remains after explicit unlink: %v", err)
			}
			scratch.Discard(nil)
			if _, err := os.Lstat(scratch.Path()); !os.IsNotExist(err) {
				t.Fatalf("scratch remains after discard: %v", err)
			}
		})
	}
}

func TestLoginChildCancellationAndSpawnFailure(t *testing.T) {
	tests := map[string]struct{ started bool }{
		"error: missing executable":               {},
		"error: cancellation reaps started child": {started: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			child, scratch, bin, log := armLoginChild(t)
			if !tt.started {
				if _, err := child.Run(t.Context(), scratch, bin+"-missing"); err == nil {
					t.Fatal("missing executable succeeded")
				}
				return
			}
			t.Setenv("AGENTCTL_FAKE_CODEX_SLEEP", "30")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := child.Run(ctx, scratch, bin); done <- err }()
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			ticker := time.NewTicker(2 * time.Millisecond)
			defer ticker.Stop()
			started := false
			for !started {
				select {
				case err := <-done:
					t.Fatalf("child ended before cancellation: %v", err)
				case <-timer.C:
					t.Fatal("fixture did not start")
				case <-ticker.C:
					_, err := os.Stat(log)
					started = err == nil
				}
			}
			cancel()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "cancelled") {
					t.Fatalf("cancellation result = %v", err)
				}
			case <-timer.C:
				t.Fatal("cancelled child was not reaped")
			}
		})
	}
}
