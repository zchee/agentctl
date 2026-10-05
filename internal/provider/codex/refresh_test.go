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
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func driverContext(paths *config.Paths) RefreshContext {
	return RefreshContext{Paths: paths, Deadline: time.Now().Add(time.Minute), LockBudget: 10 * time.Millisecond}
}

func driverRecord() *OwnedRecord {
	record := ownedTestRecord("user-one", "acct-one")
	return Owned(&record)
}

func TestRefreshDriverSettlesExactlyOnePOST(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		step   RefreshStepKind
		class  RefreshUnknownClass
	}{
		"success: rotated credential lands":                  {200, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`, RefreshStepRefreshed, ""},
		"error: invalid grant becomes dead":                  {400, `{"error":"invalid_grant"}`, RefreshStepNeedsLogin, ""},
		"error: rejected request clears marker":              {400, `{"error":"invalid_request"}`, RefreshStepStale, ""},
		"error: rate limit remains unknown":                  {429, `{}`, RefreshStepOutcomeUnknown, RefreshUnknownRateLimited},
		"error: server failure remains unknown":              {503, `{}`, RefreshStepOutcomeUnknown, RefreshUnknownServerError},
		"error: unreadable applied response remains unknown": {200, `{}`, RefreshStepOutcomeUnknown, RefreshUnknownAmbiguous},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, auth := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			before, err := os.ReadFile(auth)
			if err != nil {
				t.Fatal(err)
			}
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
					t.Error("invalid refresh request shape")
				}
				w.Header().Set("Retry-After", "999999")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			permit := &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}
			report := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			if diff := gocmp.Diff(test.step, report.Step.Kind); diff != "" {
				t.Fatal(diff)
			}
			if posts.Load() != 1 {
				t.Fatalf("POST count=%d", posts.Load())
			}
			state := store.Load(t.Context())
			if state.Kind != RefreshStatePresent {
				t.Fatalf("marker: %+v", state)
			}
			after, err := os.ReadFile(auth)
			if err != nil {
				t.Fatal(err)
			}
			if test.step == RefreshStepRefreshed {
				if bytes.Equal(before, after) || state.State.Inflight != nil || !bytes.Contains(after, []byte("test-refresh-new")) {
					t.Fatal("applied grant was not durably settled")
				}
			} else if !bytes.Equal(before, after) {
				t.Fatal("non-applied response changed credential bytes")
			}
			if test.class != "" {
				if state.State.Class == nil || *state.State.Class != test.class || state.State.Inflight == nil {
					t.Fatal("unknown outcome not preserved")
				}
				if test.class == RefreshUnknownRateLimited && (state.State.RetryAfter == nil || *state.State.RetryAfter != uint64(MaxRefreshRetryAfter/time.Second)) {
					t.Fatal("retry-after not capped")
				}
				second := RunRefresh(t.Context(), permit, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
				if second.Step.Kind != RefreshStepOutcomeUnknown || posts.Load() != 1 {
					t.Fatal("unknown grant automatically resent")
				}
			} else if state.State.Inflight != nil {
				t.Fatal("definite response retained inflight")
			}
		})
	}
}

func TestRefreshDriverRefusalsNeverPOST(t *testing.T) {
	tests := map[string]struct {
		gate string
		step RefreshStepKind
	}{
		"success: fresh access is adopted":              {"fresh", RefreshStepAdopted},
		"error: disabled policy":                        {"disabled", RefreshStepDisabled},
		"error: phase-sum budget is too small":          {"budget", RefreshStepStale},
		"error: cancelled before lock":                  {"cancelled", RefreshStepStale},
		"error: live namespace lock is busy":            {"busy", RefreshStepBusy},
		"error: corrupted state":                        {"corrupted", RefreshStepStateUnavailable},
		"error: dead grant":                             {"dead", RefreshStepNeedsLogin},
		"error: unauthorized floor":                     {"floor", RefreshStepUnauthorizedFloor},
		"error: unauthorized terminal count":            {"terminal", RefreshStepUnauthorizedTerminal},
		"error: server grant floor":                     {"server_floor", RefreshStepNotBefore},
		"success: externally rotated access is adopted": {"external_access", RefreshStepAdopted},
		"error: interrupted send is never repeated":     {"interrupted", RefreshStepOutcomeUnknown},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, guard, auth := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			credentials := lockedRead(t, namespace)
			grant, _ := credentials.RefreshDigest8()
			access, _ := credentials.Credentials().AccessDigest8()
			mode := SendMode{Kind: SendProactive}
			refresh := driverContext(paths)
			owned := driverRecord()
			ctx := t.Context()
			switch test.gate {
			case "fresh":
				jwt := syntheticJWT(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))
				writeCodexFile(t, auth, bytes.ReplaceAll(writerCredentials(t, "agctl-test-refresh-old"), []byte(`"agctl-test-access"`), fmt.Appendf(nil, "%q", jwt)))
			case "disabled":
				record := ownedTestRecord("user-one", "acct-one")
				record.Kind.Owned.Refresh = config.RefreshNever
				owned = Owned(&record)
			case "budget":
				refresh.Deadline = time.Now().Add(time.Second)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "corrupted":
				if err := ResetRefreshStateForLogin(ctx, paths, guard); err != nil {
					t.Fatal(err)
				}
				writeCodexFile(t, filepath.Join(store.dir, store.name), []byte(`{"schema":0}`))
			case "dead", "floor", "terminal", "server_floor":
				if err := store.update(ctx, guard, func(state *RefreshState) (bool, error) {
					switch test.gate {
					case "dead":
						state.DeadDigest8 = new(grant)
					case "floor":
						state.LastSentAt = new(time.Now().UTC())
						mode = SendMode{Kind: SendAfterUnauthorized, RejectedAccessDigest8: access}
					case "terminal":
						state.DidNotHelp = TerminalDidNotHelp
						mode = SendMode{Kind: SendAfterUnauthorized, RejectedAccessDigest8: access}
					case "server_floor":
						state.EarliestRefresh = &EarliestRefresh{GrantDigest8: grant, At: time.Now().Add(time.Hour).UTC()}
					}
					return true, nil
				}); err != nil {
					t.Fatal(err)
				}
			case "external_access":
				mode = SendMode{Kind: SendAfterUnauthorized, RejectedAccessDigest8: "ffffffff"}
			case "interrupted":
				if _, err := store.writeInflight(ctx, credentials); err != nil {
					t.Fatal(err)
				}
			}
			if test.gate != "busy" {
				if err := guard.Release(); err != nil {
					t.Fatal(err)
				}
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			report := RunRefresh(ctx, &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}, owned, mode, refresh)
			if diff := gocmp.Diff(test.step, report.Step.Kind); diff != "" {
				t.Fatalf("%s report=%+v", diff, report)
			}
			if posts.Load() != 0 {
				t.Fatalf("refused gate sent %d POSTs", posts.Load())
			}
			if test.gate == "interrupted" && report.Step.Class != RefreshUnknownInterrupted {
				t.Fatal("interrupted class missing")
			}
		})
	}
}

func TestRefreshDriverResendAndRetryPolicy(t *testing.T) {
	tests := map[string]struct {
		action string
		kind   RefreshStepKind
	}{
		"error: spent resend":                                       {"spent", RefreshStepResendRefused},
		"error: too early resend":                                   {"early", RefreshStepResendRefused},
		"error: stray temporary":                                    {"stray", RefreshStepResendRefused},
		"error: zero consent":                                       {"zero", RefreshStepResendRefused},
		"success: one confirmed resend":                             {"resend", RefreshStepOutcomeUnknown},
		"success: definite never-sent restores consent eligibility": {"pre_send", RefreshStepOutcomeUnknown},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, guard, auth := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			credentials := lockedRead(t, namespace)
			grant, _ := credentials.RefreshDigest8()
			since := time.Now().Add(-2 * time.Hour).UTC()
			if test.action == "early" {
				since = time.Now().UTC()
			}
			if err := store.update(t.Context(), guard, func(state *RefreshState) (bool, error) {
				state.Inflight = &Inflight{SentDigest8: grant, SentAt: since}
				state.LastSentAt = new(since)
				state.AmbiguousSince = new(since)
				state.Class = new(RefreshUnknownServerError)
				state.Resent = test.action == "spent"
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			if test.action == "stray" {
				writeCodexFile(t, filepath.Join(filepath.Dir(auth), "auth.json.tmp.1234abcd"), []byte("stray"))
			}
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(503) }))
			defer server.Close()
			url := server.URL
			if test.action == "pre_send" {
				url = "http://127.0.0.1:9"
			}
			consent, err := NewResendConsent("yes", true, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.action == "zero" {
				consent = new(ResendConsent)
			}
			report := RunRefresh(t.Context(), &PostPermit{transport: NewRefreshClient(url, "driver-test")}, driverRecord(), SendMode{Kind: SendResend, Consent: consent}, driverContext(paths))
			if diff := gocmp.Diff(test.kind, report.Step.Kind); diff != "" {
				t.Fatalf("%s report=%+v", diff, report)
			}
			expectedPosts := int32(0)
			if test.action == "resend" {
				expectedPosts = 1
			}
			if posts.Load() != expectedPosts {
				t.Fatal("resend POST count drift")
			}
			state := store.Load(t.Context()).State
			if test.action == "resend" && !state.Resent {
				t.Fatal("resend not spent")
			}
			if test.action == "pre_send" && (state.Resent || state.Inflight == nil || state.Class == nil) {
				t.Fatal("never-sent did not restore unknown eligibility")
			}
		})
	}
}

func TestRefreshDriverPreservesExternalWritesDuringPOST(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		rotate bool
		step   RefreshStepKind
	}{
		"success: applied answer discards after external grant rotation": {200, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`, true, RefreshStepDiscardedExternal},
		"success: permanent answer adopts external grant rotation":       {400, `{"error":"invalid_grant"}`, true, RefreshStepRacedExternal},
		"success: same grant merges against latest metadata":             {200, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`, false, RefreshStepRefreshed},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, auth := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			externalGrant := "agctl-test-refresh-old"
			if test.rotate {
				externalGrant = "test-external-refresh"
			}
			external := bytes.ReplaceAll(writerCredentials(t, externalGrant), []byte(`[1,2]`), []byte(`[3,4]`))
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				other, err := AcquireCodex(t.Context(), paths, driverRecord(), 0)
				if err == nil {
					_ = other.Release()
					t.Error("namespace lock released during POST")
				}
				if err := os.WriteFile(auth, external, 0o600); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			report := RunRefresh(t.Context(), &PostPermit{transport: NewRefreshClient(server.URL, "driver-test")}, driverRecord(), SendMode{Kind: SendProactive}, driverContext(paths))
			if diff := gocmp.Diff(test.step, report.Step.Kind); diff != "" {
				t.Fatalf("%s report=%+v", diff, report)
			}
			after, err := os.ReadFile(auth)
			if err != nil {
				t.Fatal(err)
			}
			if test.rotate && !bytes.Equal(external, after) {
				t.Fatal("external grant overwritten")
			}
			if !test.rotate {
				var view struct {
					Future struct {
						Ordered []int `json:"ordered"`
					} `json:"future"`
				}
				if err := json.Unmarshal(after, &view); err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff([]int{3, 4}, view.Future.Ordered); diff != "" {
					t.Fatal(diff)
				}
				if !bytes.Contains(after, []byte(`test-refresh-new`)) {
					t.Fatal("rotated grant was not merged")
				}
			}
			if posts.Load() != 1 || store.Load(t.Context()).State.Inflight != nil {
				t.Fatal("external race settlement drift")
			}
		})
	}
}

func TestRefreshRetryRecordingAndFloorResetDoNotSend(t *testing.T) {
	paths, namespace, guard, _ := writerNamespace(t)
	store := refreshWriterStore(t, paths, guard)
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	permit := NewPostPermit()
	for range 3 {
		if err := RecordRetryGet(t.Context(), permit, driverRecord(), RetryGetUnauthorized, driverContext(paths)); err != nil {
			t.Fatal(err)
		}
	}
	state := store.Load(t.Context()).State
	if diff := gocmp.Diff(uint8(3), state.DidNotHelp); diff != "" {
		t.Fatal(diff)
	}
	if state.FloorMin != 4*DefaultRefreshFloorMin {
		t.Fatal("401 floor failed to cap")
	}
	if err := RecordRetryGet(t.Context(), permit, driverRecord(), RetryGetSucceeded, driverContext(paths)); err != nil {
		t.Fatal(err)
	}
	state = store.Load(t.Context()).State
	if state.FloorMin != DefaultRefreshFloorMin || state.DidNotHelp != 0 {
		t.Fatal("successful retry failed to lift floor")
	}
	if err := RecordRetryGet(t.Context(), new(PostPermit), driverRecord(), RetryGetUnauthorized, driverContext(paths)); err == nil {
		t.Fatal("zero POST permit accepted")
	}
	guard, err := AcquireCodex(t.Context(), paths, driverRecord(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = guard.Release() }()
	namespace, err = OpenOwnedNamespace(paths, driverRecord(), guard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = namespace.Close() }()
	consent, err := NewResetConsent("yes", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ResetRefreshFloor(t.Context(), paths, namespace, consent); err != nil {
		t.Fatal(err)
	}
	if err := ResetRefreshFloor(t.Context(), paths, namespace, consent); err == nil {
		t.Fatal("reset consent reused")
	}
	audit, exists, err := ReadCodexAudit(t.Context(), paths)
	if err != nil || !exists || !strings.Contains(audit, "floor_reset") {
		t.Fatal("reset audit missing")
	}
}
