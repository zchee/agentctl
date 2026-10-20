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
	"context"
	json "encoding/json/v2"
	"net/http"
	"strings"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/internal/usage"
)

// siblingCanary marks every synthetic sibling value, so a report that
// leaked any byte of one is caught by a substring search.
const siblingCanary = "SIBLING-CANARY-7f3a"

// liveDocumentWithSiblings is a keychain document in Claude Code's shape:
// the account credential plus an mcpOAuth member holding two synthetic
// servers and two other members a reader must not count.
func liveDocumentWithSiblings(t *testing.T, fixture *testutil.Fixture) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(fixture.Blob("access-live", "refresh-live", testutil.FreshAt())), &document); err != nil {
		t.Fatalf("decode the fixture blob: %v", err)
	}
	document["mcpOAuth"] = map[string]any{
		"server-a|0001": map[string]any{"serverName": "server-a", "accessToken": siblingCanary + "-a"},
		"server-b|0002": map[string]any{"serverName": "server-b", "accessToken": siblingCanary + "-b"},
	}
	document["mcpOAuthClientConfig"] = map[string]any{
		"server-a|0001": map[string]any{"clientSecret": siblingCanary + "-client"},
	}
	document["pluginSecrets"] = siblingCanary + "-plugin"
	blob, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the live document: %v", err)
	}
	return blob
}

// seedLiveAndOwned gives the world a live keychain item carrying MCP
// siblings and one owned namespace read from its credential file.
func (w *statusWorld) seedLiveAndOwned(t *testing.T) {
	t.Helper()
	w.seedOwned(t, w.fixture.Blob("access-owned", "refresh-owned", testutil.FreshAt()))
	w.status.Reader = unlockedKeychain(map[string][]byte{
		claude.ServiceName(w.status.Env): liveDocumentWithSiblings(t, w.fixture),
	})
}

func TestStatusReportsTheMCPOAuthEntryCount(t *testing.T) {
	type statusRowEntries struct {
		Kind            string `json:"kind"`
		Source          string `json:"source"`
		MCPOAuthEntries *int   `json:"mcp_oauth_entries"`
	}

	tests := map[string]struct {
		json bool
		want func(t *testing.T, stdout string)
	}{
		"success: the JSON report counts the live document's servers and a file's none": {
			json: true,
			want: func(t *testing.T, stdout string) {
				testutil.ValidateSchema(t, "status.v1.json", []byte(stdout))
				var document struct {
					Rows []statusRowEntries `json:"rows"`
				}
				if err := json.Unmarshal([]byte(stdout), &document); err != nil {
					t.Fatalf("decode the report: %v\n%s", err, stdout)
				}
				want := []statusRowEntries{
					{Kind: "live", Source: "keychain", MCPOAuthEntries: new(2)},
					{Kind: "owned", Source: "file", MCPOAuthEntries: new(0)},
				}
				if diff := gocmp.Diff(want, document.Rows); diff != "" {
					t.Errorf("rows mismatch (-want +got):\n%s\n%s", diff, stdout)
				}
			},
		},
		"success: the table appends the count to each reporting row's state": {
			want: func(t *testing.T, stdout string) {
				for _, want := range []string{
					"keychain service `Claude Code-credentials`; mcp_oauth_entries=2)",
					"mcp_oauth_entries=0)",
				} {
					if !strings.Contains(stdout, want) {
						t.Errorf("the table is missing %q:\n%s", want, stdout)
					}
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
			world.seedLiveAndOwned(t)

			stdout, err := world.run(t, cli.ClaudeStatusOptions{JSON: tt.json})
			if err != nil {
				t.Fatalf("run: %v\n%s", err, stdout)
			}
			tt.want(t, stdout)
			for _, leaked := range []string{siblingCanary, "server-a", "server-b", "clientSecret"} {
				if strings.Contains(stdout, leaked) {
					t.Errorf("the report carries a sibling value or name %q:\n%s", leaked, stdout)
				}
			}
		})
	}
}

// capturingUsage records every credential a pass authenticates with, and
// the sibling value buffers it carried at that moment, then answers with
// the real client so the pass runs exactly as it would in production.
type capturingUsage struct {
	next provider.UsageProvider[*usage.UsageSnapshot]

	mu          sync.Mutex
	credentials []*claude.Credentials
	values      [][]byte
}

func (c *capturingUsage) Fetch(ctx context.Context, account provider.AccountRef) (*usage.UsageSnapshot, error) {
	if credentials, ok := account.Auth.(*claude.Credentials); ok {
		c.mu.Lock()
		c.credentials = append(c.credentials, credentials)
		for _, member := range credentials.Siblings {
			// The slice header, not a copy: the test must see the very
			// bytes the wipe is supposed to zero.
			c.values = append(c.values, member.Value)
		}
		c.mu.Unlock()
	}
	return c.next.Fetch(ctx, account)
}

func TestStatusWipesEveryDiscoveredCredentialAfterThePass(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedLiveAndOwned(t)
	capture := &capturingUsage{next: world.status.Client}
	world.status.Client = capture

	if stdout, err := world.run(t, cli.ClaudeStatusOptions{JSON: true}); err != nil {
		t.Fatalf("run: %v\n%s", err, stdout)
	}

	if len(capture.credentials) != 2 {
		t.Fatalf("fetched with %d credentials, want the live and the owned one", len(capture.credentials))
	}
	// Three siblings in the live document: mcpOAuth, its client config
	// and the plugin secrets. Fewer would mean the test proves nothing.
	if len(capture.values) != 3 {
		t.Fatalf("captured %d sibling values, want 3", len(capture.values))
	}
	for i, credentials := range capture.credentials {
		if credentials.Siblings != nil || credentials.Extra != nil {
			t.Errorf("credential %d still holds %d siblings and %d extra members after the pass", i, len(credentials.Siblings), len(credentials.Extra))
		}
	}
	for i, value := range capture.values {
		if nonZero := countNonZero(value); nonZero != 0 {
			t.Errorf("sibling value %d keeps %d non-zero bytes of %d after the pass", i, nonZero, len(value))
		}
	}
}

func TestDoctorReportsTheMCPOAuthEntryCount(t *testing.T) {
	fixture := testutil.New(t)
	paths := config.NewPaths(fixture.ConfigDir())
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
	fixture.WriteCredentials(testutil.Acct, testutil.Org, fixture.Blob("access-owned", "refresh-owned", testutil.FreshAt()))
	env := claude.EnvWithHome(fixture.Home())
	reader := unlockedKeychain(map[string][]byte{claude.ServiceName(&env): liveDocumentWithSiblings(t, fixture)})

	var out bytes.Buffer
	command := Doctor{Paths: paths, Env: &env, Reader: reader, Out: &out}
	if err := command.Report(t.Context()); err != nil {
		t.Fatal(err)
	}

	var accountLines []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, " kind=") {
			accountLines = append(accountLines, strings.TrimSpace(line))
		}
	}
	want := []string{
		testutil.Acct + "  " + testutil.Email + "  kind=live source=keychain state=ok mcp_oauth_entries=2",
		testutil.Acct + "  " + testutil.Email + "  kind=owned source=file state=ok mcp_oauth_entries=0",
	}
	if diff := gocmp.Diff(want, accountLines); diff != "" {
		t.Errorf("account lines mismatch (-want +got):\n%s\n%s", diff, &out)
	}
	for _, leaked := range []string{siblingCanary, "server-a", "server-b", "clientSecret"} {
		if strings.Contains(out.String(), leaked) {
			t.Errorf("the report carries a sibling value or name %q:\n%s", leaked, &out)
		}
	}
}

func TestMCPOAuthEntries(t *testing.T) {
	tests := map[string]struct {
		document string
		want     *int
	}{
		"success: a document with no mcpOAuth member counts zero": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":1}}`,
			want:     new(0),
		},
		"success: an empty mcpOAuth object counts zero": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":1},"mcpOAuth":{}}`,
			want:     new(0),
		},
		"success: each server entry counts once": {
			document: `{"mcpOAuth":{"a|1":{},"b|2":{},"c|3":{}},"claudeAiOauth":{"accessToken":"a","expiresAt":1}}`,
			want:     new(3),
		},
		"success: a non-object mcpOAuth counts zero": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":1},"mcpOAuth":["a|1"]}`,
			want:     new(0),
		},
		"success: other members are never counted": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":1},"mcpOAuthClientConfig":{"a|1":{},"b|2":{}},"pluginSecrets":{"p":{}}}`,
			want:     new(0),
		},
		"success: a duplicated mcpOAuth counts its last value": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":1},"mcpOAuth":{"a|1":{}},"mcpOAuth":{"a|1":{},"b|2":{}}}`,
			want:     new(2),
		},
		"success: no credential reports no count": {},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var credentials *claude.Credentials
			if tt.document != "" {
				parsed, err := claude.ParseBlob([]byte(tt.document))
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				t.Cleanup(parsed.Wipe)
				credentials = parsed
			}
			if diff := gocmp.Diff(tt.want, mcpOAuthEntries(t.Context(), credentials)); diff != "" {
				t.Errorf("mcpOAuthEntries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWipeRowsZeroesEveryRowsMembers(t *testing.T) {
	parsed, err := claude.ParseBlob([]byte(`{"claudeAiOauth":{"accessToken":"a","expiresAt":1,"futureField":"` + siblingCanary + `"},"mcpOAuth":{"a|1":{"accessToken":"` + siblingCanary + `"}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Siblings) != 1 || len(parsed.Extra) != 1 {
		t.Fatalf("parsed %d siblings and %d extra members, want one of each", len(parsed.Siblings), len(parsed.Extra))
	}
	values := [][]byte{parsed.Siblings[0].Value, parsed.Extra[0].Value}
	rows := []claude.AccountRow{{ID: "no credential"}, {ID: "with siblings", Credentials: parsed}}

	wipeRows(rows)

	if parsed.Siblings != nil || parsed.Extra != nil {
		t.Errorf("wiped credential still holds %d siblings and %d extra members", len(parsed.Siblings), len(parsed.Extra))
	}
	for i, value := range values {
		if nonZero := countNonZero(value); nonZero != 0 {
			t.Errorf("value %d keeps %d non-zero bytes of %d", i, nonZero, len(value))
		}
	}
}

// countNonZero counts the bytes of b a wipe left behind.
func countNonZero(b []byte) int {
	n := 0
	for _, c := range b {
		if c != 0 {
			n++
		}
	}
	return n
}
