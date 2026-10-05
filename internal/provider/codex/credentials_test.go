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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func codexFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../../fixtures/codex", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func syntheticJWT(payload string) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".agctl-test-codex-jwt-signature"
}

func credentialOutput(t *testing.T, c *Credentials) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := c.WriteJSONTo(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustCredentials(t *testing.T, data []byte) *Credentials {
	t.Helper()
	c, err := ParseCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func credentialWithAccess(t *testing.T, exp *int64, refresh *time.Time) *Credentials {
	t.Helper()
	payload := "{}"
	if exp != nil {
		payload = fmt.Sprintf(`{"exp":%d}`, *exp)
	}
	var stamp *string
	if refresh != nil {
		stamp = new(refresh.Format(time.RFC3339Nano))
	}
	data, err := json.Marshal(struct {
		Tokens struct {
			Access string `json:"access_token"`
		} `json:"tokens"`
		Refresh *string `json:"last_refresh"`
	}{Tokens: struct {
		Access string `json:"access_token"`
	}{syntheticJWT(payload)}, Refresh: stamp})
	if err != nil {
		t.Fatal(err)
	}
	return mustCredentials(t, data)
}

func TestCredentialsRoundTrip(t *testing.T) {
	tests := map[string]struct{ name string }{"success: native format": {"auth-codex-format.json"}, "success: unknown positions": {"auth-unknown-members.json"}, "success: api key": {"auth-apikey.json"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			original := codexFixture(t, tt.name)
			actual := credentialOutput(t, mustCredentials(t, original))
			if diff := gocmp.Diff(string(original), string(actual)); diff != "" {
				t.Fatalf("ordered pretty output (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCredentialsArbitrarySecretsAndDuplicates(t *testing.T) {
	tests := map[string]struct{ input, want string }{"success: object secret": {`{"bedrock_access_keys":{"id":"agctl-test-id","secret":"agctl-test-secret","n":1.5}}`, "{\n  \"bedrock_access_keys\": {\n    \"id\": \"agctl-test-id\",\n    \"secret\": \"agctl-test-secret\",\n    \"n\": 1.5\n  }\n}"}, "success: last duplicate keeps first position": {`{"x":1,"y":2,"x":3}`, "{\n  \"x\": 3,\n  \"y\": 2\n}"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, string(credentialOutput(t, mustCredentials(t, []byte(tt.input))))); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestCredentialsAuthModes(t *testing.T) {
	tests := map[string]struct {
		doc   string
		mode  AuthMode
		usage bool
	}{
		"success: explicit chatgpt":         {`{"auth_mode":"chatgpt","tokens":{"access_token":"agctl-test-access"}}`, AuthChatGPT, true},
		"success: explicit token mode wins": {`{"auth_mode":"chatgptauthtokens","OPENAI_API_KEY":"agctl-test-key","tokens":{"access_token":"agctl-test-access"}}`, AuthChatGPTTokens, true},
		"success: explicit key":             {`{"auth_mode":"apikey","OPENAI_API_KEY":"agctl-test-key"}`, AuthAPIKey, false},
		"success: inferred key":             {`{"OPENAI_API_KEY":"agctl-test-key"}`, AuthAPIKey, false},
		"success: pat precedes key":         {`{"OPENAI_API_KEY":"agctl-test-key","personal_access_token":"agctl-test-pat"}`, AuthPersonalAccessToken, false},
		"success: bedrock key":              {`{"bedrock_api_key":"agctl-test-key"}`, AuthBedrockAPIKey, false},
		"success: bedrock access keys":      {`{"bedrock_access_keys":{"id":"agctl-test-id"}}`, AuthBedrockAccessKeys, false},
		"success: null key":                 {`{"OPENAI_API_KEY":null,"tokens":{"access_token":"agctl-test-access"}}`, AuthChatGPT, true},
		"success: unknown mode":             {`{"auth_mode":"workload"}`, AuthMode("workload"), false},
		"success: missing access":           {`{"auth_mode":"chatgpt"}`, AuthChatGPT, false},
		"success: mode hides hostile text":  {`{"auth_mode":"agctl-test-secret-too-long-0123456789"}`, AuthMode("<unrecognised>"), false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := mustCredentials(t, []byte(tt.doc))
			if diff := gocmp.Diff(tt.mode, c.AuthMode()); diff != "" {
				t.Fatal(diff)
			}
			if c.HasUsageSource() != tt.usage {
				t.Fatalf("has usage source=%t, want %t", c.HasUsageSource(), tt.usage)
			}
		})
	}
}

func TestCredentialsErrors(t *testing.T) {
	tests := map[string]struct {
		data        string
		kind, field string
	}{"error: empty": {"", "truncated", ""}, "error: cutoff": {`{"auth_mode":"chatgpt","tokens":{"access_`, "truncated", ""}, "error: invalid json": {"}{", "json", ""}, "error: array": {"[]", "object", ""}, "error: wrong tokens": {`{"tokens":"agctl-test-secret"}`, "type", "tokens"}, "error: wrong access": {`{"tokens":{"access_token":7}}`, "type", "access_token"}, "error: wrong mode": {`{"auth_mode":7}`, "type", "auth_mode"}, "error: bad id": {`{"tokens":{"id_token":"agctl-test-secret"}}`, "claims", ""}, "error: trailing object": {"{}{}", "json", ""}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCredentials([]byte(tt.data))
			failure, ok := errors.AsType[*CredentialsError](err)
			if !ok {
				t.Fatalf("expected credential error, got %v", err)
			}
			if failure.Kind != tt.kind || failure.Field != tt.field {
				t.Fatalf("error classification %s/%s, want %s/%s", failure.Kind, failure.Field, tt.kind, tt.field)
			}
			if strings.Contains(fmt.Sprintf("%v %#v", err, err), "agctl-test-secret") {
				t.Fatal("payload in error")
			}
		})
	}
}

func TestAccessExpiryBoundaries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := map[string]struct {
		exp     *int64
		refresh *time.Time
		expired bool
	}{"success: inside margin": {new(now.Unix() + 299), nil, true}, "success: at margin": {new(now.Unix() + 300), nil, true}, "success: outside margin": {new(now.Unix() + 301), nil, false}, "success: older fallback": {nil, new(now.Add(-LastRefreshInterval - time.Second)), true}, "success: at fallback": {nil, new(now.Add(-LastRefreshInterval)), false}, "success: younger fallback": {nil, new(now.Add(-LastRefreshInterval + time.Second)), false}, "success: unknown": {nil, nil, true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := credentialWithAccess(t, tt.exp, tt.refresh)
			if got := c.AccessExpired(now, AccessRefreshMargin); got != tt.expired {
				t.Fatalf("expired=%t, want %t", got, tt.expired)
			}
		})
	}
}

func TestCredentialIdentityAndHeaders(t *testing.T) {
	id := syntheticJWT(`{"email":"codex-user@example.invalid","https://api.openai.com/auth":{"chatgpt_user_id":"user-one","chatgpt_account_id":"acct-claim","chatgpt_plan_type":"mystery","chatgpt_account_is_fedramp":true}}`)
	tests := map[string]struct {
		account string
		drift   bool
		headers int
	}{"success: stored account wins": {"acct-file", true, 2}, "success: matching account": {"acct-claim", false, 2}, "success: unsafe header omitted": {"acct\r\nX-Injected: 1", true, 1}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": id, "access_token": "agctl-test-access", "account_id": tt.account}})
			if err != nil {
				t.Fatal(err)
			}
			c := mustCredentials(t, body)
			want := &Identity{UserID: "user-one", AccountID: tt.account, Email: new("codex-user@example.invalid"), Plan: new("unknown")}
			if diff := gocmp.Diff(want, c.Identity()); diff != "" {
				t.Fatal(diff)
			}
			if c.IdentityDrift() != tt.drift || len(c.ExtraHeaders()) != tt.headers {
				t.Fatalf("drift=%t headers=%v", c.IdentityDrift(), c.ExtraHeaders())
			}
		})
	}
}

func TestCredentialsDigestsAndRedaction(t *testing.T) {
	c := mustCredentials(t, codexFixture(t, "auth-unknown-members.json"))
	var body map[string]jsontext.Value
	if err := json.Unmarshal(credentialOutput(t, c), &body); err != nil {
		t.Fatal(err)
	}
	var tokenValues map[string]jsontext.Value
	if err := json.Unmarshal(body["tokens"], &tokenValues); err != nil {
		t.Fatal(err)
	}
	tokens := make(map[string]string)
	for _, name := range []string{"access_token", "refresh_token", "id_token", "account_id"} {
		if raw, ok := tokenValues[name]; ok {
			var token string
			if err := json.Unmarshal(raw, &token); err != nil {
				t.Fatal(err)
			}
			tokens[name] = token
		}
	}
	for name, token := range tokens {
		if name == "account_id" {
			continue
		}
		sum := sha256.Sum256([]byte(token))
		want := hex.EncodeToString(sum[:])
		switch name {
		case "access_token":
			if diff := gocmp.Diff(want, c.Digests().AccessSHA256); diff != "" {
				t.Fatal(diff)
			}
		case "refresh_token":
			if diff := gocmp.Diff(want, c.Digests().RefreshSHA256); diff != "" {
				t.Fatal(diff)
			}
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	slog.New(slog.NewTextHandler(&log, nil)).Info("credential", "value", c)
	for _, text := range []string{fmt.Sprintf("%v %#v %+v %s", c, c, c, c), string(encoded), log.String()} {
		for name, token := range tokens {
			if name != "account_id" && strings.Contains(text, token) {
				t.Fatalf("%s exposed by carrier", name)
			}
		}
		if strings.Contains(strings.ToLower(text), "bearer") {
			t.Fatal("authorization in carrier")
		}
	}
	if err := c.WithAuthorizationHeader(func(header string) error {
		if diff := gocmp.Diff("Bearer "+tokens["access_token"], header); diff != "" {
			t.Fatal(diff)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshResponse(t *testing.T) {
	tests := map[string]struct {
		body     string
		kind     string
		access   bool
		earliest *time.Time
	}{"success: absent": {"{}", "", false, nil}, "success: null": {`{"access_token":null}`, "", false, nil}, "success: access and hint": {`{"access_token":"agctl-test-access","earliest_refresh_at":1800000000}`, "", true, new(time.Unix(1800000000, 0).UTC())}, "success: fractional hint ignored": {`{"earliest_refresh_at":1.5}`, "", false, nil}, "error: object required": {"[]", "object", false, nil}, "error: token type": {`{"refresh_token":5}`, "type", false, nil}, "error: truncated": {`{"access_token":"agctl-test-`, "truncated", false, nil}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r, err := ParseRefreshResponse([]byte(tt.body))
			if tt.kind != "" {
				failure, ok := errors.AsType[*CredentialsError](err)
				if !ok || failure.Kind != tt.kind {
					t.Fatalf("classification %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.HasAccessToken() != tt.access {
				t.Fatal("access presence differs")
			}
			if diff := gocmp.Diff(tt.earliest, r.EarliestRefreshAt()); diff != "" {
				t.Fatal(diff)
			}
			if strings.Contains(fmt.Sprintf("%+v", r), "agctl-test-access") {
				t.Fatal("response token exposed")
			}
		})
	}
}
