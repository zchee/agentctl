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
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestPKCE(t *testing.T) {
	if diff := gocmp.Diff("E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", CodeChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")); diff != "" {
		t.Fatalf("RFC challenge (-want +got): %s", diff)
	}
	seen := make(map[string]bool)
	for range 32 {
		p, err := NewPKCE()
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Challenge) != 43 || len(p.State) != 43 || p.verifier.Len() != 43 {
			t.Fatal("wrong nonce length")
		}
		if _, err := base64.RawURLEncoding.DecodeString(p.State); err != nil {
			t.Fatal(err)
		}
		if seen[p.State] || seen[p.Challenge] || p.State == p.Challenge {
			t.Fatal("nonces reused")
		}
		seen[p.State], seen[p.Challenge] = true, true
		if err := p.verifier.WithPlaintext(func(verifier []byte) error {
			if diff := gocmp.Diff(p.Challenge, CodeChallenge(string(verifier))); diff != "" {
				t.Errorf("challenge mismatch: %s", diff)
			}
			var logs bytes.Buffer
			slog.New(slog.NewTextHandler(&logs, nil)).Info("pkce", "value", p)
			encoded, err := json.Marshal(p)
			if err != nil {
				return err
			}
			for _, text := range []string{fmt.Sprintf("%v %#v %+v", p, p, p), logs.String(), string(encoded)} {
				if strings.Contains(text, string(verifier)) {
					t.Error("verifier escaped redaction")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuthorizeURL(t *testing.T) {
	p := &PKCE{Challenge: "challenge", State: "state"}
	got, err := AuthorizeURL("https://example.test/authorize?existing=1", p, Redirect{Port: 1234}, []string{"user:profile", "user:inference"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://example.test/authorize?existing=1&code=true&client_id=" + ClientID + "&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A1234%2Fcallback&scope=user%3Aprofile+user%3Ainference&code_challenge=challenge&code_challenge_method=S256&state=state"
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("authorize URL (-want +got): %s", diff)
	}
	if diff := gocmp.Diff(ManualRedirectURI, (Redirect{}).URI()); diff != "" {
		t.Fatal(diff)
	}
}

func TestManualCodeAndState(t *testing.T) {
	tests := map[string]struct {
		input, code, state string
		fail               bool
	}{
		"success: trimmed":         {"  code # state \n", "code", "state", false},
		"success: first separator": {"code#state#tail", "code", "state#tail", false},
		"error: separator absent":  {input: "code", fail: true},
		"error: code absent":       {input: "#state", fail: true},
		"error: state absent":      {input: "code#", fail: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			code, state, err := ParseManualCode(test.input)
			if (err != nil) != test.fail {
				t.Fatalf("ParseManualCode error = %v", err)
			}
			if diff := gocmp.Diff([]string{test.code, test.state}, []string{code, state}); diff != "" {
				t.Fatal(diff)
			}
		})
	}
	if VerifyState("state", "state") != nil || VerifyState("state", "other") == nil || VerifyState("state", "") == nil {
		t.Fatal("state comparison is incorrect")
	}
}

func TestRequestedScopes(t *testing.T) {
	tests := map[string]struct {
		env  string
		want []string
	}{
		"success: defaults":            {"", DefaultScopes},
		"success: whitespace defaults": {" \t\n", DefaultScopes},
		"success: override":            {" user:profile\tuser:inference ", []string{"user:profile", "user:inference"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTCTL_CLAUDE_OAUTH_SCOPES", test.env)
			if diff := gocmp.Diff(test.want, RequestedScopes()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
