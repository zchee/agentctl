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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	provider "github.com/zchee/agentctl/internal/provider/codex"
)

func refreshCommandFixture(t *testing.T, live bool) (*AccountsRefresh, *bytes.Buffer, string) {
	t.Helper()
	paths := config.NewPaths(t.TempDir())
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Fatal(err)
	}
	kind := config.CodexKindOwned("/not-a-write-target", config.RefreshAuto)
	if live {
		kind = config.CodexKindLive()
	}
	if err := config.UpdateRegistry(t.Context(), paths, func(registry *config.Registry) {
		registry.CodexAccounts = []config.CodexAccountRecord{{ChatGPTUserID: "user-a", ChatGPTAccountID: "account-a", Kind: kind, CreatedAt: "2026-09-17T00:00:00Z"}}
	}); err != nil {
		t.Fatal(err)
	}
	namespace, err := paths.CodexNamespaceDir("user-a", "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(namespace, 0o700); err != nil {
		t.Fatal(err)
	}
	claims := `{"https://api.openai.com/auth":{"chatgpt_user_id":"user-a","chatgpt_account_id":"account-a"}}`
	id := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".agentctl-test-codex-jwt"
	doc := fmt.Appendf(nil, `{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":"agentctl-test-codex-at-old","refresh_token":"agentctl-test-codex-rt-old","account_id":"account-a"},"last_refresh":"2026-09-16T00:00:00Z"}`, id)
	if err := os.WriteFile(filepath.Join(namespace, "auth.json"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := paths.CodexRefreshStatePath("user-a", "account-a")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("agentctl-test-codex-rt-old"))
	since := time.Now().Add(-2 * time.Hour).UTC()
	state := provider.NewRefreshState()
	state.Inflight = &provider.Inflight{SentDigest8: hex.EncodeToString(sum[:4]), SentAt: since}
	state.AmbiguousSince = new(since)
	state.Class = new(provider.RefreshUnknownServerError)
	state.FloorMin = 240
	state.DidNotHelp = 3
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	out := new(bytes.Buffer)
	return &AccountsRefresh{Paths: paths, Stdin: input, Prompt: commands.TerminalPrompt{In: input, Out: out}}, out, path
}

func TestAccountsRefreshNonTerminalRefusesBeforeOpeningStore(t *testing.T) {
	tests := map[string]struct {
		opts cli.CodexAccountsRefreshOptions
		live bool
		code int
		said string
	}{
		"error: no action":                                {cli.CodexAccountsRefreshOptions{ID: "user-a"}, false, 1, "--reset-floor"},
		"error: unknown account":                          {cli.CodexAccountsRefreshOptions{ID: "nobody", Resend: true, Yes: true}, false, 1, "no account matches"},
		"error: read-only account":                        {cli.CodexAccountsRefreshOptions{ID: "user-a", Resend: true, Yes: true}, true, 2, "stores no credential"},
		"error: yes never overrides terminal resend rule": {cli.CodexAccountsRefreshOptions{ID: "user-a", Resend: true, Yes: true}, false, 2, "interactive terminal"},
		"error: resend without yes":                       {cli.CodexAccountsRefreshOptions{ID: "user-a", Resend: true}, false, 2, "revoking the grant"},
		"error: reset floor without terminal":             {cli.CodexAccountsRefreshOptions{ID: "user-a", ResetFloor: true, Yes: true}, false, 2, "interactive terminal"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			command, out, marker := refreshCommandFixture(t, test.live)
			before, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			err = command.Run(t.Context(), test.opts)
			if diff := gocmp.Diff(test.code, errs.ExitCode(err)); diff != "" {
				t.Fatalf("%s error=%v", diff, err)
			}
			if err == nil || !strings.Contains(err.Error(), test.said) {
				t.Fatalf("missing explanation: %v", err)
			}
			after, readErr := os.ReadFile(marker)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if diff := gocmp.Diff(before, after); diff != "" {
				t.Fatal("refusal mutated refresh marker")
			}
			lock, err := command.Paths.CodexLockPath("user-a", "account-a")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(lock); !os.IsNotExist(err) {
				t.Fatal("refusal opened namespace lock")
			}
			if out.Len() != 0 {
				t.Fatal("refusal emitted success output")
			}
		})
	}
}

func TestAccountsRefreshDeclinedTerminalConsentChangesNothing(t *testing.T) {
	tests := map[string]struct {
		reset  bool
		answer string
		said   string
	}{
		"success: resend declined":          {false, "no\n", "nothing was sent"},
		"success: empty resend answer":      {false, "\n", "nothing was sent"},
		"success: floor reset declined":     {true, "no\n", "nothing was changed"},
		"success: empty floor reset answer": {true, "\n", "nothing was changed"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			command, out, marker := refreshCommandFixture(t, false)
			before, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
			if _, err := master.WriteString(test.answer); err != nil {
				t.Fatal(err)
			}
			command.Stdin = slave
			command.Prompt = commands.TerminalPrompt{In: slave, Out: out}
			opts := cli.CodexAccountsRefreshOptions{ID: "user-a", Resend: !test.reset, ResetFloor: test.reset}
			if err := command.Run(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), test.said) {
				t.Fatal("declined consent omitted the no-op explanation")
			}
			after, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(before, after); diff != "" {
				t.Fatal("declined consent changed the refresh marker")
			}
			lock, err := command.Paths.CodexLockPath("user-a", "account-a")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(lock); !os.IsNotExist(err) {
				t.Fatal("declined consent opened the namespace lock")
			}
		})
	}
}

func TestRefreshResendReportAndFixedExplanations(t *testing.T) {
	tests := map[string]struct {
		step provider.RefreshStep
		code int
		said string
	}{
		"success: applied":            {provider.RefreshStep{Kind: provider.RefreshStepRefreshed}, 0, "and it is stored"},
		"success: parked":             {provider.RefreshStep{Kind: provider.RefreshStepRefreshed, Parked: true}, 0, "parked as pending"},
		"success: adopted":            {provider.RefreshStep{Kind: provider.RefreshStepAdopted, Reason: "fresh"}, 0, "not due for a refresh"},
		"error: spent":                {provider.RefreshStep{Kind: provider.RefreshStepResendRefused, Reason: "already_resent"}, 2, "already spent"},
		"error: busy":                 {provider.RefreshStep{Kind: provider.RefreshStepBusy}, 2, "nothing was sent"},
		"error: disabled":             {provider.RefreshStep{Kind: provider.RefreshStepDisabled}, 2, "--refresh auto"},
		"error: daemon":               {provider.RefreshStep{Kind: provider.RefreshStepSessionDetected, PID: 123}, 2, "process (123)"},
		"error: marker":               {provider.RefreshStep{Kind: provider.RefreshStepStateUnavailable, Reason: "state unreadable"}, 2, "state unreadable"},
		"error: not before":           {provider.RefreshStep{Kind: provider.RefreshStepNotBefore, Until: time.Unix(1, 0)}, 2, "asked for no refresh"},
		"error: 401 floor":            {provider.RefreshStep{Kind: provider.RefreshStepUnauthorizedFloor, Until: time.Unix(1, 0)}, 2, "401 refresh floor"},
		"error: terminal count":       {provider.RefreshStep{Kind: provider.RefreshStepUnauthorizedTerminal}, 2, "--reset-floor"},
		"error: stale":                {provider.RefreshStep{Kind: provider.RefreshStepStale, Reason: "cancelled"}, 2, "cancelled"},
		"partial: unknown":            {provider.RefreshStep{Kind: provider.RefreshStepOutcomeUnknown, Class: provider.RefreshUnknownServerError, Since: time.Unix(1, 0)}, errs.ExitPartial, "server_error"},
		"partial: needs login":        {provider.RefreshStep{Kind: provider.RefreshStepNeedsLogin, Reason: "dead"}, errs.ExitPartial, "called this grant dead"},
		"partial: external permanent": {provider.RefreshStep{Kind: provider.RefreshStepRacedExternal}, errs.ExitPartial, "newer grant was kept"},
		"partial: external applied":   {provider.RefreshStep{Kind: provider.RefreshStepDiscardedExternal}, errs.ExitPartial, "response was discarded"},
		"error: failed prerequisite":  {provider.RefreshStep{Kind: provider.RefreshStepFailed, Reason: "namespace unavailable"}, 2, "namespace unavailable"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportResend(commands.TerminalPrompt{Out: &out}, "user-a/account-a", provider.RefreshReport{Step: test.step, Notes: []provider.RefreshNote{{Kind: provider.RefreshNoteAuditLogRefused}}})
			if diff := gocmp.Diff(test.code, errs.ExitCode(err)); diff != "" {
				t.Fatalf("%s error=%v", diff, err)
			}
			text := out.String()
			if err != nil {
				text += err.Error()
			}
			if !strings.Contains(text, test.said) || !strings.Contains(text, "write it describes still happened") {
				t.Fatalf("missing fixed explanation: %s", text)
			}
		})
	}
}
