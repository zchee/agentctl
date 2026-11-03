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
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/runtime/tty"
	"github.com/zchee/agentctl/internal/tui"
)

func TestWatchRunStartupRefusals(t *testing.T) {
	tests := map[string]struct {
		interval time.Duration
		factory  bool
		want     string
	}{
		"error: interval below polling floor": {interval: cli.WatchFloor - time.Second, want: "below the"},
		"error: absent status factory":        {interval: cli.WatchFloor, want: "status factory is not configured"},
		"error: redirected terminal":          {interval: cli.WatchFloor, factory: true, want: "requires a terminal"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "redirected")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			watch := Watch{Input: file, Output: file}
			if test.factory {
				watch.NewStatus = func() *Status { return &Status{} }
			}
			err = watch.Run(t.Context(), cli.Globals{ConfigDir: filepath.Join(t.TempDir(), "config")}, cli.CodexWatchOptions{Interval: test.interval})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Watch.Run() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWatchRunQuitsFromPTY(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = master.Close(); _ = slave.Close() }()
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "xterm-256color")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	quit := make(chan error, 1)
	go func() {
		var output strings.Builder
		var waiter tty.Readiness
		for ctx.Err() == nil {
			ready, err := waiter.WaitReadable(master, 20*time.Millisecond)
			if err != nil {
				quit <- err
				return
			}
			if !ready {
				continue
			}
			buf := make([]byte, 8192)
			n, err := master.Read(buf)
			if err != nil {
				quit <- err
				return
			}
			output.Write(buf[:n])
			if strings.Contains(output.String(), tui.HelpLine) {
				_, err := master.Write([]byte("q"))
				quit <- err
				return
			}
		}
		quit <- fmt.Errorf("watch did not render its first frame: %w; output=%q", ctx.Err(), output.String())
	}()
	root := t.TempDir()
	watch := Watch{Input: slave, Output: slave, NewStatus: func() *Status {
		return &Status{Env: &codexprovider.Env{Home: filepath.Join(root, "absent-home")}}
	}}
	err = watch.Run(ctx, cli.Globals{ConfigDir: filepath.Join(root, "config")}, cli.CodexWatchOptions{Interval: cli.WatchFloor})
	if keyErr := <-quit; keyErr != nil {
		t.Fatal(keyErr)
	}
	if err != nil {
		t.Fatalf("Watch.Run() after quit = %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("watch ended by deadline instead of quit: %v", ctx.Err())
	}
}

func TestWatchReadOnlyPass(t *testing.T) {
	tests := map[string]struct {
		expired, forced  bool
		status, wantGets int
		state            codexprovider.StateKind
	}{
		"success: ordinary tick serves fresh cache":       {status: 200, wantGets: 1, state: codexprovider.StateOK},
		"success: forced tick bypasses cache":             {forced: true, status: 200, wantGets: 2, state: codexprovider.StateOK},
		"success: expired owned grant is never refreshed": {expired: true, status: 200, state: codexprovider.StateExpired},
		"error: unauthorized usage never refreshes":       {status: 401, wantGets: 2, state: codexprovider.StateUnauthorized},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			paths, err := config.Resolve(filepath.Join(root, "config"))
			if err != nil {
				t.Fatal(err)
			}
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := paths.EnsureCodexDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			namespace, err := paths.CodexNamespaceDir("watch-user", "watch-account")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(namespace, 0o700); err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(24 * time.Hour).Unix()
			if test.expired {
				expiry = 1000
			}
			jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, expiry)) + ".watch-test"
			auth := fmt.Appendf(nil, `{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"watch-test-refresh","account_id":"watch-account"}}`, jwt)
			authPath := filepath.Join(namespace, "auth.json")
			if err := os.WriteFile(authPath, auth, 0o600); err != nil {
				t.Fatal(err)
			}
			pending := filepath.Join(namespace, "auth.json.pending")
			if err := os.WriteFile(pending, []byte(`{"parked":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := config.UpdateRegistry(t.Context(), paths, func(registry *config.Registry) {
				registry.CodexAccounts = []config.CodexAccountRecord{{ChatGPTUserID: "watch-user", ChatGPTAccountID: "watch-account", Kind: config.CodexKindOwned(namespace, config.RefreshAuto), CreatedAt: "2026-09-17T00:00:00Z"}}
			}); err != nil {
				t.Fatal(err)
			}
			var gets, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
				} else {
					gets.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000}}}`))
			}))
			defer server.Close()
			t.Setenv("AGENTCTL_CODEX_TOKEN_URL", server.URL+"/oauth/token")
			status := &Status{Env: &codexprovider.Env{Home: filepath.Join(root, "home")}, Client: codexprovider.NewUsageClient(server.URL, "watch-test", time.Second)}
			watch := Watch{NewStatus: func() *Status { return status }}
			if _, err := watch.collect(t.Context(), paths, false); err != nil {
				t.Fatal(err)
			}
			rows, err := watch.collect(t.Context(), paths, test.forced)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("watch rows=%d, want 1", len(rows))
			}
			if diff := gocmp.Diff(test.state, rows[0].State.Kind); diff != "" {
				t.Fatalf("watch state (-want +got):\n%s", diff)
			}
			if gets.Load() != int32(test.wantGets) || posts.Load() != 0 {
				t.Fatalf("watch sent GET=%d POST=%d, want GET=%d POST=0", gets.Load(), posts.Load(), test.wantGets)
			}
			for path, want := range map[string][]byte{authPath: auth, pending: []byte(`{"parked":true}`)} {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatalf("watch changed %s (-want +got):\n%s", path, diff)
				}
			}
			if files, err := os.ReadDir(filepath.Join(paths.CodexRoot(), ".state")); err != nil || len(files) != 0 {
				t.Fatalf("watch created refresh state: entries=%v error=%v", files, err)
			}
		})
	}
}
