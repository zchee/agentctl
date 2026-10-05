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
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/errs"
)

// KeychainLineLimit is the longest line `security -i` accepts, counting
// the trailing newline.
//
// The blob's author compares the whole line, newline included, against
// this bound and falls back to putting the payload in argv when it is
// longer. agentctl has no such fallback: a line one byte over the limit is
// refused and nothing is spawned, because an argv fallback is what would
// put a refresh token into the process listing.
const KeychainLineLimit = 4032

// keychainRefusalExit is the process exit status the over-long-line
// refusal carries, out of the 0–2 block so a script can tell this recorded
// cause from a degraded run.
const keychainRefusalExit = 12

// LineTooLongError reports a keychain line over [KeychainLineLimit].
// Nothing was spawned and the keychain was not touched.
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

// UnquotableError reports an account or service name that cannot be
// written into the quoted keychain line without changing what the line
// means.
type UnquotableError struct {
	// Field is "account" or "service".
	Field string
}

// Error names the unquotable field.
func (e *UnquotableError) Error() string {
	return fmt.Sprintf("the %s name cannot be quoted into a `security -i` line", e.Field)
}

// KeychainStdinLine is one keychain update line, built and ready for
// `security -i`'s standard input.
//
// Three things make this a type rather than a bare secret:
//
//   - It can only be built by [NewKeychainStdinLine]; the line field is
//     unexported and there is no other constructor, so the write transport
//     cannot be handed a bare token or a hand-assembled line in place of
//     one that went through the shape and length rules.
//   - It records who it was built for. The account and service are not
//     secrets; carrying them is what lets the write transport refuse a
//     line built for a different item than the target it was asked to
//     write.
//   - It is written without being read back. [KeychainStdinLine.WriteTo]
//     hands the bytes to the sink from inside the locked buffer, so no
//     caller ever holds the plaintext line.
type KeychainStdinLine struct {
	line    *Secret
	account string
	service string
	size    int
}

// NewKeychainStdinLine builds the keychain update line that stores blob —
// a serialized credential document — under the named item, ready for
// `security -i`'s standard input.
//
// account is the item's acct attribute and service is the target item's
// service name. Both are recorded in the returned value so the write
// transport can refuse a line built for a different item than the one it
// was asked to write.
//
// The payload is blob's lowercase hex, so the caller keeps ownership of
// the plaintext document: blob is read, never retained, and wiping it
// afterwards stays the caller's job.
//
// A finished line — trailing newline included — over [KeychainLineLimit]
// returns [LineTooLongError], whose exit classification is the lettered
// refusal for an over-long credential line.
func NewKeychainStdinLine(account, service string, blob []byte) (*KeychainStdinLine, error) {
	if err := quotable("account", account); err != nil {
		return nil, err
	}
	if err := quotable("service", service); err != nil {
		return nil, err
	}

	prefix := `add-generic-password -U -a "` + account + `" -s "` + service + `" -X "`
	line := make([]byte, 0, len(prefix)+hex.EncodedLen(len(blob))+2)
	line = append(line, prefix...)
	line = hex.AppendEncode(line, blob)
	line = append(line, '"', '\n')

	size := len(line)
	if size > KeychainLineLimit {
		memguard.WipeBytes(line)
		return nil, &LineTooLongError{Len: size}
	}

	sealed, err := NewSecret(line)
	if err != nil {
		return nil, err
	}
	return &KeychainStdinLine{line: sealed, account: account, service: service, size: size}, nil
}

// quotable refuses a value that would change what the quoted line means.
//
// `security -i` reads a command line, so a value carrying a quote, a
// backslash or a control character could end the field early and append
// arguments of its own — a second -s, or a second command. Neither name
// can carry one in practice, which is exactly why refusing costs nothing
// and closes the case anyway.
func quotable(field, value string) error {
	if strings.ContainsFunc(value, func(r rune) bool { return r == '"' || r == '\\' || unicode.IsControl(r) }) {
		return &UnquotableError{Field: field}
	}
	return nil
}

// Account returns the item's acct attribute this line was built for.
func (l *KeychainStdinLine) Account() string { return l.account }

// Service returns the keychain service name this line was built for.
func (l *KeychainStdinLine) Service() string { return l.service }

// Len returns the line's length in bytes, trailing newline included: what
// the [KeychainLineLimit] rule is measured against, and what the write
// transport re-checks before it spawns anything.
func (l *KeychainStdinLine) Len() int { return l.size }

// WriteTo writes the line to sink from inside the locked buffer and keeps
// no copy. The returned count is the whole line on success; a short write
// is the sink's error.
func (l *KeychainStdinLine) WriteTo(sink io.Writer) (int64, error) {
	var written int64
	err := l.line.WithPlaintext(func(b []byte) error {
		n, writeErr := sink.Write(b)
		written = int64(n)
		return writeErr
	})
	return written, err
}

// String prints the item the line names and its length, never the line.
func (l *KeychainStdinLine) String() string {
	return fmt.Sprintf("KeychainStdinLine{account: %q, service: %q, len: %d, line: <redacted>}", l.account, l.service, l.size)
}

// GoString prints the same redacted form as String.
func (l *KeychainStdinLine) GoString() string { return l.String() }

// Format prints the same redacted form as String for every verb, width,
// precision and flag.
func (l *KeychainStdinLine) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, l.String())
}

// LogValue prints the same redacted form as String.
func (l *KeychainStdinLine) LogValue() slog.Value { return slog.StringValue(l.String()) }
