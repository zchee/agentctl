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

package commands

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func TestUseBuildSubject(t *testing.T) {
	tests := map[string]struct {
		live    bool
		owned   bool
		secure  *string
		moved   bool
		badHash bool
		missing bool
		symlink bool
		refusal claude.SwapRefusalKind
	}{
		"success: live store":                 {live: true},
		"success: live store through symlink": {live: true, symlink: true},
		"success: empty override names live":  {live: true, secure: new("")},
		"success: owned namespace":            {owned: true},
		"error: nonempty override":            {live: true, secure: new("unsafe\nvalue"), refusal: claude.SwapLiveNamespaceEnv},
		"error: missing live store":           {live: true, missing: true, refusal: claude.SwapLiveUnreachable},
		"error: missing namespace owner":      {refusal: claude.SwapNotOwned},
		"error: moved owned namespace":        {owned: true, moved: true, refusal: claude.SwapNotOwned},
		"error: malformed namespace suffix":   {owned: true, badHash: true, refusal: claude.SwapNotOwned},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.NewPaths(filepath.Join(home, "store"))
			env := claude.EnvWithHome(home)
			env.SecureStorageDir = test.secure
			if !test.missing {
				livePath := filepath.Join(home, ".claude")
				if test.symlink {
					realPath := filepath.Join(home, "actual")
					if err := os.Mkdir(realPath, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(realPath, livePath); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(livePath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var record *config.AccountRecord
			inherited := paths.NamespaceDir("account", "org")
			if test.owned {
				hash := claude.SHA8(inherited)
				if test.badHash {
					hash = "ABCDef12"
				}
				record = &config.AccountRecord{AccountUUID: "account", OrganizationUUID: "org", Kind: config.AccountKind{Owned: &config.OwnedKind{ExportSpelling: inherited, ExportSHA8: hash}}}
			}
			if test.moved {
				inherited += "-moved"
			}
			subject, report := useBuildSubject(paths, &env, test.live, record, inherited)
			if test.refusal != 0 {
				if subject != nil || report == nil || report.outcome.Refusal.Kind != test.refusal {
					t.Fatalf("subject=%+v report=%+v; want refusal %v", subject, report, test.refusal)
				}
				if test.secure != nil && strings.Contains(*report.note, "unsafe\nvalue") {
					t.Fatal("refusal emitted an unescaped environment value")
				}
				return
			}
			if report != nil || subject == nil {
				t.Fatalf("subject=%+v report=%+v", subject, report)
			}
			want := secret.TargetLive
			if !test.live {
				want = secret.NamespaceTarget(record.Kind.Owned.ExportSHA8)
			}
			if diff := gocmp.Diff(want, subject.audit); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUseIdentifyUsesProfileOrOwnAudit(t *testing.T) {
	tests := map[string]struct {
		status   int
		body     string
		expires  int64
		claimed  string
		write    bool
		legacy   bool
		refusal  claude.SwapRefusalKind
		requests int32
	}{
		"success: profile resolves credential":                 {status: 200, expires: 200, requests: 1},
		"success: expired credential resolved by own write":    {expires: 10, write: true},
		"success: revoked credential resolved by own write":    {status: 401, expires: 200, write: true, requests: 1},
		"success: forbidden credential resolved by own write":  {status: 403, expires: 200, write: true, requests: 1},
		"error: expired credential without audit":              {expires: 10, refusal: claude.SwapTokenExpired},
		"error: newer legacy write hides older identity":       {expires: 10, write: true, legacy: true, refusal: claude.SwapTokenExpired},
		"error: profile unavailable never falls back to audit": {status: 500, expires: 200, write: true, requests: 1, refusal: claude.SwapProfileUnavailable},
		"error: malformed profile never falls back to audit":   {status: 200, body: `{"account":{"uuid":"account"}}`, expires: 200, write: true, requests: 1, refusal: claude.SwapProfileUnavailable},
		"error: token identity conflicts with profile":         {status: 200, expires: 200, claimed: "other", requests: 1, refusal: claude.SwapCannotAdopt},
		"error: token identity conflicts with own write":       {expires: 10, write: true, claimed: "other", refusal: claude.SwapCannotAdopt},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer test-access" {
					t.Error("missing bearer header")
				}
				w.WriteHeader(test.status)
				body := test.body
				if body == "" {
					body = `{"account":{"uuid":"account","email":"owner@example.invalid"},"organization":{"uuid":"org"}}`
				}
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := claude.NewOAuthClient(server.URL, server.URL, "agentctl-test")
			if err != nil {
				t.Fatal(err)
			}
			item, err := claude.ParseBlob(fmt.Appendf(nil, `{"claudeAiOauth":{"accessToken":"test-access","refreshToken":"test-refresh","expiresAt":%d}}`, test.expires))
			if err != nil {
				t.Fatal(err)
			}
			if test.claimed != "" {
				item.TokenAccount = &claude.TokenAccount{UUID: new(test.claimed), OrganizationUUID: new("org")}
			}
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if test.write {
				event := &secret.WriteEvent{Target: secret.TargetLive, ToDigest8: "12345678", Direction: secret.DirectionForward, Outcome: secret.WriteUnknown, IncomingIdentity: &secret.IncomingIdentity{AccountUUID: "account", OrganizationUUID: new("org")}}
				if _, err := secret.AuditAppend(t.Context(), paths, secret.NewAuditEntry(event)); err != nil {
					t.Fatal(err)
				}
				if test.legacy {
					event.IncomingIdentity = nil
					if _, err := secret.AuditAppend(t.Context(), paths, secret.NewAuditEntry(event)); err != nil {
						t.Fatal(err)
					}
				}
			}
			identity, profile, failure := useIdentify(t.Context(), client, item, "12345678", func() (secret.AuditTail, error) { return secret.TailAuditLog(paths, 64) }, 100)
			if requests.Load() != test.requests {
				t.Fatalf("requests=%d, want %d", requests.Load(), test.requests)
			}
			if test.refusal != 0 {
				if failure == nil {
					t.Fatalf("identity=%+v profile=%+v; wanted refusal", identity, profile)
				}
				report := failure.report(claude.LiveService)
				if report.outcome.Refusal.Kind != test.refusal {
					t.Fatalf("refusal=%v, want %v", report.outcome.Refusal.Kind, test.refusal)
				}
				return
			}
			if failure != nil {
				t.Fatalf("failure=%+v", failure)
			}
			want := &claude.Identity{AccountUUID: "account", OrganizationUUID: new("org")}
			if diff := gocmp.Diff(want, identity); diff != "" {
				t.Fatal(diff)
			}
			if (profile != nil) != (test.status == 200) {
				t.Fatalf("unexpected retained profile: %v", profile != nil)
			}
		})
	}
}

func TestUseIdentityDisplayEscapesControlCharacters(t *testing.T) {
	tests := map[string]struct {
		value string
		want  string
	}{
		"success: normal text":        {value: "hello世界", want: "hello世界"},
		"success: control characters": {value: "a\n\r\t\x00\x1b", want: `a\n\r\t\0\u{1b}`},
		"success: quotes":             {value: "'\"\\", want: `\'\"\\`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(test.want, usePrintable(test.value)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUseEmitRefusalJSON(t *testing.T) {
	var out bytes.Buffer
	process := SessionProcess{Out: &out}
	report := useRefused(claude.SwapRefusal{Kind: claude.SwapTokenExpired}, claude.LiveService, "expired")
	if err := process.emitUse(report, true); err != nil {
		t.Fatal(err)
	}
	want := `{
  "adopted_to": null,
  "audit": {
    "id": null
  },
  "config": null,
  "from": {
    "digest8": null
  },
  "kind": "outcome",
  "lock": {
    "budget_ms": null,
    "break": null,
    "hold_ms": null
  },
  "note": "expired",
  "outcome": "refused",
  "reason": "live_token_expired",
  "refusal": null,
  "service": "Claude Code-credentials",
  "target": null,
  "to": {
    "digest8": null
  },
  "warnings": []
}
`
	if diff := gocmp.Diff(want, out.String()); diff != "" {
		t.Fatal(diff)
	}
}

func TestUseEmitAppliedAndPlan(t *testing.T) {
	var out, diagnostic bytes.Buffer
	process := SessionProcess{Out: &out, Err: &diagnostic}
	report := &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapApplied}, service: claude.LiveService, toDigest8: new("12345678"), note: new("cleanup failed"), adoptedTo: new("saved"), auditID: new("audit-id")}
	if err := process.emitUse(report, false); err != nil {
		t.Fatal(err)
	}
	want := "swapped: `Claude Code-credentials` now holds the incoming credential (digest 12345678). It takes effect on your next message, within 30 s; run `/model` once to refresh model access.\nthe displaced credential was adopted into `saved`\naudit: audit-id\n"
	if diff := gocmp.Diff(want, out.String()); diff != "" {
		t.Fatal(diff)
	}
	if diff := gocmp.Diff("warning: cleanup failed\n", diagnostic.String()); diff != "" {
		t.Fatal(diff)
	}
	out.Reset()
	if err := process.emitUsePlan(&useSubject{storeDir: "/store", service: claude.LiveService}, &config.AccountRecord{AccountUUID: "account", Email: new("owner@example.invalid")}, nil, "12345678", secret.DirectionUndo, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"direction": "reverse"`) || !strings.Contains(out.String(), `"kind": "plan"`) || !strings.Contains(out.String(), `"account": "owner@example.invalid"`) {
		t.Fatalf("unexpected plan: %s", out.String())
	}
}
