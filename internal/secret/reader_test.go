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
	"fmt"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
)

func TestClassifyStderr(t *testing.T) {
	t.Parallel()

	// The substrings and their order match the vendor tooling this program
	// coexists with; several messages contain more than one substring, so
	// the order decides the class.
	tests := map[string]struct {
		stderr string
		want   StderrClass
	}{
		"success: empty":                        {stderr: "", want: StderrEmpty},
		"success: whitespace only":              {stderr: "   \n", want: StderrEmpty},
		"success: duplicate by code":            {stderr: "errSecDuplicateItem", want: StderrDuplicateItem},
		"success: duplicate by text":            {stderr: "The item already exists.", want: StderrDuplicateItem},
		"success: unavailable":                  {stderr: "security: unable to open /x", want: StderrKeychainUnavailable},
		"success: unavailable, other wording":   {stderr: "could not open the keychain", want: StderrKeychainUnavailable},
		"success: no keychain by code":          {stderr: "errSecNoDefaultKeychain", want: StderrNoKeychain},
		"success: no keychain by text":          {stderr: "A default keychain could not be found", want: StderrNoKeychain},
		"success: not found by code":            {stderr: "errSecItemNotFound", want: StderrItemNotFound},
		"success: not found by text":            {stderr: "The specified item could not be found in the keychain.", want: StderrItemNotFound},
		"success: interaction by code":          {stderr: "errSecInteractionNotAllowed", want: StderrInteractionNotAllowed},
		"success: interaction by text":          {stderr: "User interaction is not allowed.", want: StderrInteractionNotAllowed},
		"success: cancelled by code":            {stderr: "errSecUserCanceled", want: StderrUserCanceled},
		"success: cancelled by text":            {stderr: "the user pressed Cancel", want: StderrUserCanceled},
		"success: auth by code":                 {stderr: "errSecAuthFailed", want: StderrAuthFailed},
		"success: auth by text":                 {stderr: "The user name or passphrase you entered is not correct.", want: StderrAuthFailed},
		"success: locked":                       {stderr: "The user interaction... keychain is locked", want: StderrKeychainLocked},
		"success: unlock":                       {stderr: "please unlock the keychain first", want: StderrKeychainLocked},
		"success: anything else":                {stderr: "something entirely new", want: StderrOther},
		"success: case-insensitive code":        {stderr: "ERRSECITEMNOTFOUND", want: StderrItemNotFound},
		"success: case-insensitive text":        {stderr: "The Keychain Is LOCKED", want: StderrKeychainLocked},
		"success: two classes take the earlier": {stderr: "could not open: no keychain available", want: StderrKeychainUnavailable},
		"success: cancel precedes authorization": {
			stderr: "authorization cancelled by the user",
			want:   StderrUserCanceled,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyStderr(tt.stderr); got != tt.want {
				t.Errorf("ClassifyStderr(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestStderrClassKeychainClass(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		class StderrClass
		want  errs.KeychainClass
	}{
		"success: item not found gets the named class":    {class: StderrItemNotFound, want: errs.KeychainNotFound},
		"success: locked gets the named class":            {class: StderrKeychainLocked, want: errs.KeychainLocked},
		"success: unavailable gets the named class":       {class: StderrKeychainUnavailable, want: errs.KeychainUnavailable},
		"success: no keychain folds into unavailable":     {class: StderrNoKeychain, want: errs.KeychainUnavailable},
		"success: user canceled travels verbatim":         {class: StderrUserCanceled, want: errs.KeychainClass("UserCanceled")},
		"success: interaction refusal travels verbatim":   {class: StderrInteractionNotAllowed, want: errs.KeychainClass("InteractionNotAllowed")},
		"success: an unclassified message stays its own":  {class: StderrOther, want: errs.KeychainClass("Other")},
		"success: a duplicate item names itself verbatim": {class: StderrDuplicateItem, want: errs.KeychainClass("DuplicateItem")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tt.class.KeychainClass(); got != tt.want {
				t.Errorf("%v.KeychainClass() = %q, want %q", tt.class, got, tt.want)
			}
		})
	}
}

func TestKeychainErrorMatching(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err    error
		target *KeychainError
		want   bool
	}{
		"success: a locked failure matches its sentinel": {
			err:    &KeychainError{Class: errs.KeychainLocked, Detail: "exit status 36", Transient: true},
			target: ErrKeychainLocked,
			want:   true,
		},
		"success: a wrapped timeout still matches": {
			err:    fmt.Errorf("resolve the live row: %w", &KeychainError{Class: errs.KeychainTimeout, Detail: "security timed out after 2000 ms", Transient: true}),
			target: ErrKeychainTimeout,
			want:   true,
		},
		"success: not found matches regardless of detail": {
			err:    &KeychainError{Class: errs.KeychainNotFound, Detail: "SecKeychainSearchCopyNext"},
			target: ErrItemNotFound,
			want:   true,
		},
		"error: a timeout is not a lock": {
			err:    &KeychainError{Class: errs.KeychainTimeout, Transient: true},
			target: ErrKeychainLocked,
			want:   false,
		},
		"error: an unsupported platform is not unavailable": {
			err:    &KeychainError{Class: KeychainClassUnsupported},
			target: ErrKeychainUnavailable,
			want:   false,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := errors.Is(tt.err, tt.target); got != tt.want {
				t.Errorf("errors.Is(%v, %v) = %t, want %t", tt.err, tt.target, got, tt.want)
			}
		})
	}
}

func TestKeychainErrorMapsOntoThePartialExit(t *testing.T) {
	t.Parallel()

	// The read path's failures degrade a row; they never abort the run. The
	// mapping travels through Unwrap into the errs vocabulary, so main needs
	// no knowledge of this package.
	err := fmt.Errorf("status pass: %w", &KeychainError{Class: errs.KeychainLocked, Transient: true})

	var kerr *errs.KeychainError
	if !errors.As(err, &kerr) {
		t.Fatalf("errors.As found no errs.KeychainError in %v", err)
	}
	if kerr.Class != errs.KeychainLocked {
		t.Errorf("unwrapped class = %q, want %q", kerr.Class, errs.KeychainLocked)
	}
	if got := errs.ExitCode(err); got != errs.ExitPartial {
		t.Errorf("ExitCode(%v) = %d, want %d", err, got, errs.ExitPartial)
	}
}

func TestKeychainErrorMessageNamesTheClassAndDetail(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  *KeychainError
		want string
	}{
		"success: class only": {
			err:  &KeychainError{Class: errs.KeychainLocked, Transient: true},
			want: "keychain read failed: locked",
		},
		"success: class and detail": {
			err:  &KeychainError{Class: errs.KeychainTimeout, Detail: "security timed out after 2000 ms", Transient: true},
			want: "keychain read failed: timeout: security timed out after 2000 ms",
		},
		"success: a verbatim class": {
			err:  &KeychainError{Class: errs.KeychainClass("UserCanceled"), Detail: "the user pressed Cancel", Transient: true},
			want: "keychain read failed: UserCanceled: the user pressed Cancel",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.want, tt.err.Error()); diff != "" {
				t.Errorf("Error() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDisabledReaderAnswersNothingToEverything(t *testing.T) {
	t.Parallel()

	reader := DisabledReader{}
	ctx := t.Context()

	status := reader.Preflight(ctx)
	want := KeychainStatus{State: KeychainStateUnavailable, Reason: "disabled"}
	if diff := gocmp.Diff(want, status); diff != "" {
		t.Errorf("Preflight() mismatch (-want +got):\n%s", diff)
	}

	entries, err := reader.ListServices(ctx, "")
	if err != nil {
		t.Fatalf("ListServices() error = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Errorf("ListServices() = %v, want nothing", entries)
	}

	sec, err := reader.Read(ctx, "anything")
	if sec != nil {
		t.Errorf("Read() secret = %v, want nil", sec)
	}
	if !errors.Is(err, ErrItemNotFound) {
		t.Errorf("Read() error = %v, want %v", err, ErrItemNotFound)
	}
}
