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
	"os/exec"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestLoginChildExitRefusal(t *testing.T) {
	tests := map[string]struct {
		command string
		want    string
	}{
		"error: child exit code":    {"exit 17", "the Codex login was refused: the login exited with exit status: 17"},
		"error: child signal death": {"kill -TERM $$", "the Codex login was refused: the login exited with signal: 15 (SIGTERM)"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			child := exec.CommandContext(t.Context(), "/bin/sh", "-c", test.command)
			if err := child.Run(); err == nil {
				t.Fatal("child unexpectedly succeeded")
			}
			report := postExitReportFromChild(nil, nil, ScratchSurvey{}, child.ProcessState)
			login, err := VerifyLogin(t.Context(), t.TempDir(), report)
			if login != nil || err == nil {
				t.Fatal("failed child was not refused")
			}
			if diff := gocmp.Diff(test.want, err.Error()); diff != "" {
				t.Fatalf("refusal bytes (-want +got):\n%s", diff)
			}
		})
	}
}
