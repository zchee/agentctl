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
	"math"
	"testing"
	"time"
)

func TestParseRetryAfterReadsBothFormsHTTPAllows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		value   string
		want    time.Duration
		wantSet bool
	}{
		"success: a delay in seconds":            {value: "30", want: 30 * time.Second, wantSet: true},
		"success: outer whitespace is trimmed":   {value: "  30  ", want: 30 * time.Second, wantSet: true},
		"success: zero seconds is an answer":     {value: "0", want: 0, wantSet: true},
		"success: an IMF-fixdate in the future":  {value: "Tue, 08 Sep 2026 00:02:00 GMT", want: 2 * time.Minute, wantSet: true},
		"success: a date already past means now": {value: "Mon, 07 Sep 2026 00:00:00 GMT", want: 0, wantSet: true},
		"success: a long delay is clamped":       {value: "18446744073709551615", want: time.Duration(math.MaxInt64), wantSet: true},
		"error: an empty value is no hint":       {value: "", wantSet: false},
		"error: prose is no hint":                {value: "soon", wantSet: false},
		"error: a negative delay is no hint":     {value: "-5", wantSet: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseRetryAfter(tt.value, now)
			if ok != tt.wantSet {
				t.Fatalf("ParseRetryAfter(%q) ok = %v, want %v", tt.value, ok, tt.wantSet)
			}
			if ok && got != tt.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
