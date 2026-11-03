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

package provider

import (
	"encoding/json/jsontext"
	"math"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestJSONInt64(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		value string
		want  int64
		ok    bool
	}{
		"success: zero":             {value: "0", ok: true},
		"success: negative zero":    {value: "-0", ok: true},
		"success: negative integer": {value: "-42", want: -42, ok: true},
		"success: maximum":          {value: "9223372036854775807", want: math.MaxInt64, ok: true},
		"success: minimum":          {value: "-9223372036854775808", want: math.MinInt64, ok: true},
		"error: overflow":           {value: "9223372036854775808"},
		"error: underflow":          {value: "-9223372036854775809"},
		"error: fraction":           {value: "42.0"},
		"error: exponent":           {value: "4e2"},
		"error: uppercase exponent": {value: "4E2"},
		"error: string":             {value: `"42"`},
		"error: null":               {value: "null"},
		"error: boolean":            {value: "true"},
		"error: array":              {value: "[]"},
		"error: object":             {value: "{}"},
		"error: absent":             {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := JSONInt64(jsontext.Value(tt.value))
			if ok != tt.ok {
				t.Fatalf("JSONInt64(%q) ok = %v, want %v", tt.value, ok, tt.ok)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("JSONInt64(%q) (-want +got):\n%s", tt.value, diff)
			}
		})
	}
}
