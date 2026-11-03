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
	json "encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/testutil"
)

// seedLabelledOwned gives the world one owned namespace whose registry
// record carries the label `login --label` would have written, and,
// when live is set, a live keychain item for the same account so the
// identity view has two rows to fold.
func (w *statusWorld) seedLabelledOwned(t *testing.T, label string, live bool) {
	t.Helper()
	record := w.fixture.OwnedRecord(testutil.Acct, testutil.Org)
	record["label"] = label
	w.fixture.WriteRegistry([]any{record})
	w.fixture.WriteCredentials(testutil.Acct, testutil.Org, w.fixture.Blob("access-owned", "refresh-owned", testutil.FreshAt()))
	if live {
		w.status.Reader = unlockedKeychain(map[string][]byte{
			claude.ServiceName(w.status.Env): liveDocumentWithSiblings(t, w.fixture),
		})
	}
}

func TestStatusShowsTheLoginLabel(t *testing.T) {
	type labelledRow struct {
		Kind  string  `json:"kind"`
		Email *string `json:"email"`
		Label *string `json:"label"`
	}

	tests := map[string]struct {
		label string
		live  bool
		opts  cli.ClaudeStatusOptions
		want  func(t *testing.T, stdout string)
	}{
		"success: the table folds the label into the Account cell": {
			label: "work",
			opts:  cli.ClaudeStatusOptions{Accounts: ownedOnly},
			want: func(t *testing.T, stdout string) {
				if want := " " + testutil.Email + " (work) |"; !strings.Contains(stdout, want) {
					t.Errorf("the Account cell should read %q:\n%s", want, stdout)
				}
			},
		},
		"success: an unlabelled account keeps a bare Account cell": {
			opts: cli.ClaudeStatusOptions{Accounts: ownedOnly},
			want: func(t *testing.T, stdout string) {
				if want := " " + testutil.Email + " |"; !strings.Contains(stdout, want) {
					t.Errorf("the Account cell should read %q:\n%s", want, stdout)
				}
				if strings.Contains(stdout, testutil.Email+" (") {
					t.Errorf("an absent label must not print parentheses:\n%s", stdout)
				}
			},
		},
		"success: the JSON report carries the label beside the email": {
			label: "work",
			opts:  cli.ClaudeStatusOptions{JSON: true, Accounts: ownedOnly},
			want: func(t *testing.T, stdout string) {
				testutil.ValidateSchema(t, "status.v1.json", []byte(stdout))
				var document struct {
					Rows []labelledRow `json:"rows"`
				}
				if err := json.Unmarshal([]byte(stdout), &document); err != nil {
					t.Fatalf("decode the report: %v\n%s", err, stdout)
				}
				want := []labelledRow{{Kind: "owned", Email: new(testutil.Email), Label: new("work")}}
				if diff := gocmp.Diff(want, document.Rows); diff != "" {
					t.Errorf("rows mismatch (-want +got):\n%s\n%s", diff, stdout)
				}
			},
		},
		"success: the JSON report spells an absent label as null": {
			opts: cli.ClaudeStatusOptions{JSON: true, Accounts: ownedOnly},
			want: func(t *testing.T, stdout string) {
				testutil.ValidateSchema(t, "status.v1.json", []byte(stdout))
				if !strings.Contains(stdout, `"label": null`) {
					t.Errorf("the row should carry a null label:\n%s", stdout)
				}
			},
		},
		"success: the identity view keeps the label on the folded row": {
			label: "work",
			live:  true,
			opts:  cli.ClaudeStatusOptions{ByIdentity: true},
			want: func(t *testing.T, stdout string) {
				if want := testutil.Email + " (work)"; strings.Count(stdout, want) != 1 {
					t.Errorf("want exactly one folded row named %q:\n%s", want, stdout)
				}
				if !strings.Contains(stdout, "live+owned") {
					t.Errorf("the live row should have folded into the owned one:\n%s", stdout)
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			if tt.label == "" {
				world.seedOwned(t, world.fixture.Blob("access-owned", "refresh-owned", testutil.FreshAt()))
			} else {
				world.seedLabelledOwned(t, tt.label, tt.live)
			}

			stdout, err := world.run(t, tt.opts)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, stdout)
			}
			t.Logf("status output:\n%s", stdout)
			tt.want(t, stdout)
		})
	}
}
