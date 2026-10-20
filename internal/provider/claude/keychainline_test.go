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
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/secret"
)

// liveService is the live keychain item's service name the update vectors
// target.
const liveService = LiveService

// keychainCanary marks a carried member's value, so a render that leaks a
// carried value is caught by name.
const keychainCanary = "SIBLING-CANARY-9f31"

// credentialsOfDocumentLen builds a credential whose keychain document is
// exactly documentLen bytes, with the padding in a carried member, so the
// ceiling is measured against the whole document and not the account
// object alone.
func credentialsOfDocumentLen(t *testing.T, documentLen int) *Credentials {
	t.Helper()
	minimal := `{"claudeAiOauth":{"accessToken":"a","expiresAt":0,"scopes":[]},"pluginSecrets":""}`
	credentials, err := ParseBlob([]byte(minimal))
	if err != nil {
		t.Fatalf("the minimal blob parses: %v", err)
	}
	base := len(mustKeychainBlobJSON(t, credentials))
	padding := documentLen - base
	if padding < 0 {
		t.Fatalf("the requested document of %d bytes is under the %d-byte base", documentLen, base)
	}
	credentials.Siblings[0].Value = jsontext.Value(`"` + strings.Repeat("p", padding) + `"`)
	if got := len(mustKeychainBlobJSON(t, credentials)); got != documentLen {
		t.Fatalf("the padded document is %d bytes, want %d", got, documentLen)
	}
	return credentials
}

func TestTheKeychainUpdateCarriesTheWholeDocument(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	credentials.Siblings = []ExtraMember{{Name: "mcpOAuth", Value: jsontext.Value(`{"server":{"accessToken":"` + keychainCanary + `"}}`)}}
	update, err := credentials.ToKeychainUpdate("example", liveService)
	if err != nil {
		t.Fatalf("the fixture document is well under the ceiling: %v", err)
	}

	document := mustKeychainBlobJSON(t, credentials)
	if want := len(hex.EncodeToString(document)); update.Len() != want {
		t.Fatalf("Len() = %d, want %d hex digits: the update must carry the account object and the carried member", update.Len(), want)
	}
	if update.Len() <= len(hex.EncodeToString(mustBlobJSON(t, credentials))) {
		t.Fatalf("Len() = %d is not longer than the sibling-free document's hex", update.Len())
	}
	if update.Account() != "example" || update.Service() != liveService {
		t.Fatalf("the update must record its item: %s", update)
	}
}

func TestTheCeilingCountsTheCarriedMembers(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		length  int
		refused bool
	}{
		"success: one byte under the ceiling": {length: secret.KeychainBlobLimit - 1},
		"success: exactly at the ceiling":     {length: secret.KeychainBlobLimit},
		"error: one byte over the ceiling":    {length: secret.KeychainBlobLimit + 1, refused: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			credentials := credentialsOfDocumentLen(t, tt.length)
			if account := len(mustBlobJSON(t, credentials)); account >= secret.KeychainBlobLimit {
				t.Fatalf("the account object alone is %d bytes; the case must be decided by the carried member", account)
			}
			update, err := credentials.ToKeychainUpdate("u", liveService)
			if !tt.refused {
				if err != nil {
					t.Fatalf("a document of %d bytes is allowed: %v", tt.length, err)
				}
				if update.Len() != 2*tt.length {
					t.Fatalf("Len() = %d, want %d", update.Len(), 2*tt.length)
				}
				return
			}
			tooLarge, ok := errors.AsType[*secret.BlobTooLargeError](err)
			if !ok {
				t.Fatalf("want the too-large refusal, got %v", err)
			}
			if tooLarge.Len != tt.length {
				t.Fatalf("the refusal must name the length %d, got %d", tt.length, tooLarge.Len)
			}
			if !errors.Is(err, secret.ErrCredentialTooLarge) {
				t.Fatalf("the refusal must match ErrCredentialTooLarge: %v", err)
			}
			if code := errs.ExitCode(err); code != 31 {
				t.Fatalf("ExitCode = %d, want 31", code)
			}
		})
	}
}

func TestAnUnquotableNameIsRefusedBeforeAnythingIsBuilt(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	tests := map[string]struct {
		account string
		service string
		field   string
	}{
		"error: a quote in the account": {
			account: `ex"ample`, service: liveService, field: "account",
		},
		"error: a backslash in the service": {
			account: "example", service: `live\service`, field: "service",
		},
		"error: a newline in the account": {
			account: "exam\nple", service: liveService, field: "account",
		},
		"error: a space in the account": {
			account: "exam ple", service: liveService, field: "account",
		},
		"error: a service read as an option": {
			account: "example", service: "-s", field: "service",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := credentials.ToKeychainUpdate(tt.account, tt.service)
			unquotable, ok := errors.AsType[*secret.UnquotableError](err)
			if !ok || unquotable.Field != tt.field {
				t.Fatalf("want the unquotable %s refusal, got %v", tt.field, err)
			}
		})
	}
}

func TestTheUpdateNeverPrintsItsPayload(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	credentials.Siblings = []ExtraMember{{Name: "mcpOAuth", Value: jsontext.Value(`{"server":{"accessToken":"` + keychainCanary + `"}}`)}}
	update, err := credentials.ToKeychainUpdate("example", liveService)
	if err != nil {
		t.Fatalf("well under the ceiling: %v", err)
	}
	payload := hex.EncodeToString(mustKeychainBlobJSON(t, credentials))
	canaryHex := hex.EncodeToString([]byte(keychainCanary))

	renders := map[string]string{
		"String()": update.String(),
		"%v":       fmt.Sprintf("%v", update),
		"%+v":      fmt.Sprintf("%+v", update),
		"%#v":      fmt.Sprintf("%#v", update),
		"%s":       fmt.Sprintf("%s", update),
	}
	for name, rendered := range renders {
		if strings.Contains(rendered, "sk-ant-") || strings.Contains(rendered, keychainCanary) {
			t.Errorf("%s leaked a token or a carried value", name)
		}
		if strings.Contains(rendered, payload) || strings.Contains(rendered, canaryHex) {
			t.Errorf("%s leaked the payload", name)
		}
		if !strings.Contains(rendered, "<redacted>") {
			t.Errorf("%s must mark the redaction: %q", name, rendered)
		}
		if !strings.Contains(rendered, liveService) {
			t.Errorf("%s: the item the update names is not a secret: %q", name, rendered)
		}
	}
}
