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

package secret

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/errs"
)

// updateService is the service name the update vectors target. It carries
// the space the live item's service carries.
const updateService = "Claude Code-credentials"

// blobOfLen builds a synthetic document of exactly blobLen bytes. The
// content is arbitrary because the update carries its lowercase hex.
func blobOfLen(blobLen int) []byte {
	return bytes.Repeat([]byte{'p'}, blobLen)
}

// updatePayload opens the sealed hex of update, for the tests that check
// the encoding.
func updatePayload(t *testing.T, update *KeychainUpdate) string {
	t.Helper()
	var payload string
	if err := update.hex.WithPlaintext(func(b []byte) error {
		payload = string(b)
		return nil
	}); err != nil {
		t.Fatalf("open the update's payload: %v", err)
	}
	return payload
}

func TestKeychainUpdateCarriesTheDocumentAsLowercaseHex(t *testing.T) {
	t.Parallel()

	blob := []byte(`{"claudeAiOauth":{"accessToken":"synthetic-access"},"mcpOAuth":{"server":{"accessToken":"synthetic-mcp"}}}`)
	original := bytes.Clone(blob)
	update, err := NewKeychainUpdate("example", updateService, blob)
	if err != nil {
		t.Fatalf("the document is well under the ceiling: %v", err)
	}

	if diff := gocmp.Diff(hex.EncodeToString(original), updatePayload(t, update)); diff != "" {
		t.Errorf("payload mismatch (-want +got):\n%s", diff)
	}
	if got, want := update.Len(), 2*len(original); got != want {
		t.Errorf("Len() = %d, want %d: two hex digits per document byte", got, want)
	}
	if update.Account() != "example" || update.Service() != updateService {
		t.Errorf("the update must record its item: %s", update)
	}
	// The caller owns the plaintext document; the constructor reads it and
	// leaves it as it was.
	if !bytes.Equal(blob, original) {
		t.Errorf("the constructor changed the caller's document")
	}
}

func TestKeychainUpdateCeiling(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		blobLen int
		refused bool
	}{
		"success: one byte under the ceiling":  {blobLen: KeychainBlobLimit - 1},
		"success: exactly at the ceiling":      {blobLen: 262144},
		"error: one byte over the ceiling":     {blobLen: 262145, refused: true},
		"error: far over the ceiling (1 MiB)":  {blobLen: 1 << 20, refused: true},
		"success: the measured 11.5 KB item":   {blobLen: 11776},
		"success: a single byte is a document": {blobLen: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			update, err := NewKeychainUpdate("example", updateService, blobOfLen(tt.blobLen))
			if !tt.refused {
				if err != nil {
					t.Fatalf("a %d-byte document is allowed: %v", tt.blobLen, err)
				}
				if update.Len() != 2*tt.blobLen {
					t.Fatalf("Len() = %d, want %d", update.Len(), 2*tt.blobLen)
				}
				return
			}
			tooLarge, ok := errors.AsType[*BlobTooLargeError](err)
			if !ok {
				t.Fatalf("want the over-ceiling refusal, got %v", err)
			}
			if tooLarge.Len != tt.blobLen {
				t.Errorf("the refusal must name the length %d, got %d", tt.blobLen, tooLarge.Len)
			}
			if !errors.Is(err, ErrCredentialTooLarge) {
				t.Errorf("the refusal must match ErrCredentialTooLarge: %v", err)
			}
			for _, other := range []error{ErrKeychainNotStarted, ErrKeychainOutcomeUnknown, ErrKeychainWriteFailed} {
				if errors.Is(err, other) {
					t.Errorf("a constructor refusal is not a write failure, but matches %v", other)
				}
			}
			refused, ok := errors.AsType[*errs.RefusedError](err)
			if !ok || refused.Reason != errs.ReasonCredentialTooLarge || refused.Letter != "" {
				t.Errorf("want the unlettered %q refusal, got %#v", errs.ReasonCredentialTooLarge, refused)
			}
			if code := errs.ExitCode(err); code != cli.SwapExitCredentialTooLarge {
				t.Errorf("ExitCode = %d, want %d", code, cli.SwapExitCredentialTooLarge)
			}
			if !strings.Contains(err.Error(), "262144-byte keychain update ceiling") {
				t.Errorf("the refusal must name the ceiling: %v", err)
			}
		})
	}
}

func TestCredentialTooLargeExitMatchesTheCLI(t *testing.T) {
	t.Parallel()

	if credentialTooLargeExit != cli.SwapExitCredentialTooLarge {
		t.Fatalf("credentialTooLargeExit = %d, cli.SwapExitCredentialTooLarge = %d", credentialTooLargeExit, cli.SwapExitCredentialTooLarge)
	}
}

func TestKeychainUpdateValidatesTheNamesFirst(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		account string
		service string
		field   string
		// blobLen is the document's length; zero means a small one.
		blobLen int
	}{
		"error: empty account":                          {account: "", service: updateService, field: "account"},
		"error: empty service":                          {account: "example", service: "", field: "service"},
		"error: a control character in the account":     {account: "exam\nple", service: updateService, field: "account"},
		"error: a control character in the service":     {account: "example", service: "live\x1bservice", field: "service"},
		"error: a quote in the account":                 {account: `ex"ample`, service: updateService, field: "account"},
		"error: a quote in the service":                 {account: "example", service: `live"service`, field: "service"},
		"error: a backslash in the account":             {account: `ex\ample`, service: updateService, field: "account"},
		"error: a backslash in the service":             {account: "example", service: `live\service`, field: "service"},
		"error: a leading dash in the account":          {account: "-example", service: updateService, field: "account"},
		"error: a leading dash in the service":          {account: "example", service: "-s", field: "service"},
		"error: a space in the account":                 {account: "ex ample", service: updateService, field: "account"},
		"error: a tab in the account":                   {account: "ex\tample", service: updateService, field: "account"},
		"error: a non-breaking space in the account":    {account: "ex ample", service: updateService, field: "account"},
		"error: the account is checked before the size": {account: "-x", service: updateService, field: "account", blobLen: KeychainBlobLimit + 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			update, err := NewKeychainUpdate(tt.account, tt.service, blobOfLen(max(tt.blobLen, 8)))
			unquotable, ok := errors.AsType[*UnquotableError](err)
			if !ok || unquotable.Field != tt.field {
				t.Fatalf("want the %s refusal, got %v", tt.field, err)
			}
			if update != nil {
				t.Fatalf("a refused name still built an update: %s", update)
			}
		})
	}
}

func TestKeychainUpdateAcceptsNamesSecurityTakesAsOneArgument(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		account string
		service string
	}{
		"success: the live service, with its space":    {account: "example", service: "Claude Code-credentials"},
		"success: a namespace service":                 {account: "example", service: "Claude Code-credentials-0123abcd"},
		"success: a dash inside the account":           {account: "first-last", service: updateService},
		"success: a dot and an underscore in the name": {account: "first.last_2", service: updateService},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			update, err := NewKeychainUpdate(tt.account, tt.service, blobOfLen(8))
			if err != nil {
				t.Fatalf("NewKeychainUpdate(%q, %q) = %v", tt.account, tt.service, err)
			}
			if update.Account() != tt.account || update.Service() != tt.service {
				t.Fatalf("the update names %q/%q, want %q/%q", update.Account(), update.Service(), tt.account, tt.service)
			}
		})
	}
}

func TestKeychainUpdateRefusesAnEmptyDocument(t *testing.T) {
	t.Parallel()

	if _, err := NewKeychainUpdate("example", updateService, nil); err == nil {
		t.Fatal("an empty document built an update")
	}
}

func TestKeychainUpdateNeverPrintsItsPayload(t *testing.T) {
	t.Parallel()

	blob := []byte(`{"claudeAiOauth":{"accessToken":"synthetic-update-marker"}}`)
	payload := hex.EncodeToString(blob)
	update, err := NewKeychainUpdate("example", updateService, blob)
	if err != nil {
		t.Fatalf("well under the ceiling: %v", err)
	}

	renders := map[string]string{
		"String()":    update.String(),
		"GoString()":  update.GoString(),
		"%v":          fmt.Sprintf("%v", update),
		"%+v":         fmt.Sprintf("%+v", update),
		"%#v":         fmt.Sprintf("%#v", update),
		"%s":          fmt.Sprintf("%s", update),
		"value %v":    fmt.Sprintf("%v", *update),
		"value %#v":   fmt.Sprintf("%#v", *update),
		"LogValue()":  update.LogValue().String(),
		"nested %+v":  fmt.Sprintf("%+v", struct{ U *KeychainUpdate }{update}),
		"nested val":  fmt.Sprintf("%+v", struct{ U KeychainUpdate }{*update}),
		"length only": fmt.Sprint(update.Len()),
	}
	for name, rendered := range renders {
		if strings.Contains(rendered, "synthetic-update-marker") {
			t.Errorf("%s leaked the document: %q", name, rendered)
		}
		if strings.Contains(rendered, payload) {
			t.Errorf("%s leaked the payload: %q", name, rendered)
		}
		if name == "length only" {
			continue
		}
		if !strings.Contains(rendered, "<redacted>") {
			t.Errorf("%s must mark the redaction: %q", name, rendered)
		}
		if !strings.Contains(rendered, updateService) {
			t.Errorf("%s: the item the update names is not a secret: %q", name, rendered)
		}
	}
}
