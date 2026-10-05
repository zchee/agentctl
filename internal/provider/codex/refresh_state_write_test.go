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
	json "encoding/json/v2"
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

func refreshWriterStore(t *testing.T, paths *config.Paths, guard *Lock) *RefreshStateStore {
	t.Helper()
	user, account := guard.IDs()
	store, err := NewRefreshStateStore(paths, user, account)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDurableMarkerAuthorizesOneLockedPOST(t *testing.T) {
	tests := map[string]struct {
		change    string
		wantCalls int32
	}{
		"success: durable grant":         {wantCalls: 1},
		"error: zero authorization":      {change: "zero"},
		"error: released namespace lock": {change: "release"},
		"error: another namespace lock":  {change: "lock"},
		"error: replaced grant":          {change: "grant"},
		"error: cancelled before send":   {change: "cancel"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, guard, authPath := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			credentials := lockedRead(t, namespace)
			token, err := store.writeInflight(t.Context(), credentials)
			if err != nil {
				t.Fatal(err)
			}
			marker := store.Load(t.Context())
			if marker.Kind != RefreshStatePresent || marker.State.Inflight == nil || marker.State.LastSentAt == nil {
				t.Fatalf("missing durable send marker: %+v", marker)
			}
			digest, _ := credentials.RefreshDigest8()
			if diff := gocmp.Diff(digest, marker.State.Inflight.SentDigest8); diff != "" {
				t.Fatal(diff)
			}
			info, err := os.Stat(store.shown)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("marker permissions: %v %v", info, err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				onDisk := store.Load(t.Context())
				if onDisk.Kind != RefreshStatePresent || onDisk.State.Inflight == nil {
					t.Error("POST preceded durable marker")
				}
				var body struct {
					ClientID     string `json:"client_id"`
					GrantType    string `json:"grant_type"`
					RefreshToken string `json:"refresh_token"`
				}
				if err := json.UnmarshalRead(r.Body, &body); err != nil || body.ClientID != ClientID || body.GrantType != "refresh_token" || body.RefreshToken != "agctl-test-refresh-old" {
					t.Error("wrong refresh body")
				}
				_, _ = io.WriteString(w, `{"access_token":"test-access-new","refresh_token":"test-refresh-new"}`)
			}))
			defer server.Close()
			ctx := t.Context()
			switch test.change {
			case "zero":
				token = &InflightToken{}
			case "release":
				if err := guard.Release(); err != nil {
					t.Fatal(err)
				}
			case "lock":
				_, other, _, _ := writerNamespace(t)
				credentials = lockedRead(t, other)
			case "grant":
				writeCodexFile(t, authPath, writerCredentials(t, "another-grant"))
				credentials = lockedRead(t, namespace)
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			copyToken := *token
			client := NewRefreshClient(server.URL, "agentctl/test")
			first := refreshOAuth(ctx, credentials, token, client)
			second := refreshOAuth(ctx, credentials, &copyToken, client)
			if diff := gocmp.Diff(test.wantCalls, calls.Load()); diff != "" {
				t.Fatal(diff)
			}
			if second.Kind != RefreshPreSend {
				t.Fatalf("copied capability reused: %v", second)
			}
			if test.wantCalls == 1 && first.Kind != RefreshApplied {
				t.Fatalf("first send: %v", first)
			}
			if test.wantCalls == 0 && first.Kind != RefreshPreSend {
				t.Fatalf("invalid capability: %v", first)
			}
			entries, err := os.ReadDir(store.dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != store.name {
				t.Fatalf("temporary leaked: %v %v", entries, err)
			}
		})
	}
}

func TestRefreshStateLockAndStoreBinding(t *testing.T) {
	tests := map[string]struct{ change string }{
		"error: zero lock":            {"zero"},
		"error: released lock":        {"release"},
		"error: different store root": {"root"},
		"error: different namespace":  {"namespace"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, _, guard, _ := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			switch test.change {
			case "zero":
				guard = &Lock{}
			case "release":
				if err := guard.Release(); err != nil {
					t.Fatal(err)
				}
			case "root":
				store = refreshWriterStore(t, config.NewPaths(t.TempDir()), guard)
			case "namespace":
				var err error
				store, err = NewRefreshStateStore(paths, "another-user", "acct-one")
				if err != nil {
					t.Fatal(err)
				}
			}
			if store.resetFloor(t.Context(), guard) == nil {
				t.Fatal("unbound mutation accepted")
			}
			if read := store.Load(t.Context()); read.Kind != RefreshStateAbsent {
				t.Fatalf("unbound mutation wrote marker: %+v", read)
			}
		})
	}
}

func TestRefreshStateMutationsAndLoginReset(t *testing.T) {
	paths, namespace, guard, _ := writerNamespace(t)
	store := refreshWriterStore(t, paths, guard)
	credentials := lockedRead(t, namespace)
	if _, err := store.writeInflight(t.Context(), credentials); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writeInflight(t.Context(), credentials); err == nil {
		t.Fatal("second ordinary send authorized")
	}
	if err := store.markInterrupted(t.Context(), guard); err != nil {
		t.Fatal(err)
	}
	state := store.Load(t.Context()).State
	if state.Class == nil || *state.Class != RefreshUnknownInterrupted || state.AmbiguousSince == nil {
		t.Fatal("interruption not recorded")
	}
	if _, err := store.writeResend(t.Context(), credentials); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writeResend(t.Context(), credentials); err == nil {
		t.Fatal("second resend authorized")
	}
	wait := 125 * time.Second
	if err := store.markUnknown(t.Context(), guard, RefreshUnknownRateLimited, time.Now(), &wait); err != nil {
		t.Fatal(err)
	}
	state = store.Load(t.Context()).State
	if state.RetryAfter == nil || *state.RetryAfter != 125 || !state.Resent {
		t.Fatal("resend facts lost")
	}
	for range 3 {
		if err := store.recordDidNotHelp(t.Context(), guard); err != nil {
			t.Fatal(err)
		}
	}
	state = store.Load(t.Context()).State
	if state.DidNotHelp != 3 || state.FloorMin != 240 {
		t.Fatalf("wrong floor: %+v", state)
	}
	if err := store.resetFloor(t.Context(), guard); err != nil {
		t.Fatal(err)
	}
	state = store.Load(t.Context()).State
	if state.DidNotHelp != 0 || state.FloorMin != 60 || state.Inflight == nil || state.Class == nil {
		t.Fatal("floor reset changed grant state")
	}
	if err := ResetRefreshStateForLogin(t.Context(), paths, guard); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(NewRefreshState(), store.Load(t.Context()).State); diff != "" {
		t.Fatal(diff)
	}
	for i := range 2 {
		removed, err := RemoveRefreshState(t.Context(), paths, guard)
		if err != nil || removed != (i == 0) {
			t.Fatalf("removal=%t err=%v", removed, err)
		}
	}
}

func TestRefreshMarkerRejectsInvalidDigestMembers(t *testing.T) {
	members := map[string]struct{ set func(*RefreshState, string) }{
		"inflight.sent_digest8": {func(s *RefreshState, v string) { s.Inflight = &Inflight{SentDigest8: v, SentAt: time.Unix(1, 0).UTC()} }},
		"earliest_refresh.grant_digest8": {func(s *RefreshState, v string) {
			s.EarliestRefresh = &EarliestRefresh{GrantDigest8: v, At: time.Unix(1, 0).UTC()}
		}},
		"dead_digest8": {func(s *RefreshState, v string) { s.DeadDigest8 = new(v) }},
	}
	tests := map[string]struct {
		digest string
		valid  bool
	}{
		"success: lowercase hex": {"0123abcd", true},
		"error: uppercase":       {"0123ABCD", false},
		"error: too short":       {"0123abc", false},
		"error: too long":        {"0123abcde", false},
		"error: non hex":         {"private!", false},
	}
	for member, field := range members {
		t.Run(member, func(t *testing.T) {
			for name, test := range tests {
				t.Run(name, func(t *testing.T) {
					paths := config.NewPaths(t.TempDir())
					if err := paths.EnsureCodexDirs(t.Context()); err != nil {
						t.Fatal(err)
					}
					store, err := NewRefreshStateStore(paths, "user", "acct")
					if err != nil {
						t.Fatal(err)
					}
					state := NewRefreshState()
					field.set(&state, test.digest)
					data, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(store.shown, data, 0o600); err != nil {
						t.Fatal(err)
					}
					read := store.Load(t.Context())
					if test.valid {
						if read.Kind != RefreshStatePresent {
							t.Fatal(read.Reason)
						}
						return
					}
					if read.Kind != RefreshStateUnavailable || !strings.Contains(read.Reason, member) || strings.Contains(read.Reason, test.digest) {
						t.Fatalf("unsafe digest projection: %+v", read)
					}
				})
			}
		})
	}
}

func TestRefreshMarkerRemovalNeverFollowsLeaf(t *testing.T) {
	paths, _, guard, _ := writerNamespace(t)
	store := refreshWriterStore(t, paths, guard)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store.shown); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveRefreshState(t.Context(), paths, guard)
	if err != nil || !removed {
		t.Fatalf("remove link: %t %v", removed, err)
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "keep" {
		t.Fatal("marker removal touched target")
	}
}
