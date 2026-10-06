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

package testutil

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestCodexLoginOutputOracle(t *testing.T) {
	tests := map[string]struct{ stdout, stderr, forbidden, want string }{
		"error: stdout needle reported by name and byte offset":           {stdout: "before agctl-test-codex-rt-0001 after", forbidden: "agctl-test-codex-rt-0001", want: "positive-control: stdout carries the needle `the refresh token` at byte 7"},
		"error: stderr needle reported by name and byte offset":           {stdout: "clean", stderr: "x agctl-test-codex-login-sig y", forbidden: "agctl-test-codex-login-sig", want: "positive-control: stderr carries the needle `the JWT signature` at byte 2"},
		"error: last needle is checked":                                   {stdout: "abc bearer xyz", want: "positive-control: stdout carries the needle `a bearer header` at byte 4"},
		"error: dropped receipt is rejected independently of exit status": {stderr: "agentctl unaudited write receipt: Delete was dropped before codex::audit::append\n", want: "positive-control: the binary dropped a Codex write receipt before the audit log: agentctl unaudited write receipt: Delete was dropped before codex::audit::append"},
		"success: clean streams pass":                                     {stdout: "nothing to see", stderr: "nor here"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := codexLoginOutputError("positive-control", tt.stdout, tt.stderr)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if tt.forbidden != "" && strings.Contains(got, tt.forbidden) {
				t.Fatal("oracle report contains the forbidden value")
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
