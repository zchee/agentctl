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
	"encoding/json/jsontext"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestUsageRawEmailSwapRemoval(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"success: email first":                          {`{"email":"private","first":1,"last":3}`, `{"last":3,"first":1}`},
		"success: email middle":                         {`{"first":1,"email":"private","middle":2,"last":3}`, `{"first":1,"last":3,"middle":2}`},
		"success: email last":                           {`{"first":1,"last":3,"email":"private"}`, `{"first":1,"last":3}`},
		"success: no email":                             {`{"first":1,"last":3}`, `{"first":1,"last":3}`},
		"success: only email":                           {`{"email":"private"}`, `{}`},
		"success: duplicate values keep first position": {`{"first":1,"email":"old","middle":2,"email":"private","last":3,"first":4}`, `{"first":4,"last":3,"middle":2}`},
		"success: vendor number spelling retained":      {`{"first":1.00,"email":"private","last":3e2}`, `{"first":1.00,"last":3e2}`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Normalize(jsontext.Value(test.body), usageAt(t), true)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, string(got.Raw)); diff != "" {
				t.Fatalf("raw bytes (-want +got):\n%s", diff)
			}
		})
	}
}
