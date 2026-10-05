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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexMarkerWait(t *testing.T) {
	tests := map[string]struct {
		present   bool
		wantError bool
	}{
		"success: reached marker":                             {present: true},
		"error: a_failed_wait_names_the_marker_it_waited_for": {wantError: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "never.reached")
			child := exec.CommandContext(t.Context(), "sleep", "30")
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			if test.present {
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := waitCodexMarker(marker, 5*time.Millisecond)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), marker) {
					t.Fatalf("wait error %v does not name marker %s", err, marker)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
