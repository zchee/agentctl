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
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func TestCodexReceiptAttemptIsOneUseEvenWhenLogRefuses(t *testing.T) {
	tests := map[string]struct{ linked bool }{
		"success: applied write audited once":  {},
		"error: linked audit consumes receipt": {true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, _, _ := writerNamespace(t)
			merged := mergedCredential(t, lockedRead(t, namespace))
			written, err := namespace.Write(t.Context(), merged)
			if err != nil || written.Receipt == nil {
				t.Fatalf("write failed: %v", err)
			}
			copyReceipt := *written.Receipt
			if test.linked {
				if err := os.Symlink(paths.ConfigDir(), CodexAuditLogPath(paths)); err != nil {
					t.Fatal(err)
				}
			}
			err = AppendCodexReceipt(t.Context(), paths, written.Receipt)
			if (err != nil) != test.linked || !written.Receipt.Consumed() {
				t.Fatalf("audit refusal=%v consumed=%t", err, written.Receipt.Consumed())
			}
			if AppendCodexReceipt(t.Context(), paths, &copyReceipt) == nil {
				t.Fatal("copied receipt appended twice")
			}
			if test.linked {
				return
			}
			log, present, err := ReadCodexAudit(t.Context(), paths)
			if err != nil || !present || strings.Count(log, "\n") != 1 {
				t.Fatalf("audit line count or read: %v %t", err, present)
			}
			var entry CodexAuditEntry
			if err := json.Unmarshal([]byte(strings.TrimSpace(log)), &entry); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(AuditApplied, entry.Outcome); diff != "" {
				t.Fatal(diff)
			}
			if diff := gocmp.Diff(written.Receipt.Digest8After(), entry.Digest8After); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestCodexReceiptKindsHaveFixedAuditProjection(t *testing.T) {
	tests := map[string]struct {
		kind      WriteKind
		overwrite bool
		want      AuditOutcome
	}{
		"success: refresh":          {WriteRefreshApplied, false, AuditApplied},
		"success: pending saved":    {WriteRefreshSavedToPending, false, AuditSavedToPending},
		"success: external discard": {WriteDiscardedExternal, false, AuditDiscardedExternal},
		"success: pending replay":   {WritePendingReplayed, false, AuditPendingReplayed},
		"success: pending discard":  {WritePendingDiscarded, false, AuditPendingDiscarded},
		"success: login":            {WriteLoginInstall, false, AuditLoginInstall},
		"success: overwrite":        {WriteLoginInstall, true, AuditLoginOverwrite},
		"success: deletion":         {WriteDelete, false, AuditDelete},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureCodexDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			receipt := newWriteReceipt(test.kind, "user", "acct", nil, nil)
			receipt.state.overwrote = test.overwrite
			if err := AppendCodexReceipt(t.Context(), paths, receipt); err != nil {
				t.Fatal(err)
			}
			log, _, err := ReadCodexAudit(t.Context(), paths)
			if err != nil {
				t.Fatal(err)
			}
			var entry CodexAuditEntry
			if err := json.Unmarshal([]byte(strings.TrimSpace(log)), &entry); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, entry.Outcome); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
