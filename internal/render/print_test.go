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

package render

import (
	"bytes"
	"errors"
	"io"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestPrint(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		text string
		want string
	}{
		"success: empty output has one newline": {want: "\n"},
		"success: right padding survives":       {text: "row  ", want: "row  \n"},
		"success: interior newlines survive":    {text: "heading\nrow  ", want: "heading\nrow  \n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if err := Print(&output, tt.text); err != nil {
				t.Fatalf("Print: %v", err)
			}
			if diff := gocmp.Diff(tt.want, output.String()); diff != "" {
				t.Errorf("output mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPrintReturnsWriteError(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := Print(writer, "row  "); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Print error = %v, want %v", err, io.ErrClosedPipe)
	}
}
