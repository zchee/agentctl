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
	"errors"
	"fmt"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestClaimsAllowlist(t *testing.T) {
	tests := map[string]struct {
		payload string
		want    Claims
	}{"success: labels": {`{"email":"user@example.invalid","exp":42,"unknown":"agctl-test-secret","https://api.openai.com/auth":{"chatgpt_user_id":"user-one","chatgpt_account_id":"acct-one","chatgpt_plan_type":"pro","chatgpt_account_is_fedramp":true}}`, Claims{Email: new("user@example.invalid"), Exp: new(int64(42)), ChatGPTUserID: new("user-one"), ChatGPTAccountID: new("acct-one"), PlanType: new("pro"), IsFedramp: true}}, "success: missing": {`{}`, Claims{}}, "success: fallback": {`{"https://api.openai.com/profile":{"email":"profile@example.invalid"},"https://api.openai.com/auth":{"user_id":"fallback","chatgpt_account_is_fedramp":"yes"}}`, Claims{Email: new("profile@example.invalid"), ChatGPTUserID: new("fallback")}}, "success: wrong optional types": {`{"email":4,"exp":4.5,"https://api.openai.com/auth":{"chatgpt_user_id":4,"chatgpt_account_id":false}}`, Claims{}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			token := syntheticJWT(tt.payload)
			got, err := ParseClaims([]byte(token))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatal(diff)
			}
			if strings.Contains(fmt.Sprintf("%+v", got), "agctl-test-secret") {
				t.Fatal("unknown claim survived")
			}
			parts := strings.Split(token, ".")
			parts[1] += "=="
			if _, err := ParseClaims([]byte(strings.Join(parts, "."))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMalformedClaims(t *testing.T) {
	tests := map[string]struct {
		token string
		err   error
	}{"error: empty": {"", ErrJWTShape}, "error: two segments": {"header.payload", ErrJWTShape}, "error: four segments": {"header.payload.signature.extra", ErrJWTShape}, "error: empty payload": {"header..signature", ErrJWTShape}, "error: encoding": {"header.!!.signature", ErrJWTEncoding}, "error: array": {syntheticJWT(`[]`), ErrJWTJSON}, "error: large": {"header." + strings.Repeat("A", 300*1024) + ".signature", ErrJWTTooLarge}, "error: malformed object": {syntheticJWT(`{"secret":"agctl-test-secret",`), ErrJWTJSON}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseClaims([]byte(tt.token))
			if !errors.Is(err, tt.err) {
				t.Fatalf("error=%v want %v", err, tt.err)
			}
			if strings.Contains(fmt.Sprintf("%v %#v", err, err), "agctl-test-secret") {
				t.Fatal("error carries token")
			}
			if _, err := ParseExpiry([]byte(tt.token)); !errors.Is(err, tt.err) {
				t.Fatalf("expiry error=%v want %v", err, tt.err)
			}
		})
	}
}

func TestExpiryAllowlist(t *testing.T) {
	tests := map[string]struct {
		payload string
		want    *int64
	}{"success: integer": {`{"exp":42}`, new(int64(42))}, "success: absent": {`{}`, nil}, "success: fraction": {`{"exp":42.5}`, nil}, "success: huge": {`{"exp":9223372036854775808}`, nil}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseExpiry([]byte(syntheticJWT(tt.payload)))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
