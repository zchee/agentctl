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
	"fmt"
	"io/fs"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
)

func TestLocationConstructors(t *testing.T) {
	t.Parallel()

	item := KeychainItem("Claude Code-credentials")
	wantItem := Location{Kind: LocationKeychain, Service: "Claude Code-credentials"}
	if diff := gocmp.Diff(wantItem, item); diff != "" {
		t.Errorf("KeychainItem() mismatch (-want +got):\n%s", diff)
	}

	file := CredentialFile("/store/acct/org/.credentials.json")
	wantFile := Location{Kind: LocationFile, Path: "/store/acct/org/.credentials.json"}
	if diff := gocmp.Diff(wantFile, file); diff != "" {
		t.Errorf("CredentialFile() mismatch (-want +got):\n%s", diff)
	}
}

func TestLocationClassify(t *testing.T) {
	t.Parallel()

	keychain := KeychainItem("Claude Code-credentials")
	file := CredentialFile("/store/.credentials.json")

	tests := map[string]struct {
		loc  Location
		err  error
		want Outcome
	}{
		"success: a keychain read that succeeded is the credential": {
			loc:  keychain,
			err:  nil,
			want: Outcome{Kind: OutcomeCredential},
		},
		"success: a missing keychain item is absent, not an error": {
			loc:  keychain,
			err:  &KeychainError{Class: errs.KeychainNotFound, Detail: "SecKeychainSearchCopyNext"},
			want: Outcome{Kind: OutcomeAbsent},
		},
		"success: a locked keychain asks for an unlock, not a retry": {
			loc:  keychain,
			err:  &KeychainError{Class: errs.KeychainLocked, Transient: true},
			want: Outcome{Kind: OutcomeLocked},
		},
		"success: a keychain timeout is transient and says how long it waited": {
			loc:  keychain,
			err:  &KeychainError{Class: errs.KeychainTimeout, Detail: "security timed out after 2000 ms", Transient: true},
			want: Outcome{Kind: OutcomeTransient, Reason: "keychain read failed: timeout: security timed out after 2000 ms"},
		},
		"success: an unreachable keychain is transient": {
			loc:  keychain,
			err:  &KeychainError{Class: errs.KeychainUnavailable, Detail: "unable to open the keychain", Transient: true},
			want: Outcome{Kind: OutcomeTransient, Reason: "keychain read failed: unavailable: unable to open the keychain"},
		},
		"success: a spawn failure is transient for the row, never the file": {
			loc:  keychain,
			err:  &KeychainError{Class: errs.KeychainUnavailable, Detail: "/usr/bin/security: no such file"},
			want: Outcome{Kind: OutcomeTransient, Reason: "keychain read failed: unavailable: /usr/bin/security: no such file"},
		},
		"success: an unsupported platform is transient for the row": {
			loc:  keychain,
			err:  &KeychainError{Class: KeychainClassUnsupported},
			want: Outcome{Kind: OutcomeTransient, Reason: "keychain read failed: unsupported on this platform"},
		},
		"success: a verbatim class is transient": {
			loc:  keychain,
			err:  &KeychainError{Class: errs.KeychainClass("UserCanceled"), Detail: "the user pressed Cancel", Transient: true},
			want: Outcome{Kind: OutcomeTransient, Reason: "keychain read failed: UserCanceled: the user pressed Cancel"},
		},
		"success: a wrapped keychain failure still classifies": {
			loc:  keychain,
			err:  fmt.Errorf("resolve the live row: %w", &KeychainError{Class: errs.KeychainLocked, Transient: true}),
			want: Outcome{Kind: OutcomeLocked},
		},
		"success: a corrupt blob is transient rather than absent": {
			// Absent would lead to writing a new file over the corrupt one;
			// transient leaves it alone for the user to look at.
			loc:  keychain,
			err:  fmt.Errorf("parse credentials: unexpected end of JSON"),
			want: Outcome{Kind: OutcomeTransient, Reason: "parse credentials: unexpected end of JSON"},
		},
		"success: a file read that succeeded is the credential": {
			loc:  file,
			err:  nil,
			want: Outcome{Kind: OutcomeCredential},
		},
		"success: a missing credential file is absent": {
			loc:  file,
			err:  fmt.Errorf("open credentials: %w", fs.ErrNotExist),
			want: Outcome{Kind: OutcomeAbsent},
		},
		"success: a file permission failure is transient": {
			loc:  file,
			err:  fmt.Errorf("open credentials: %w", fs.ErrPermission),
			want: Outcome{Kind: OutcomeTransient, Reason: "open credentials: permission denied"},
		},
		"error: the zero location fails closed on any error": {
			loc:  Location{},
			err:  fmt.Errorf("read from nowhere"),
			want: Outcome{Kind: OutcomeTransient, Reason: "read from nowhere"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tt.loc.Classify(tt.err)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Classify(%v) mismatch (-want +got):\n%s", tt.err, diff)
			}
		})
	}
}

func TestKeychainFailuresNeverFallThroughToTheFile(t *testing.T) {
	t.Parallel()

	// The rule the type system cannot state alone: every keychain failure
	// classifies into {absent, locked, transient}, and no outcome kind
	// exists that names another store. A transient keychain failure leaves
	// the credential file unconsulted, because the two stores can hold
	// different accounts' credentials.
	keychain := KeychainItem("Claude Code-credentials")
	failures := []error{
		&KeychainError{Class: errs.KeychainNotFound},
		&KeychainError{Class: errs.KeychainLocked, Transient: true},
		&KeychainError{Class: errs.KeychainTimeout, Detail: "security timed out after 2000 ms", Transient: true},
		&KeychainError{Class: errs.KeychainUnavailable, Transient: true},
		&KeychainError{Class: KeychainClassUnsupported},
		&KeychainError{Class: errs.KeychainClass("AuthFailed"), Transient: true},
		fmt.Errorf("something unclassified"),
	}
	for _, err := range failures {
		got := keychain.Classify(err)
		switch got.Kind {
		case OutcomeAbsent, OutcomeLocked, OutcomeTransient:
		default:
			t.Errorf("Classify(%v) = %+v, which is not a keychain-only outcome", err, got)
		}
		if got.Kind == OutcomeTransient && !strings.Contains(got.Reason, err.Error()) {
			t.Errorf("Classify(%v) dropped the reason: %+v", err, got)
		}
	}
}
