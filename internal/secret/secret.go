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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/awnumar/memguard"
)

const redacted = "[REDACTED]"

// Secret keeps immutable token bytes encrypted outside plaintext operations.
// Copies share the encrypted backing. The zero value contains no secret.
// Formatting and ordinary serialization never expose its contents.
type Secret struct {
	enclave *memguard.Enclave
}

// NewSecret seals b and wipes its contents. Empty input returns an error.
// The caller must own writable b and must not use it concurrently.
// Failure to allocate locked memory panics after memguard purges the session.
func NewSecret(b []byte) (*Secret, error) {
	defer memguard.WipeBytes(b)
	if len(b) == 0 {
		return nil, errors.New("secret must not be empty")
	}
	return &Secret{enclave: memguard.NewEnclave(b)}, nil
}

// WithPlaintext opens a locked, read-only buffer for one synchronous operation.
// The callback must not modify, retain, or use the slice after it returns.
// The buffer is destroyed on return or panic. An invalid or purged secret
// returns an error without calling fn; callback errors are passed through.
// Allocation failures panic after memguard purges the session.
func (s *Secret) WithPlaintext(fn func([]byte) error) error {
	if s == nil || s.enclave == nil {
		return errors.New("secret is not initialized")
	}
	if fn == nil {
		return errors.New("secret plaintext callback is nil")
	}
	buffer, err := s.enclave.Open()
	if err != nil {
		return fmt.Errorf("open secret: %w", err)
	}
	defer buffer.Destroy()
	return fn(buffer.Bytes())
}

// Digest returns the lowercase SHA-256 hex digest, computed while locked.
// It returns an error when the secret cannot be opened.
func (s *Secret) Digest() (string, error) {
	var digest [sha256.Size]byte
	if err := s.WithPlaintext(func(b []byte) error {
		digest = sha256.Sum256(b)
		return nil
	}); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest[:]), nil
}

// Digest8 returns the eight-hex-digit digest prefix used by audit records.
// It returns an error when the secret cannot be opened.
func (s *Secret) Digest8() (string, error) {
	digest, err := s.Digest()
	if err != nil {
		return "", err
	}
	return digest[:8], nil
}

// Len returns the byte length without opening plaintext, or zero if absent.
func (s *Secret) Len() int {
	if s == nil || s.enclave == nil {
		return 0
	}
	return s.enclave.Size()
}

// Equal compares equally sized secrets in constant time with both locked.
// Lengths are not secret. It returns an error if either secret cannot be opened.
func (s *Secret) Equal(other *Secret) (bool, error) {
	var equal bool
	err := s.WithPlaintext(func(left []byte) error {
		return other.WithPlaintext(func(right []byte) error {
			equal = subtle.ConstantTimeCompare(left, right) == 1
			return nil
		})
	})
	return equal, err
}

// Clone returns a new wrapper sharing immutable encrypted backing, without
// opening plaintext. A nil receiver stays nil; Purge invalidates all copies.
func (s *Secret) Clone() *Secret {
	if s == nil {
		return nil
	}
	return &Secret{enclave: s.enclave}
}

// AppendPlaintextTo is the only serialization path that copies plaintext out
// of the enclave. It is reserved for credential documents sent to their store.
// The caller must wipe the returned slice after that I/O; ordinary logging and
// serialization must never use it. An open failure leaves dst unchanged.
func (s *Secret) AppendPlaintextTo(dst []byte) ([]byte, error) {
	err := s.WithPlaintext(func(b []byte) error {
		dst = append(dst, b...)
		return nil
	})
	return dst, err
}

// String returns a redacted representation.
func (Secret) String() string { return redacted }

// GoString returns a redacted Go-syntax representation.
func (Secret) GoString() string { return redacted }

// Format redacts every verb, width, precision, and formatting flag.
func (Secret) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redacted)
}

// LogValue returns a redacted structured-log value.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON returns a redacted JSON string.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo writes a redacted JSON string through the streaming hook.
func (Secret) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String(redacted))
}

// MarshalText returns redacted text.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// AppendText appends redacted text without changing the existing prefix.
func (Secret) AppendText(dst []byte) ([]byte, error) { return append(dst, redacted...), nil }
