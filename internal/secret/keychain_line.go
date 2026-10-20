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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"

	"github.com/zchee/agentctl/internal/errs"
)

// KeychainBlobLimit is the largest credential document, in plaintext bytes,
// a keychain update carries.
//
// The document travels to security(1) as lowercase hex in one argument, so
// the ceiling costs twice its size in argument space: 512 KiB of hex. That is
// about half of the argument space a macOS host offers (ARG_MAX is 1 MiB,
// shared with the environment and the other arguments), and more than twenty
// times the size of a real item with its MCP tokens. It is a portable bound,
// not the host's own limit: a host with less argument space refuses the list
// at spawn time with E2BIG, which the writer reports as the same refusal.
const KeychainBlobLimit = 256 << 10

// credentialTooLargeExit is the process exit status the over-ceiling refusal
// carries. It equals cli.SwapExitCredentialTooLarge; this package does not
// import the command-line layer, so a test pins the two together.
const credentialTooLargeExit = 31

// ErrCredentialTooLarge matches a credential document a keychain update
// cannot carry: one over [KeychainBlobLimit] at construction, or one whose
// argument list the host refused before security(1) started. In both cases
// nothing was written to the keychain.
var ErrCredentialTooLarge = &KeychainError{Class: errs.KeychainCredentialTooLarge}

// BlobTooLargeError reports a credential document over [KeychainBlobLimit].
// Nothing was built, nothing was spawned and the keychain was not touched.
type BlobTooLargeError struct {
	// Len is the document's length in plaintext bytes.
	Len int
}

// Error states the length and the ceiling it exceeded.
func (e *BlobTooLargeError) Error() string {
	return fmt.Sprintf("the credential document is %d bytes, over the %d-byte keychain update ceiling", e.Len, KeychainBlobLimit)
}

// Is matches [ErrCredentialTooLarge], so a caller tells the constructor's
// refusal and the host's E2BIG apart from every other failure with one
// check.
func (e *BlobTooLargeError) Is(target error) bool {
	return target == ErrCredentialTooLarge
}

// Unwrap classifies the failure as the unlettered refusal whose exit status
// and reason token scripts branch on.
func (e *BlobTooLargeError) Unwrap() error {
	return errs.NewRefused(credentialTooLargeExit, errs.ReasonCredentialTooLarge)
}

// UnquotableError reports an account or service name that cannot be handed
// to security(1) as the value of one option.
type UnquotableError struct {
	// Field is "account" or "service".
	Field string
}

// Error names the refused field.
func (e *UnquotableError) Error() string {
	return fmt.Sprintf("the %s name cannot be passed to security as a single option value", e.Field)
}

// KeychainUpdate is one keychain item update, built and ready for the
// keychain writer: the credential document as lowercase hex, sealed, and the
// item it was built for.
//
// Three things make this a type rather than a bare secret:
//
//   - It can only be built by [NewKeychainUpdate]; its fields are
//     unexported and there is no other constructor, so the writer cannot be
//     handed a bare token or a hand-assembled payload in place of one that
//     went through the size and name rules.
//   - It records who it was built for. The account and service are not
//     secrets; carrying them is what lets the writer refuse an update built
//     for a different item than the target it was asked to write.
//   - It never prints its payload: every formatting, logging and JSON form
//     names the item and the payload's length only.
type KeychainUpdate struct {
	hex     *Secret
	account string
	service string
	size    int
}

// NewKeychainUpdate builds the update that stores blob, a serialized
// credential document, under the item named by account and service.
//
// account is the item's acct attribute and service its svce attribute. Both
// are validated before anything else: neither may be empty, start with "-",
// or carry a control character, a double quote or a backslash, and the
// account may not carry whitespace. A service with a space is valid, because
// the live item's service has one and the writer passes it as one argument.
// A refused name returns [UnquotableError].
//
// A blob over [KeychainBlobLimit] returns [BlobTooLargeError]. The payload is
// blob's lowercase hex, encoded straight into a buffer that is sealed at
// once, so the caller keeps ownership of the plaintext document: blob is
// read, never retained, and wiping it afterwards stays the caller's job.
func NewKeychainUpdate(account, service string, blob []byte) (*KeychainUpdate, error) {
	if err := quotable("account", account, true); err != nil {
		return nil, err
	}
	if err := quotable("service", service, false); err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, errors.New("the credential document is empty")
	}
	if len(blob) > KeychainBlobLimit {
		return nil, &BlobTooLargeError{Len: len(blob)}
	}

	payload := hex.AppendEncode(make([]byte, 0, hex.EncodedLen(len(blob))), blob)
	size := len(payload)
	sealed, err := NewSecret(payload)
	if err != nil {
		return nil, err
	}
	return &KeychainUpdate{hex: sealed, account: account, service: service, size: size}, nil
}

// quotable refuses a name that security(1) could read as something other
// than the value of its option.
//
// The writer passes each name as its own argv element, so nothing is quoted
// or shell-parsed; what remains is the option parser. A leading "-" would be
// read as an option, and a control character, a quote or a backslash has no
// place in either name, so refusing them costs nothing. The account must
// also be free of whitespace: it is the login name the vendor tooling stores
// its items under, and a space there means the environment was not what the
// writer expects, so the write fails closed.
func quotable(field, value string, noSpace bool) error {
	if value == "" || strings.HasPrefix(value, "-") {
		return &UnquotableError{Field: field}
	}
	if strings.ContainsFunc(value, func(r rune) bool {
		return r == '"' || r == '\\' || unicode.IsControl(r) || (noSpace && unicode.IsSpace(r))
	}) {
		return &UnquotableError{Field: field}
	}
	return nil
}

// Account returns the item's acct attribute this update was built for.
func (u *KeychainUpdate) Account() string { return u.account }

// Service returns the keychain service name this update was built for.
func (u *KeychainUpdate) Service() string { return u.service }

// Len returns the payload's length in hex digits: twice the document's
// length in bytes.
func (u *KeychainUpdate) Len() int { return u.size }

// String prints the item the update names and its length, never the payload.
func (u KeychainUpdate) String() string {
	return fmt.Sprintf("KeychainUpdate{account: %q, service: %q, len: %d, hex: <redacted>}", u.account, u.service, u.size)
}

// GoString prints the same redacted form as String.
func (u KeychainUpdate) GoString() string { return u.String() }

// Format prints the same redacted form as String for every verb, width,
// precision and flag.
func (u KeychainUpdate) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, u.String())
}

// LogValue prints the same redacted form as String.
func (u KeychainUpdate) LogValue() slog.Value { return slog.StringValue(u.String()) }

// drop forgets the sealed payload, so a consumed or refused update cannot be
// written again. The payload is held encrypted, so there is no plaintext
// here to wipe; the writer's plaintext lives only in the locked buffer it
// opens for the write.
func (u *KeychainUpdate) drop() { u.hex = nil }
