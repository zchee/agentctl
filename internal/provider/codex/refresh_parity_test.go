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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRefreshAppliedResponseSurvivesCredentialDisruption(t *testing.T) {
	tests := map[string]struct {
		change string
		parked bool
		kind   RefreshStepKind
	}{
		"success: omitted refresh token keeps old grant": {"omit", false, RefreshStepRefreshed},
		"success: removed credential is replaced":        {"remove", false, RefreshStepRefreshed},
		"success: torn credential parks rotated grant":   {"torn", true, RefreshStepRefreshed},
		"success: unreadable credential parks grant":     {"unreadable", true, RefreshStepRefreshed},
		"error: no writable target retains unknown":      {"unwritable", false, RefreshStepOutcomeUnknown},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, auth := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				var err error
				switch test.change {
				case "remove":
					err = os.Remove(auth)
				case "torn":
					err = os.WriteFile(auth, []byte(`{"tokens":`), 0o600)
				case "unreadable":
					err = os.Chmod(auth, 0)
				case "unwritable":
					err = os.Chmod(filepath.Dir(auth), 0o500)
				}
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				if test.change == "omit" {
					_, _ = io.WriteString(w, `{"access_token":"test-access-new"}`)
				} else {
					_, _ = io.WriteString(w, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`)
				}
			}))
			defer server.Close()
			permit := &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}
			report := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			if test.change == "unwritable" {
				if err := os.Chmod(filepath.Dir(auth), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if diff := gocmp.Diff(test.kind, report.Step.Kind); diff != "" {
				t.Fatalf("%s report=%+v", diff, report)
			}
			if diff := gocmp.Diff(test.parked, report.Step.Parked); diff != "" {
				t.Fatal(diff)
			}
			if test.change == "unreadable" {
				blocked := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
				if blocked.Step.Kind != RefreshStepFailed || posts.Load() != 1 {
					t.Fatal("unresolved pending grant permitted another POST")
				}
				if err := os.Chmod(auth, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.change == "torn" {
				blocked := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
				if blocked.Step.Kind != RefreshStepFailed || posts.Load() != 1 {
					t.Fatal("torn credential with pending grant permitted another POST")
				}
				writeCodexFile(t, auth, writerCredentials(t, "agctl-test-refresh-old"))
			}
			if test.parked {
				pending, err := os.ReadFile(auth + ".pending")
				if err != nil || !bytes.Contains(pending, []byte("test-refresh-new")) {
					t.Fatal("rotated grant was not preserved as pending")
				}
			}
			if test.change == "omit" {
				body, err := os.ReadFile(auth)
				if err != nil || !bytes.Contains(body, []byte("agctl-test-refresh-old")) {
					t.Fatal("omitted refresh token discarded original grant")
				}
			}
			second := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			want := RefreshStepAdopted
			if test.change == "unwritable" {
				want = RefreshStepOutcomeUnknown
				state := store.Load(t.Context()).State
				if state.Inflight == nil || state.Class == nil || *state.Class != RefreshUnknownWriteFailed {
					t.Fatal("unsaved rotated grant lost its unknown marker")
				}
			}
			if diff := gocmp.Diff(want, second.Step.Kind); diff != "" {
				t.Fatalf("%s second=%+v", diff, second)
			}
			if diff := gocmp.Diff(int32(1), posts.Load()); diff != "" {
				t.Fatal("credential disruption repeated the refresh POST")
			}
		})
	}
}

func TestRefreshDaemonEvidenceIsCheckedUnderLiveLock(t *testing.T) {
	tests := map[string]struct {
		leaf   string
		resend bool
		torn   bool
		kind   RefreshStepKind
	}{
		"error: proactive legacy daemon blocks": {"app-server.pid", false, false, RefreshStepSessionDetected},
		"error: proactive second daemon blocks": {"daemon.pid", false, false, RefreshStepSessionDetected},
		"error: resend legacy daemon blocks":    {"app-server.pid", true, false, RefreshStepSessionDetected},
		"error: resend second daemon blocks":    {"daemon.pid", true, false, RefreshStepSessionDetected},
		"error: unreadable record blocks":       {"daemon.pid", false, true, RefreshStepStale},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, guard, auth := writerNamespace(t)
			mode := SendMode{Kind: SendProactive}
			if test.resend {
				store := refreshWriterStore(t, paths, guard)
				credentials := lockedRead(t, namespace)
				grant, _ := credentials.RefreshDigest8()
				since := time.Now().Add(-2 * time.Hour).UTC()
				if err := store.update(t.Context(), guard, func(state *RefreshState) (bool, error) {
					state.Inflight = &Inflight{SentDigest8: grant, SentAt: since}
					state.AmbiguousSince = new(since)
					state.Class = new(RefreshUnknownAmbiguous)
					return true, nil
				}); err != nil {
					t.Fatal(err)
				}
				consent, err := NewResendConsent("yes", true, false)
				if err != nil {
					t.Fatal(err)
				}
				mode = SendMode{Kind: SendResend, Consent: consent}
			}
			body := fmt.Appendf(nil, `{"pid":%d}`, os.Getpid())
			if test.torn {
				body = []byte(`{"pid":`)
			}
			writeCodexFile(t, filepath.Join(filepath.Dir(auth), "app-server-daemon", test.leaf), body)
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			report := RunRefresh(t.Context(), &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}, driverRecord(), mode, driverContext(paths))
			if diff := gocmp.Diff(test.kind, report.Step.Kind); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(int32(0), posts.Load()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
