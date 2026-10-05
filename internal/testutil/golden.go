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
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	// The -update flag has exactly one owner: the shared golden package
	// registers it at initialisation, the TUI test library registers it
	// through the same package, and a second flag.Bool("update", ...)
	// here would panic any test binary that links both. Imported for the
	// registration; the value is read back through the flag set.
	_ "github.com/charmbracelet/x/exp/golden"
)

// updating reports whether this test run was asked to rewrite golden
// files: go test ./... -args -update.
func updating() bool {
	f := flag.Lookup("update")
	if f == nil {
		return false
	}
	getter, ok := f.Value.(flag.Getter)
	if !ok {
		return false
	}
	value, _ := getter.Get().(bool)
	return value
}

// GoldenPath returns the golden file for name under testdata/golden.
func GoldenPath(tb testing.TB, name string) string {
	tb.Helper()
	return filepath.Join(RepoRoot(tb), "testdata", "golden", name+".golden")
}

// ReadGolden returns one golden file's bytes.
func ReadGolden(tb testing.TB, name string) []byte {
	tb.Helper()
	data, err := os.ReadFile(GoldenPath(tb, name))
	if err != nil {
		tb.Fatalf("read golden %q: %v", name, err)
	}
	return data
}

// Golden compares got against the named golden file byte for byte. One
// exact-byte comparison per table output is what proves the trailing
// padding and final newline a normalising comparator cannot see.
func Golden(tb testing.TB, name string, got []byte) {
	tb.Helper()
	goldenCompare(tb, GoldenPath(tb, name), got, false)
}

// GoldenTrimmed compares got against the named golden file under the
// snapshot normalisation: CRLF becomes LF and trailing whitespace at end
// of file is removed, on both sides. Interior trailing padding stays, so
// seven of the table oracles - whose stored bodies lack the final row's
// right padding - compare equal to the padded output a real run emits.
func GoldenTrimmed(tb testing.TB, name string, got []byte) {
	tb.Helper()
	goldenCompare(tb, GoldenPath(tb, name), got, true)
}

// goldenCompare is the shared core, parameterised on the path so its own
// tests can run against scratch files instead of the tracked oracles.
func goldenCompare(tb testing.TB, path string, got []byte, trim bool) {
	tb.Helper()
	if updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatalf("create the golden directory: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			tb.Fatalf("update golden %q: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read golden %q (run with -args -update to create it): %v", path, err)
	}
	wantCmp, gotCmp := want, got
	if trim {
		wantCmp, gotCmp = normalizeEOF(want), normalizeEOF(got)
	}
	if !bytes.Equal(wantCmp, gotCmp) {
		tb.Fatalf("output does not match golden %q (-want +got):\n%s", path, gocmp.Diff(string(wantCmp), string(gotCmp)))
	}
}

// normalizeEOF applies the snapshot normalisation: every CRLF becomes LF,
// and whitespace at the very end of the file is removed. Nothing else
// changes, so padding inside the body survives.
func normalizeEOF(data []byte) []byte {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.TrimRight(normalized, " \t\n")
}
