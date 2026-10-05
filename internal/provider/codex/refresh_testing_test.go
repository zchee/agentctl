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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestAppliedRefreshWriteRecoveryNeverRepeatsPOST(t *testing.T) {
	tests := map[string]struct {
		phase  string
		parked bool
	}{
		"success: failed rename parks and next pass replays": {"codex_rename_fail", true},
		"success: failed marker clear adopts rotated grant":  {"codex_refresh_state_dir_sync", false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, _ := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			if test.parked {
				t.Setenv("AGENTCTL_FAULT", test.phase)
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if !test.parked {
					t.Setenv("AGENTCTL_FAULT", test.phase)
				}
				_, _ = io.WriteString(w, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`)
			}))
			defer server.Close()
			permit := &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}
			report := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			if report.Step.Kind != RefreshStepRefreshed || report.Step.Parked != test.parked {
				t.Fatalf("unexpected recovery: %+v", report)
			}
			t.Setenv("AGENTCTL_FAULT", "")
			second := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			if diff := gocmp.Diff(RefreshStepAdopted, second.Step.Kind); diff != "" {
				t.Fatal(diff)
			}
			if posts.Load() != 1 || store.Load(t.Context()).State.Inflight != nil {
				t.Fatal("recovery repeated POST or failed to clear marker")
			}
			if test.parked {
				found := false
				for _, note := range second.Notes {
					found = found || note.Kind == RefreshNotePendingReplayed
				}
				if !found {
					t.Fatal("pending replay note missing")
				}
			}
		})
	}
}

func TestRefreshDriverMarkerFailuresDoNotSend(t *testing.T) {
	tests := map[string]struct{ phase string }{
		"error: temporary write": {"codex_refresh_state_write"},
		"error: file flush":      {"codex_refresh_state_file_sync"},
		"error: rename":          {"codex_refresh_state_rename"},
		"error: directory flush": {"codex_refresh_state_dir_sync"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, _ := writerNamespace(t)
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENTCTL_FAULT", test.phase)
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			report := RunRefresh(t.Context(), &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			if report.Step.Kind != RefreshStepStateUnavailable || posts.Load() != 0 {
				t.Fatal("failed durable marker authorized POST")
			}
		})
	}
}

func TestAuditRefusalNeverUndoesAppliedRefresh(t *testing.T) {
	paths, _, guard, auth := writerNamespace(t)
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(CodexAuditLogPath(paths), 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`)
	}))
	defer server.Close()
	report := RunRefresh(t.Context(), &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
	if report.Step.Kind != RefreshStepRefreshed {
		t.Fatalf("audit refusal undid write: %+v", report)
	}
	found := false
	for _, note := range report.Notes {
		found = found || note.Kind == RefreshNoteAuditLogRefused
	}
	if !found {
		t.Fatal("audit refusal note missing")
	}
	resolved := ReadAuth(t.Context(), filepath.Dir(auth))
	if resolved.Kind != ResolvedCredentials {
		t.Fatal("written credential unavailable")
	}
}
