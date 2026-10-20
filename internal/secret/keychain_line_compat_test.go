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
	"errors"
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/errs"
)

// lineOfExactly builds, through the stdin-line constructor, an update whose
// retired stdin line would be exactly length bytes, trailing newline
// included. The document contributes two bytes per byte, so the parity of
// the length is fixed by the account name; one of the two account lengths
// below always lands on the requested number.
func lineOfExactly(t *testing.T, length int) (*KeychainStdinLine, error) {
	t.Helper()
	const fixed = 42
	for accountLen := 1; accountLen <= 2; accountLen++ {
		overhead := fixed + accountLen + len(updateService)
		if length < overhead || (length-overhead)%2 != 0 {
			continue
		}
		return NewKeychainStdinLine(strings.Repeat("u", accountLen), updateService, blobOfLen((length-overhead)/2))
	}
	t.Fatalf("no account length makes a line of exactly %d bytes", length)
	return nil, nil
}

func TestTheStdinLineConstructorKeepsTheStdinLimit(t *testing.T) {
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
			update, err := lineOfExactly(t, tt.length)
			if !tt.refused {
				if err != nil {
					t.Fatalf("a line of %d bytes is allowed: %v", tt.length, err)
				}
				if got := 42 + len(update.Account()) + len(update.Service()) + update.Len(); got != tt.length {
					t.Fatalf("the line would be %d bytes, want %d", got, tt.length)
				}
				return
			}
			tooLong, ok := errors.AsType[*LineTooLongError](err)
			if !ok || tooLong.Len != tt.length {
				t.Fatalf("want the over-long refusal naming %d, got %v", tt.length, err)
			}
			if code := errs.ExitCode(err); code != 12 {
				t.Fatalf("ExitCode = %d, want 12", code)
			}
		})
	}
}

func TestTheStdinLineConstructorValidatesLikeTheUpdate(t *testing.T) {
	t.Parallel()

	_, err := NewKeychainStdinLine("-example", updateService, blobOfLen(8))
	if unquotable, ok := errors.AsType[*UnquotableError](err); !ok || unquotable.Field != "account" {
		t.Fatalf("want the account refusal, got %v", err)
	}
}
