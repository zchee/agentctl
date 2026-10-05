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
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/secret"
)

func TestTokenResponseContainsEveryRepresentation(t *testing.T) {
	const marker = "sk-ant-marker-never-render"
	access, err := secret.NewSecret([]byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	response := TokenResponse{AccessToken: access, RefreshToken: access, Scope: new(marker), TokenType: new(marker), Workspace: jsontext.Value(`{"name":"` + marker + `"}`), Account: &ExchangeAccount{UUID: marker}, Organization: &ExchangeOrganization{UUID: marker}}
	tests := map[string]struct{ value any }{"success: pointer": {value: &response}, "success: value": {value: response}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var logged bytes.Buffer
			slog.New(slog.NewJSONHandler(&logged, nil)).Info("response", "value", tt.value)
			encoded, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			for _, rendered := range []string{fmt.Sprintf("%v", tt.value), fmt.Sprintf("%+v", tt.value), fmt.Sprintf("%#v", tt.value), fmt.Sprintf("%s", tt.value), logged.String(), string(encoded)} {
				if strings.Contains(rendered, marker) || !strings.Contains(rendered, "REDACTED") {
					t.Fatalf("response containment failed: %s", rendered)
				}
			}
		})
	}
	legacy, err := response.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := response.MarshalJSONTo(jsontext.NewEncoder(&stream)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy)+stream.String(), marker) {
		t.Fatal("JSON hooks leaked response bytes")
	}
}

func TestMalformedSuccessfulRefreshNeverQuotesResponseBytes(t *testing.T) {
	const marker = "sk-ant-marker-never-render"
	tests := map[string]struct{ body string }{
		"error: malformed member value": {body: `{"access_token":"` + marker + `","expires_in":` + marker + `}`},
		"error: malformed member name":  {body: `{"access_token":"ok", "` + marker + `":}`},
		"error: truncated token":        {body: `{"access_token":"` + marker},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			client, err := NewOAuthClient(server.URL, server.URL, "agentctl-test")
			if err != nil {
				t.Fatal(err)
			}
			credentials := parseFixture(t, "credentials-new-blob.json")
			_, err = client.RefreshAccess(t.Context(), credentials)
			if err == nil || strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "sk-ant") {
				t.Fatalf("malformed response error = %v", err)
			}
		})
	}
}

func TestOrganizationPlanMappingsAreIndependentValues(t *testing.T) {
	first := organizationPlans()
	first[0][1] = "changed"
	if organizationPlans()[0][1] != "max" {
		t.Fatal("organization plan mapping is mutable across callers")
	}
}
