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
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestGoldenAcceptsTheTrackedOracle(t *testing.T) {
	const name = "render__table__tests__all_rows"
	Golden(t, name, ReadGolden(t, name))
	GoldenTrimmed(t, name, ReadGolden(t, name))
}

func TestGoldenTrimmedSeesThroughTheSnapshotNormalisation(t *testing.T) {
	const name = "render__table__tests__all_rows"
	// A real run's stdout may carry trailing padding on the final row and
	// a final newline the stored snapshot body lost; the normalising
	// comparator must accept it, and CRLF output must compare equal too.
	padded := append(ReadGolden(t, name), []byte("   \n")...)
	GoldenTrimmed(t, name, padded)
}

func TestGoldenComparatorDistinguishesTheTwoModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.golden")
	if err := os.WriteFile(path, []byte(" a \n b \n"), 0o644); err != nil {
		t.Fatalf("write scratch golden: %v", err)
	}

	tests := map[string]struct {
		got      string
		trim     bool
		wantFail bool
	}{
		"success: identical bytes pass exactly":         {got: " a \n b \n", trim: false, wantFail: false},
		"success: eof padding passes trimmed":           {got: " a \n b   \n\n", trim: true, wantFail: false},
		"success: crlf output passes trimmed":           {got: " a \r\n b \r\n", trim: true, wantFail: false},
		"error: eof padding fails the exact comparison": {got: " a \n b \n\n", trim: false, wantFail: true},
		"error: interior padding still fails trimmed":   {got: " a   \n b \n", trim: true, wantFail: true},
		"error: different body fails both":              {got: " a \n c \n", trim: true, wantFail: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := &recordingTB{TB: t}
			goldenCompare(recorder, path, []byte(tt.got), tt.trim)
			if recorder.failed != tt.wantFail {
				t.Fatalf("goldenCompare failed=%t, want %t", recorder.failed, tt.wantFail)
			}
		})
	}
}

func TestGoldenUpdateRewritesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "case.golden")

	if err := flag.Set("update", "true"); err != nil {
		t.Fatalf("arm the update flag: %v", err)
	}
	defer func() {
		if err := flag.Set("update", "false"); err != nil {
			t.Fatalf("disarm the update flag: %v", err)
		}
	}()

	goldenCompare(t, path, []byte("fresh bytes\n"), false)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the updated golden: %v", err)
	}
	if string(data) != "fresh bytes\n" {
		t.Fatalf("updated golden holds %q, want the compared bytes", data)
	}
}
