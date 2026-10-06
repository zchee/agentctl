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
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/errs"
	provider "github.com/zchee/agentctl/internal/provider/codex"
)

func TestAccountsRefreshTerminalConsentAndOneShotPOST(t *testing.T) {
	tests := map[string]struct {
		action string
		status int
		code   int
		posts  int32
		said   string
	}{
		"success: confirmed terminal resend stores rotated grant": {"resend", 200, 0, 1, "and it is stored"},
		"partial: unknown resend spends its single opportunity":   {"resend", 503, errs.ExitPartial, 1, "one re-send is spent"},
		"error: spent marker refuses after consent":               {"spent", 200, 2, 0, "already spent"},
		"error: too early marker refuses after consent":           {"early", 200, 2, 0, "waits an hour"},
		"success: reset lifts only floor and count":               {"reset", 200, 0, 0, "no token was sent"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			command, out, marker := refreshCommandFixture(t, false)
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
			command.Stdin = slave
			command.Prompt = commands.TerminalPrompt{In: slave, Out: out}
			var state provider.RefreshState
			before, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(before, &state); err != nil {
				t.Fatal(err)
			}
			if test.action == "spent" {
				state.Resent = true
			}
			if test.action == "early" {
				since := time.Now().UTC()
				state.AmbiguousSince = new(since)
				state.Inflight.SentAt = since
			}
			body, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, body, 0o600); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if r.Method != http.MethodPost {
					t.Error("refresh request is not POST")
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"access_token":"agentctl-test-codex-at-new","refresh_token":"agentctl-test-codex-rt-new"}`)
			}))
			defer server.Close()
			t.Setenv("AGENTCTL_CODEX_TOKEN_URL", server.URL)
			opts := cli.CodexAccountsRefreshOptions{ID: "user-a", Resend: test.action != "reset", ResetFloor: test.action == "reset", Yes: true}
			err = command.Run(t.Context(), opts)
			if diff := gocmp.Diff(test.code, errs.ExitCode(err)); diff != "" {
				t.Fatalf("%s error=%v", diff, err)
			}
			if diff := gocmp.Diff(test.posts, posts.Load()); diff != "" {
				t.Fatal(diff)
			}
			text := out.String()
			if err != nil {
				text += err.Error()
			}
			if !strings.Contains(text, test.said) {
				t.Fatalf("missing result: %s", text)
			}
			for _, needle := range []string{"agentctl-test-codex-at-", "agentctl-test-codex-rt-", "agentctl-test-codex-jwt", "Bearer ", "eyJ"} {
				if strings.Contains(text, needle) {
					t.Fatal("refresh output leaked a token carrier")
				}
			}
			read, err := provider.NewRefreshStateStore(command.Paths, "user-a", "account-a")
			if err != nil {
				t.Fatal(err)
			}
			current := read.Load(t.Context()).State
			if test.action == "reset" {
				if current.FloorMin != provider.DefaultRefreshFloorMin || current.DidNotHelp != 0 {
					t.Fatal("floor reset failed")
				}
				if diff := gocmp.Diff(state.Inflight, current.Inflight); diff != "" {
					t.Fatal("floor reset cleared unknown grant")
				}
			}
			if test.action == "resend" && test.status == 503 {
				if !current.Resent {
					t.Fatal("unknown resend did not spend opportunity")
				}
				out.Reset()
				second := command.Run(t.Context(), opts)
				if errs.ExitCode(second) != 2 || posts.Load() != 1 {
					t.Fatal("spent grant sent again")
				}
			}
		})
	}
}
