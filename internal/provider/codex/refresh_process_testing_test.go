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

package codex

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func TestRefreshProcessChild(t *testing.T) {
	root := os.Getenv("AGENTCTL_REFRESH_TEST_ROOT")
	if root == "" {
		return
	}
	paths := config.NewPaths(root)
	permit := &PostPermit{transport: NewRefreshClient(os.Getenv("AGENTCTL_CODEX_TOKEN_URL"), "child-test")}
	report := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
	if report.Step.Kind != RefreshStepRefreshed {
		t.Fatalf("child unexpectedly finished: %+v", report)
	}
}

func TestRefreshDurableMarkerSurvivesActualProcessTermination(t *testing.T) {
	tests := map[string]struct {
		phase  string
		parked bool
		kill   bool
		posts  int32
	}{
		"success: exit immediately after durable marker":  {"codex_abort_after_marker", false, false, 0},
		"success: kill after server consumed POST body":   {"", false, true, 1},
		"success: exit after pending before marker clear": {"codex_rename_fail,codex_abort_after_pending", true, false, 1},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, auth := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			received := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				if test.kill {
					received <- struct{}{}
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`)
			}))
			defer server.Close()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(t.Context(), binary, "-test.run=^TestRefreshProcessChild$")
			child.Env = append(os.Environ(), "AGENTCTL_REFRESH_TEST_ROOT="+paths.ConfigDir(), "AGENTCTL_CODEX_TOKEN_URL="+server.URL, "AGENTCTL_FAULT="+test.phase)
			child.Stdout, child.Stderr = io.Discard, io.Discard
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			if test.kill {
				timer := time.NewTimer(10 * time.Second)
				defer timer.Stop()
				select {
				case <-received:
				case <-timer.C:
					_ = child.Process.Kill()
					_ = child.Wait()
					t.Fatal("child never sent its request body")
				case <-t.Context().Done():
					_ = child.Process.Kill()
					_ = child.Wait()
					t.Fatal(t.Context().Err())
				}
				if err := child.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			err = child.Wait()
			exit, ok := errors.AsType[*exec.ExitError](err)
			if !ok || test.kill && exit.ExitCode() != -1 || !test.kill && exit.ExitCode() != 134 {
				t.Fatal("child did not terminate at the requested durability point")
			}
			before := store.Load(t.Context())
			if before.Kind != RefreshStatePresent || before.State.Inflight == nil {
				t.Fatal("process death lost the durable marker")
			}
			if test.parked {
				if _, err := os.Stat(filepath.Join(filepath.Dir(auth), "auth.json.pending")); err != nil {
					t.Fatal("process death lost the parked grant")
				}
			}
			report := RunRefresh(t.Context(), &PostPermit{transport: NewRefreshClient(server.URL, "parent-test")}, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			want := RefreshStepOutcomeUnknown
			if test.parked {
				want = RefreshStepAdopted
				found := false
				for _, note := range report.Notes {
					found = found || note.Kind == RefreshNotePendingReplayed
				}
				if !found || store.Load(t.Context()).State.Inflight != nil {
					t.Fatal("pending-first recovery did not settle the old marker")
				}
			} else if report.Step.Class != RefreshUnknownInterrupted {
				t.Fatal("crash was not classified as interrupted")
			}
			if diff := gocmp.Diff(want, report.Step.Kind); diff != "" {
				t.Fatalf("%s report=%+v", diff, report)
			}
			if diff := gocmp.Diff(test.posts, posts.Load()); diff != "" {
				t.Fatal("crash recovery repeated the POST")
			}
		})
	}
}
