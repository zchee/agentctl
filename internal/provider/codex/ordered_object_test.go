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
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestOrderedObjectDecoders(t *testing.T) {
	tests := map[string]struct {
		input     string
		wantAuth  string
		wantUsage usageMembers
	}{
		"success: empty object": {
			input: "{}", wantAuth: "{}",
		},
		"success: distinct names retain wire order": {
			input:    `{"z":1,"a":2,"m":3}`,
			wantAuth: `{"z":1,"a":2,"m":3}`,
			wantUsage: usageMembers{
				{name: "z", value: jsontext.Value("1")},
				{name: "a", value: jsontext.Value("2")},
				{name: "m", value: jsontext.Value("3")},
			},
		},
		"success: repeated first middle and last names keep first position": {
			input:    `{"z":1,"a":2,"m":3,"a":4,"z":5,"m":6,"a":null}`,
			wantAuth: `{"z":5,"a":null,"m":6}`,
			wantUsage: usageMembers{
				{name: "z", value: jsontext.Value("5")},
				{name: "a", value: jsontext.Value("null")},
				{name: "m", value: jsontext.Value("6")},
			},
		},
		"success: escaped duplicate names share a position": {
			input:    `{"alpha":1,"beta":2,"\u0061lpha":3}`,
			wantAuth: `{"alpha":3,"beta":2}`,
			wantUsage: usageMembers{
				{name: "alpha", value: jsontext.Value("3")},
				{name: "beta", value: jsontext.Value("2")},
			},
		},
		"success: nested objects have independent member positions": {
			input:    `{"z":{"z":1,"a":2,"z":3},"a":[{"a":4,"z":5,"a":6}]}`,
			wantAuth: `{"z":{"z":3,"a":2},"a":[{"a":6,"z":5}]}`,
			wantUsage: usageMembers{
				{name: "z", value: jsontext.Value(`{"z":1,"a":2,"z":3}`)},
				{name: "a", value: jsontext.Value(`[{"a":4,"z":5,"a":6}]`)},
			},
		},
		"success: duplicate replaces the entire prior object": {
			input:    `{"z":{"old":1},"a":2,"z":{"new":3}}`,
			wantAuth: `{"z":{"new":3},"a":2}`,
			wantUsage: usageMembers{
				{name: "z", value: jsontext.Value(`{"new":3}`)},
				{name: "a", value: jsontext.Value("2")},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Run("auth", func(t *testing.T) {
				input := []byte(tt.input)
				value, err := readAuthValue(jsontext.NewDecoder(bytes.NewReader(input), jsontext.AllowDuplicateNames(true)))
				if err != nil {
					t.Fatal(err)
				}
				defer value.wipe()
				clear(input)
				var output bytes.Buffer
				if err := value.write(&output, 0, false); err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(tt.wantAuth, output.String()); diff != "" {
					t.Fatalf("auth output (-want +got):\n%s", diff)
				}
			})
			t.Run("usage", func(t *testing.T) {
				input := []byte(tt.input)
				members, err := usageObject(input)
				if err != nil {
					t.Fatal(err)
				}
				clear(input)
				if diff := gocmp.Diff(tt.wantUsage, members, gocmp.AllowUnexported(usageMember{})); diff != "" {
					t.Fatalf("usage members (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func orderedObjectBenchmarkInput(count int) []byte {
	input := make([]byte, 0, count*20)
	input = append(input, '{')
	for index := range count {
		if index > 0 {
			input = append(input, ',')
		}
		input = fmt.Appendf(input, `"member_%05d":%d`, index, index)
	}
	return append(input, '}')
}

func BenchmarkReadAuthValueMembers(b *testing.B) {
	tests := map[string]struct{ members int }{
		"5000": {5000}, "10000": {10000}, "20000": {20000},
	}
	for name, tt := range tests {
		b.Run(name, func(b *testing.B) {
			input := orderedObjectBenchmarkInput(tt.members)
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			for b.Loop() {
				value, err := readAuthValue(jsontext.NewDecoder(bytes.NewReader(input), jsontext.AllowDuplicateNames(true)))
				if err != nil {
					b.Fatal(err)
				}
				value.wipe()
			}
		})
	}
}

func BenchmarkUsageObjectMembers(b *testing.B) {
	tests := map[string]struct{ members int }{
		"5000": {5000}, "10000": {10000}, "20000": {20000},
	}
	for name, tt := range tests {
		b.Run(name, func(b *testing.B) {
			input := orderedObjectBenchmarkInput(tt.members)
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := usageObject(input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
