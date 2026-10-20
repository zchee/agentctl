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

	"github.com/zchee/agentctl/internal/errs"
)

// KeychainStdinLine is the name the swap and refresh paths still build
// keychain updates under.
//
// The alias keeps those paths compiling until they build updates through
// [NewKeychainUpdate] directly; new code uses [KeychainUpdate].
type KeychainStdinLine = KeychainUpdate

// KeychainLineLimit is the longest line the retired `security -i` transport
// accepted, counting the trailing newline.
//
// The writer passes the document in argv, bounded by [KeychainBlobLimit];
// this bound is still enforced by [NewKeychainStdinLine], so the swap's
// refusal D keeps its behaviour until the swap and refresh paths move to
// [NewKeychainUpdate].
const KeychainLineLimit = 4032

// keychainRefusalExit is the process exit status the over-long-line refusal
// carries.
const keychainRefusalExit = 12

// LineTooLongError reports a credential whose stdin line would be over
// [KeychainLineLimit].
//
// [NewKeychainUpdate] reports [BlobTooLargeError] instead; only
// [NewKeychainStdinLine] still returns this.
type LineTooLongError struct {
	// Len is the line's length in bytes, trailing newline included.
	Len int
}

// Error states the length and the limit it exceeded.
func (e *LineTooLongError) Error() string {
	return fmt.Sprintf("the keychain line is %d bytes, over the %d-byte `security -i` limit", e.Len, KeychainLineLimit)
}

// Unwrap classifies the failure as the lettered refusal whose exit status
// scripts branch on.
func (e *LineTooLongError) Unwrap() error {
	return errs.NewRefusedLetter(keychainRefusalExit, "D")
}

// NewKeychainStdinLine refuses, with [LineTooLongError], a document whose
// retired stdin line would be over [KeychainLineLimit], and otherwise builds
// the update through [NewKeychainUpdate]. It is kept only for the swap and
// refresh paths that still build their update through it; new code uses
// [NewKeychainUpdate].
func NewKeychainStdinLine(account, service string, blob []byte) (*KeychainStdinLine, error) {
	update, err := NewKeychainUpdate(account, service, blob)
	if err != nil {
		return nil, err
	}
	// The retired line was `add-generic-password -U -a "<account>" -s
	// "<service>" -X "<hex>"` and a newline: 42 fixed bytes.
	if size := 42 + len(account) + len(service) + update.Len(); size > KeychainLineLimit {
		update.drop()
		return nil, &LineTooLongError{Len: size}
	}
	return update, nil
}
