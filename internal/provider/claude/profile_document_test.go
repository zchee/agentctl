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

package claude

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestProfileDocumentAllowsOrganizationOnly(t *testing.T) {
	tests := map[string]struct {
		body  string
		valid bool
	}{
		"success: organization without account":          {body: `{"organization":{"uuid":"org"}}`, valid: true},
		"error: malformed document never quotes content": {body: `{"private-marker": invalid}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			client, err := NewOAuthClient(server.URL, server.URL, "agentctl-test")
			if err != nil {
				t.Fatal(err)
			}
			credentials, err := ParseBlob([]byte(`{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000}}`))
			if err != nil {
				t.Fatal(err)
			}
			document, err := client.ProfileDocument(t.Context(), credentials)
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(tt.body, string(document)); diff != "" {
					t.Fatal(diff)
				}
				if _, err := client.ProfileOf(t.Context(), credentials); err == nil {
					t.Fatal("identity parser accepted organization-only profile")
				}
			} else if err == nil || strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("malformed document error = %v", err)
			}
		})
	}
}
