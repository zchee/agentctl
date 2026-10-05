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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
)

func TestTrimTrailingNewline(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   []byte
		want []byte
	}{
		"success: a trailing LF is dropped":     {in: []byte("token\n"), want: []byte("token")},
		"success: a trailing CRLF is dropped":   {in: []byte("token\r\n"), want: []byte("token")},
		"success: no newline stays intact":      {in: []byte("token"), want: []byte("token")},
		"success: only one newline is dropped":  {in: []byte("token\n\n"), want: []byte("token\n")},
		"success: an interior newline survives": {in: []byte("a\nb\n"), want: []byte("a\nb")},
		"success: empty input stays empty":      {in: []byte{}, want: []byte{}},
		"success: a bare newline empties":       {in: []byte("\n"), want: []byte{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := trimTrailingNewline(tt.in)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("trimTrailingNewline(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

func TestRunOutputFailureClassification(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		out    runOutput
		target *KeychainError
		detail string
	}{
		"success: exit 36 is locked whatever stderr says": {
			out:    runOutput{code: exitLocked, stderr: "anything at all"},
			target: ErrKeychainLocked,
		},
		"success: an unopenable keychain is unavailable": {
			out:    runOutput{code: 1, stderr: "security: unable to open the keychain\n"},
			target: ErrKeychainUnavailable,
			detail: "security: unable to open the keychain",
		},
		"success: a not-found message carries the named class": {
			out:    runOutput{code: 1, stderr: "The specified item could not be found in the keychain."},
			target: ErrItemNotFound,
		},
		"success: a locked message without exit 36 still reads locked": {
			out:    runOutput{code: 1, stderr: "please unlock the keychain first"},
			target: ErrKeychainLocked,
		},
		"success: an unclassified message travels verbatim": {
			out:    runOutput{code: 1, stderr: "something entirely new"},
			target: &KeychainError{Class: errs.KeychainClass("Other")},
			detail: "something entirely new",
		},
		"success: a signal death classifies by stderr": {
			out:    runOutput{code: -1, stderr: ""},
			target: &KeychainError{Class: errs.KeychainClass("Empty")},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tt.out.failure()
			if !errors.Is(err, tt.target) {
				t.Fatalf("failure() = %v, want class %q", err, tt.target.Class)
			}
			if !err.Transient {
				t.Errorf("failure() = %v is not transient; every classified exit is", err)
			}
			if tt.detail != "" && err.Detail != tt.detail {
				t.Errorf("Detail = %q, want %q", err.Detail, tt.detail)
			}
		})
	}
}

func TestUnsupportedReaderRefusesEverything(t *testing.T) {
	t.Parallel()

	reader := unsupportedReader{}
	ctx := t.Context()

	if got := reader.Preflight(ctx); got.State != KeychainStateUnsupported {
		t.Errorf("Preflight() = %+v, want the unsupported state", got)
	}
	if _, err := reader.ListServices(ctx, "anything"); !errors.Is(err, ErrKeychainUnsupported) {
		t.Errorf("ListServices() error = %v, want %v", err, ErrKeychainUnsupported)
	}
	sec, err := reader.Read(ctx, "anything")
	if sec != nil || !errors.Is(err, ErrKeychainUnsupported) {
		t.Errorf("Read() = (%v, %v), want (nil, %v)", sec, err, ErrKeychainUnsupported)
	}
	var kerr *KeychainError
	if !errors.As(err, &kerr) || kerr.Transient {
		t.Errorf("Read() error = %#v; an absent platform transport will not fix itself", err)
	}
}

func TestCurrentAccountFallsBackThroughLogname(t *testing.T) {
	tests := map[string]struct {
		user    string
		logname string
		want    string
	}{
		"success: USER wins":                  {user: "alice", logname: "bob", want: "alice"},
		"success: LOGNAME is the fallback":    {user: "", logname: "bob", want: "bob"},
		"success: neither set is well-formed": {user: "", logname: "", want: ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("USER", tt.user)
			t.Setenv("LOGNAME", tt.logname)
			if got := CurrentAccount(); got != tt.want {
				t.Errorf("currentAccount() = %q, want %q", got, tt.want)
			}
		})
	}
}
