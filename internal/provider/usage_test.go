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
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// probeSentinel is the token a plain-field credential behind the seam must
// still not reach a render with.
const probeSentinel = "agentctl-test-probe-token"

// unredactedAuth does exactly what the seam must survive: it holds a token
// in a plain exported field, so its own default rendering prints the token
// in full.
type unredactedAuth struct {
	Token string
}

func (a *unredactedAuth) AuthorizationHeader() string {
	return "Bearer " + a.Token
}

func (a *unredactedAuth) ExtraHeaders() []Header {
	return nil
}

func TestAnAccountRefRenderRedactsEvenACredentialThatDoesNot(t *testing.T) {
	t.Parallel()

	auth := &unredactedAuth{Token: probeSentinel}
	if rendered := fmt.Sprintf("%+v", auth); !strings.Contains(rendered, probeSentinel) {
		t.Fatalf("the probe must leak on its own, or it proves nothing about the seam: %q", rendered)
	}

	account := AccountRef{ID: "acct-uuid", Auth: auth}
	renders := map[string]string{
		"%v of the value":    fmt.Sprintf("%v", account),
		"%+v of the value":   fmt.Sprintf("%+v", account),
		"%#v of the value":   fmt.Sprintf("%#v", account),
		"%s of the value":    fmt.Sprintf("%s", account),
		"%q of the value":    fmt.Sprintf("%q", account),
		"%v of the pointer":  fmt.Sprintf("%v", &account),
		"%+v of the pointer": fmt.Sprintf("%+v", &account),
		"String()":           account.String(),
		"GoString()":         account.GoString(),
	}
	for name, rendered := range renders {
		if strings.Contains(rendered, probeSentinel) {
			t.Errorf("%s: the probe token leaked into: %q", name, rendered)
		}
		if strings.Contains(rendered, "Bearer") {
			t.Errorf("%s: a bearer string leaked into: %q", name, rendered)
		}
		if !strings.Contains(rendered, "acct-uuid") {
			t.Errorf("%s: the row id is not a secret and identifies it: %q", name, rendered)
		}
	}
}

func TestAnAccountRefLogValueCarriesItsIdentityOnly(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))
	logger.Info("fetching", "account", AccountRef{ID: "acct-uuid", Auth: &unredactedAuth{Token: probeSentinel}})

	line := buffer.String()
	if strings.Contains(line, probeSentinel) {
		t.Fatalf("the probe token leaked into the log line: %q", line)
	}
	if !strings.Contains(line, "acct-uuid") {
		t.Fatalf("the log line must identify the account: %q", line)
	}
}

func TestTheAuthorizationHeaderIsTheOnlyWayThroughTheSeam(t *testing.T) {
	t.Parallel()

	// The header itself is the one deliberate plaintext return per
	// provider, so it does carry the token — and it is the interface's
	// only route to one.
	var auth UsageAuth = &unredactedAuth{Token: probeSentinel}

	if diff := gocmp.Diff("Bearer "+probeSentinel, auth.AuthorizationHeader()); diff != "" {
		t.Fatalf("authorization header mismatch (-want +got):\n%s", diff)
	}
	if extra := auth.ExtraHeaders(); len(extra) != 0 {
		t.Fatalf("a credential with no extra headers must return none, got %v", extra)
	}
}
