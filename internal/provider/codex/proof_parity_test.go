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
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func TestOwnedProofKindsAndIDs(t *testing.T) {
	tests := map[string]struct {
		user, account string
		kind          config.CodexKind
		valid         bool
	}{
		"success: auto owned":  {"user-one", "acct-one", config.CodexKindOwned("/somewhere/else", config.RefreshAuto), true},
		"success: never owned": {"user-one", "acct-one", config.CodexKindOwned("/somewhere/else", config.RefreshNever), true},
		"error: live":          {"user-one", "acct-one", config.CodexKindLive(), false},
		"error: read only":     {"user-one", "acct-one", config.CodexKindHomeReadOnly("/somewhere/else"), false},
		"error: traversal":     {"../x", "acct-one", config.CodexKindOwned("x", config.RefreshAuto), false},
		"error: account path":  {"user-one", "b/c", config.CodexKindOwned("x", config.RefreshAuto), false},
		"error: reserved user": {".locks", "acct-one", config.CodexKindOwned("x", config.RefreshAuto), false},
		"error: empty account": {"user-one", "", config.CodexKindOwned("x", config.RefreshAuto), false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			proof := Owned(&config.CodexAccountRecord{ChatGPTUserID: tt.user, ChatGPTAccountID: tt.account, Kind: tt.kind})
			if (proof != nil) != tt.valid {
				t.Fatalf("valid=%t; expected=%t", proof != nil, tt.valid)
			}
			if proof != nil && (proof.User() != tt.user || proof.Account() != tt.account || proof.ExportSpelling() == nil || *proof.ExportSpelling() != "/somewhere/else" || proof.Refresh() != tt.kind.Owned.Refresh) {
				t.Fatal("owned proof lost record fields")
			}
		})
	}
}

func TestRefreshChangesOnlyPermittedLeaves(t *testing.T) {
	tests := map[string]struct {
		body    string
		changes []string
	}{
		"success: full rotation changes four leaves":             {body: fmt.Sprintf(`{"id_token":%q,"access_token":"agctl-test-access-new","refresh_token":"agctl-test-refresh-new"}`, syntheticJWT(`{"email":"new@example.invalid","https://api.openai.com/auth":{"chatgpt_user_id":"user-one","chatgpt_account_id":"acct-one"}}`)), changes: []string{"/last_refresh", "/tokens/access_token", "/tokens/id_token", "/tokens/refresh_token"}},
		"success: absent grant members stay unchanged":           {body: `{}`, changes: []string{"/last_refresh"}},
		"success: unreadable id token retains previous identity": {body: `{"id_token":"not-a-jwt","access_token":"agctl-test-access-new"}`, changes: []string{"/last_refresh", "/tokens/access_token"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, namespace, _, _ := writerNamespace(t)
			before := lockedRead(t, namespace)
			leaves := credentialLeaves(t, before.Credentials())
			response, err := ParseRefreshResponse([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			merged, _, err := before.MergeRefresh(response, time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			after := credentialLeaves(t, merged.Credentials())
			var changed []string
			for pointer, value := range leaves {
				if !reflect.DeepEqual(value, after[pointer]) {
					changed = append(changed, pointer)
				}
			}
			for pointer := range after {
				if _, present := leaves[pointer]; !present {
					changed = append(changed, pointer)
				}
			}
			slices.Sort(changed)
			if diff := gocmp.Diff(tt.changes, changed); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestPostExitReportEachAnomaly(t *testing.T) {
	clean := successfulChildReport(t, ScratchSurvey{})
	if !clean.clean() || len(clean.anomalies()) != 0 {
		t.Fatal("successful child did not produce a clean report")
	}
	failed := exec.CommandContext(t.Context(), "/usr/bin/false")
	if err := failed.Run(); err == nil {
		t.Fatal("expected child failure")
	}
	tests := map[string]struct {
		report *PostExitReport
		phrase string
	}{
		"error: failed exit":   {postExitReportFromChild(nil, nil, ScratchSurvey{}, failed.ProcessState), "exited with"},
		"error: missing exit":  {postExitReportFromChild(nil, nil, ScratchSurvey{}, nil), "no exit status"},
		"error: keychain item": {postExitReportFromChild([]string{"cli|0000"}, nil, ScratchSurvey{}, clean.exit), "keychain item"},
		"error: survivor":      {postExitReportFromChild(nil, []string{"private-scratch"}, ScratchSurvey{}, clean.exit), "still use"},
		"error: daemon":        {postExitReportFromChild(nil, nil, ScratchSurvey{daemonDir: true}, clean.exit), "daemon"},
		"error: held lock":     {postExitReportFromChild(nil, nil, ScratchSurvey{heldLocks: []string{"a.lock"}}, clean.exit), "still held"},
		"error: odd lock":      {postExitReportFromChild(nil, nil, ScratchSurvey{oddLocks: []string{"a.lock"}}, clean.exit), "not regular files"},
		"error: truncated":     {postExitReportFromChild(nil, nil, ScratchSurvey{truncated: true}, clean.exit), "survey completely"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.report.clean() {
				t.Fatal("an anomalous child was accepted")
			}
			found := tt.report.anomalies()
			if len(found) != 1 || !strings.Contains(found[0], tt.phrase) {
				t.Fatalf("wrong report shape: %v", found)
			}
			assertNoCredentialNeedles(t, strings.Join(found, "; "))
		})
	}
}

func TestProofAndReceiptFormattingContainsNoGrant(t *testing.T) {
	_, namespace, _, _ := writerNamespace(t)
	credentials := lockedRead(t, namespace)
	merged := mergedCredential(t, credentials)
	result, err := namespace.Write(t.Context(), merged)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := result.Receipt.Consume(); err != nil {
			t.Error(err)
		}
	}()
	scratch := t.TempDir()
	writeCodexFile(t, filepath.Join(scratch, authFile), writerCredentials(t, "agctl-test-login"))
	login, err := VerifyLogin(t.Context(), scratch, successfulChildReport(t, ScratchSurvey{}))
	if err != nil {
		t.Fatal(err)
	}
	assertNoCredentialNeedles(t, fmt.Sprintf("%v %+v %#v %v %#v %v %#v", credentials, credentials, credentials, login, login, result.Receipt, result.Receipt))
}

func TestCodexTimestampPrecision(t *testing.T) {
	tests := map[string]struct {
		nanos    int
		expected string
	}{
		"success: seconds":      {0, "2026-09-16T12:00:00Z"},
		"success: milliseconds": {123000000, "2026-09-16T12:00:00.123Z"},
		"success: microseconds": {123456000, "2026-09-16T12:00:00.123456Z"},
		"success: nanoseconds":  {123456789, "2026-09-16T12:00:00.123456789Z"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.expected, codexTimestamp(time.Date(2026, 9, 16, 12, 0, 0, tt.nanos, time.UTC))); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
