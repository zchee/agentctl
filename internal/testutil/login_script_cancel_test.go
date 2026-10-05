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

package testutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/app"
	"github.com/zchee/agentctl/internal/cli"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"agentctl": func() {
			if err := os.WriteFile("login.pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			command := cli.New(app.Handlers(app.Dependencies{Stdout: os.Stdout}))
			if err := command.Root().Execute(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		},
	})
}

func TestLoginProcessCancellationReapsChild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cancel.txtar"), []byte("cancel-login\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(blocked)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	testscript.Run(t, testscript.Params{
		Dir: dir,
		Setup: func(env *testscript.Env) error {
			if err := ScriptSetup(env); err != nil {
				return err
			}
			env.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+"/token")
			return nil
		},
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"cancel-login": func(ts *testscript.TestScript, _ bool, _ []string) {
				ctx, cancel := context.WithCancel(ts.Value(scriptContextKey{}).(context.Context))
				defer cancel()
				finished := make(chan struct{})
				cancelled := make(chan time.Time, 1)
				watchdog := make(chan error, 1)
				pidPath := ts.MkAbs("login.pid")
				go func() {
					select {
					case <-blocked:
					case <-finished:
						return
					}
					cancelled <- time.Now()
					cancel()
					timer := time.NewTimer(5 * time.Second)
					defer timer.Stop()
					select {
					case <-finished:
						watchdog <- nil
					case <-timer.C:
						// Force cleanup on regression without waiting for the login budget.
						data, err := os.ReadFile(pidPath)
						if err == nil {
							var pid int
							pid, err = strconv.Atoi(string(data))
							if err == nil {
								err = unix.Kill(pid, unix.SIGKILL)
							}
						}
						watchdog <- fmt.Errorf("login child survived cancellation for 5s; forced cleanup: %v", err)
					}
				}()
				func() {
					defer close(finished)
					loginProcess(ctx, ts, "manual", -1, nil, false)
				}()
				elapsed := time.Since(<-cancelled)
				if err := <-watchdog; err != nil || elapsed >= 5*time.Second {
					ts.Fatalf("login child was not terminated and reaped promptly: %s; %v", elapsed, err)
				}
				data, err := os.ReadFile(pidPath)
				ts.Check(err)
				pid, err := strconv.Atoi(string(data))
				ts.Check(err)
				var status unix.WaitStatus
				if _, err := unix.Wait4(pid, &status, unix.WNOHANG, nil); !errors.Is(err, unix.ECHILD) {
					ts.Fatalf("login child %d was not reaped: %v", pid, err)
				}
				t.Logf("login child %d terminated and reaped in %s after cancellation", pid, elapsed)
			},
		},
	})
}
