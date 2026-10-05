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
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/errs"
)

// liveService is the live keychain item's service name the line vectors
// target.
const liveService = LiveService

// lineBytes captures the bytes a line puts on a pipe.
func lineBytes(t *testing.T, line *KeychainStdinLine) []byte {
	t.Helper()
	var sink bytes.Buffer
	if _, err := line.WriteTo(&sink); err != nil {
		t.Fatalf("a buffer accepts a write: %v", err)
	}
	return sink.Bytes()
}

// credentialsOfBlobLen builds a credential whose serialized blob is
// exactly blobLen bytes, with the padding in an unrecognised member so the
// length is reachable exactly rather than approximately.
func credentialsOfBlobLen(t *testing.T, blobLen int) *Credentials {
	t.Helper()
	minimal := `{"claudeAiOauth":{"accessToken":"a","expiresAt":0,"scopes":[],"pad":""}}`
	credentials, err := ParseBlob([]byte(minimal))
	if err != nil {
		t.Fatalf("the minimal blob parses: %v", err)
	}
	base := len(mustBlobJSON(t, credentials))
	padding := blobLen - base
	if padding < 0 {
		t.Fatalf("the requested blob of %d bytes is under the %d-byte base", blobLen, base)
	}
	for i := range credentials.Extra {
		if credentials.Extra[i].Name == "pad" {
			credentials.Extra[i].Value = jsontext.Value(`"` + strings.Repeat("p", padding) + `"`)
		}
	}
	if got := len(mustBlobJSON(t, credentials)); got != blobLen {
		t.Fatalf("the padded blob is %d bytes, want %d", got, blobLen)
	}
	return credentials
}

// lineOfExactly builds a line of exactly length bytes, trailing newline
// included. The blob contributes two bytes per byte, so the parity of the
// length is fixed by the account and service names; one of the two account
// lengths below always lands on the requested number.
func lineOfExactly(t *testing.T, length int) (*KeychainStdinLine, error) {
	t.Helper()
	// 28 bytes through the opening quote, 6 for `" -s "`, 6 for `" -X "`,
	// 1 for the closing quote and 1 for the newline.
	const fixed = 42
	for accountLen := 1; accountLen <= 2; accountLen++ {
		overhead := fixed + accountLen + len(liveService)
		if length < overhead || (length-overhead)%2 != 0 {
			continue
		}
		credentials := credentialsOfBlobLen(t, (length-overhead)/2)
		return credentials.ToKeychainStdinLine(strings.Repeat("u", accountLen), liveService)
	}
	t.Fatalf("no account length makes a line of exactly %d bytes", length)
	return nil, nil
}

func TestTheKeychainLineIsTheUpdateShape(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	line, err := credentials.ToKeychainStdinLine("example", liveService)
	if err != nil {
		t.Fatalf("the fixture blob is well under the limit: %v", err)
	}

	expected := fmt.Sprintf("add-generic-password -U -a %q -s %q -X %q\n",
		"example", liveService, hex.EncodeToString(mustBlobJSON(t, credentials)))
	if got := string(lineBytes(t, line)); got != expected {
		t.Fatalf("line mismatch:\ngot  %q\nwant %q", got, expected)
	}
	if line.Len() != len(expected) {
		t.Fatalf("Len() = %d, want %d", line.Len(), len(expected))
	}
	if line.Account() != "example" || line.Service() != liveService {
		t.Fatalf("the line must record its item: %s", line)
	}
}

func TestTheLinesLengthIsFixedOverheadPlusTwiceTheBlob(t *testing.T) {
	t.Parallel()

	credentials := credentialsOfBlobLen(t, 100)
	line, err := credentials.ToKeychainStdinLine("u", liveService)
	if err != nil {
		t.Fatalf("well under the limit: %v", err)
	}
	if want := 42 + 1 + len(liveService) + 200; line.Len() != want {
		t.Fatalf("Len() = %d, want %d", line.Len(), want)
	}
}

func TestTheLimitCountsTheTrailingNewline(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		length  int
		refused bool
	}{
		"success: one byte under the limit": {length: 4031},
		"success: exactly at the limit":     {length: KeychainLineLimit},
		"error: one byte over the limit":    {length: 4033, refused: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			line, err := lineOfExactly(t, tt.length)
			if !tt.refused {
				if err != nil {
					t.Fatalf("a line of %d bytes is allowed: %v", tt.length, err)
				}
				if line.Len() != tt.length {
					t.Fatalf("Len() = %d, want %d", line.Len(), tt.length)
				}
				return
			}
			tooLong, ok := errors.AsType[*LineTooLongError](err)
			if !ok {
				t.Fatalf("want the over-long refusal, got %v", err)
			}
			if tooLong.Len != tt.length {
				t.Fatalf("the refusal must name the length %d, got %d", tt.length, tooLong.Len)
			}
			// The refusal is the recorded cause scripts branch on: there
			// is no argv fallback, so the only outcome is the dedicated
			// exit status.
			if code := errs.ExitCode(err); code != 12 {
				t.Fatalf("ExitCode = %d, want 12", code)
			}
			if !strings.Contains(err.Error(), "4032-byte `security -i` limit") {
				t.Fatalf("the refusal must name the limit: %v", err)
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
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := credentials.ToKeychainStdinLine(tt.account, tt.service)
			unquotable, ok := errors.AsType[*UnquotableError](err)
			if !ok || unquotable.Field != tt.field {
				t.Fatalf("want the unquotable %s refusal, got %v", tt.field, err)
			}
		})
	}
}

func TestTheLineNeverPrintsItsPayload(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	line, err := credentials.ToKeychainStdinLine("example", liveService)
	if err != nil {
		t.Fatalf("well under the limit: %v", err)
	}
	payload := hex.EncodeToString(mustBlobJSON(t, credentials))

	renders := map[string]string{
		"String()": line.String(),
		"%v":       fmt.Sprintf("%v", line),
		"%+v":      fmt.Sprintf("%+v", line),
		"%#v":      fmt.Sprintf("%#v", line),
		"%s":       fmt.Sprintf("%s", line),
	}
	for name, rendered := range renders {
		if strings.Contains(rendered, "sk-ant-") {
			t.Errorf("%s leaked a token: %q", name, rendered)
		}
		if strings.Contains(rendered, payload) {
			t.Errorf("%s leaked the payload: %q", name, rendered)
		}
		if !strings.Contains(rendered, "<redacted>") {
			t.Errorf("%s must mark the redaction: %q", name, rendered)
		}
		if !strings.Contains(rendered, liveService) {
			t.Errorf("%s: the item the line names is not a secret: %q", name, rendered)
		}
	}
}
