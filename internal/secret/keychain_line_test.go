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

	"github.com/zchee/agentctl/internal/errs"
)

// lineService is the service name the line vectors target. Any quotable
// name works; the shape and length rules do not read it.
const lineService = "example-service"

// keychainLineBytes captures the bytes a line puts on a pipe.
func keychainLineBytes(t *testing.T, line *KeychainStdinLine) []byte {
	t.Helper()
	var sink bytes.Buffer
	if _, err := line.WriteTo(&sink); err != nil {
		t.Fatalf("a buffer accepts a write: %v", err)
	}
	return sink.Bytes()
}

// blobOfLen builds a payload of exactly blobLen bytes. The content is
// arbitrary because the line carries its lowercase hex, never the bytes.
func blobOfLen(blobLen int) []byte {
	return bytes.Repeat([]byte{'p'}, blobLen)
}

// keychainLineOfExactly builds a line of exactly length bytes, trailing
// newline included. The blob contributes two bytes per byte, so the parity
// of the length is fixed by the account and service names; one of the two
// account lengths below always lands on the requested number.
func keychainLineOfExactly(t *testing.T, length int) (*KeychainStdinLine, error) {
	t.Helper()
	// 28 bytes through the opening quote, 6 for `" -s "`, 6 for `" -X "`,
	// 1 for the closing quote and 1 for the newline.
	const fixed = 42
	for accountLen := 1; accountLen <= 2; accountLen++ {
		overhead := fixed + accountLen + len(lineService)
		if length < overhead || (length-overhead)%2 != 0 {
			continue
		}
		return NewKeychainStdinLine(strings.Repeat("u", accountLen), lineService, blobOfLen((length-overhead)/2))
	}
	t.Fatalf("no account length makes a line of exactly %d bytes", length)
	return nil, nil
}

func TestTheKeychainLineIsTheUpdateShape(t *testing.T) {
	t.Parallel()

	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-abc"}}`)
	line, err := NewKeychainStdinLine("example", lineService, blob)
	if err != nil {
		t.Fatalf("the blob is well under the limit: %v", err)
	}

	expected := fmt.Sprintf("add-generic-password -U -a %q -s %q -X %q\n",
		"example", lineService, hex.EncodeToString(blob))
	if got := string(keychainLineBytes(t, line)); got != expected {
		t.Fatalf("line mismatch:\ngot  %q\nwant %q", got, expected)
	}
	if line.Len() != len(expected) {
		t.Fatalf("Len() = %d, want %d", line.Len(), len(expected))
	}
	if line.Account() != "example" || line.Service() != lineService {
		t.Fatalf("the line must record its item: %s", line)
	}
}

func TestTheLinesLengthIsFixedOverheadPlusTwiceTheBlob(t *testing.T) {
	t.Parallel()

	line, err := NewKeychainStdinLine("u", lineService, blobOfLen(100))
	if err != nil {
		t.Fatalf("well under the limit: %v", err)
	}
	if want := 42 + 1 + len(lineService) + 200; line.Len() != want {
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
			line, err := keychainLineOfExactly(t, tt.length)
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

	tests := map[string]struct {
		account string
		service string
		field   string
	}{
		"error: a quote in the account": {
			account: `ex"ample`, service: lineService, field: "account",
		},
		"error: a backslash in the service": {
			account: "example", service: `live\service`, field: "service",
		},
		"error: a newline in the account": {
			account: "exam\nple", service: lineService, field: "account",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := NewKeychainStdinLine(tt.account, tt.service, blobOfLen(8))
			unquotable, ok := errors.AsType[*UnquotableError](err)
			if !ok || unquotable.Field != tt.field {
				t.Fatalf("want the unquotable %s refusal, got %v", tt.field, err)
			}
		})
	}
}

func TestTheLineNeverPrintsItsPayload(t *testing.T) {
	t.Parallel()

	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-abc"}}`)
	payload := hex.EncodeToString(blob)
	line, err := NewKeychainStdinLine("example", lineService, blob)
	if err != nil {
		t.Fatalf("well under the limit: %v", err)
	}

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
		if !strings.Contains(rendered, lineService) {
			t.Errorf("%s: the item the line names is not a secret: %q", name, rendered)
		}
	}
}
