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

package secret

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/fixtures"
)

// dumpFixture returns the shared dump-keychain capture the parser is proved
// against.
func dumpFixture(t *testing.T) string {
	t.Helper()
	data, err := fixtures.FS.ReadFile("claude/security-dump.txt")
	if err != nil {
		t.Fatalf("read the dump fixture: %v", err)
	}
	return string(data)
}

func TestParseDumpFindsEveryServiceInTheFixture(t *testing.T) {
	t.Parallel()

	entries, err := parseDump(dumpFixture(t))
	if err != nil {
		t.Fatalf("parseDump() error = %v, want nil", err)
	}
	services := make([]string, 0, len(entries))
	for _, entry := range entries {
		services = append(services, entry.Service)
	}
	want := []string{
		"Claude Code-credentials",
		"Claude Code-credentials-5cdc535f",
		"Claude Code-credentials-6cdd6b98",
		"claude-switcher:alice@example.com",
		"claude-switcher:bob@example.com",
		"Claude Code-86c75be7",
		"Chrome Safe Storage",
	}
	if diff := gocmp.Diff(want, services); diff != "" {
		t.Errorf("services mismatch (-want +got):\n%s", diff)
	}
}

func TestParseDumpReadsTheAccountAndTimestamps(t *testing.T) {
	t.Parallel()

	entries, err := parseDump(dumpFixture(t))
	if err != nil {
		t.Fatalf("parseDump() error = %v, want nil", err)
	}
	if len(entries) == 0 {
		t.Fatal("the fixture has entries")
	}
	// The hex form is followed by the printable rendering, and the trailing
	// NUL security prints inside it is dropped.
	want := ServiceEntry{
		Service:    "Claude Code-credentials",
		Account:    "example",
		CreatedAt:  "20260908012005Z",
		ModifiedAt: "20260908012005Z",
	}
	if diff := gocmp.Diff(want, entries[0]); diff != "" {
		t.Errorf("live entry mismatch (-want +got):\n%s", diff)
	}
}

func TestParseDump(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		text    string
		want    []ServiceEntry
		wantErr string
	}{
		"success: a record with no service name is dropped": {
			text: "class: \"genp\"\n" +
				"attributes:\n" +
				"    \"acct\"<blob>=\"example\"\n" +
				"    \"svce\"<blob>=<NULL>\n" +
				"class: \"genp\"\n" +
				"attributes:\n" +
				"    \"svce\"<blob>=\"kept\"\n",
			want: []ServiceEntry{{Service: "kept"}},
		},
		"success: empty output parses as an empty listing": {
			text: "",
			want: nil,
		},
		"success: unrelated output parses as an empty listing": {
			text: "security: SecKeychainCopyDefault failed\n",
			want: nil,
		},
		"success: a null account stays empty": {
			text: "class: \"genp\"\n" +
				"attributes:\n" +
				"    \"acct\"<blob>=<NULL>\n" +
				"    \"svce\"<blob>=\"item\"\n",
			want: []ServiceEntry{{Service: "item"}},
		},
		"error: a kept attribute with no value is reported": {
			text: "class: \"genp\"\n" +
				"attributes:\n" +
				"    \"svce\"<blob>\n",
			want:    nil,
			wantErr: `attribute "svce" has no value`,
		},
		"error: a kept attribute with no quoted rendering is reported": {
			text: "class: \"genp\"\n" +
				"attributes:\n" +
				"    \"svce\"<blob>=0x6162\n",
			want:    nil,
			wantErr: `attribute "svce" has no quoted rendering`,
		},
		"error: the report names the line": {
			text: "class: \"genp\"\n" +
				"attributes:\n" +
				"    \"svce\"<blob>=\"kept\"\n" +
				"    \"mdat\"<timedate>=0xABCD\n",
			want:    []ServiceEntry{{Service: "kept"}},
			wantErr: "line 4",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDump(tt.text)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parseDump() error = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("parseDump() error = nil, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseDump() error = %v, want one containing %q", err, tt.wantErr)
				}
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("entries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
