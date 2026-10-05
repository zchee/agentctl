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
	"encoding/json/jsontext"
	"fmt"
	"io"
	"log/slog"
)

// String returns a redacted representation of the read result.
func (ReadOutcome) String() string { return redacted }

// GoString returns a redacted Go-syntax representation.
func (ReadOutcome) GoString() string { return redacted }

// Format redacts every formatting verb and flag.
func (ReadOutcome) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redacted)
}

// LogValue returns a redacted structured-log value.
func (ReadOutcome) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON returns a redacted JSON string.
func (ReadOutcome) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo writes a redacted JSON string through the streaming hook.
func (ReadOutcome) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String(redacted))
}

// String returns a redacted representation of the write request.
func (WriteRequest) String() string { return redacted }

// GoString returns a redacted Go-syntax representation.
func (WriteRequest) GoString() string { return redacted }

// Format redacts every formatting verb and flag.
func (WriteRequest) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redacted)
}

// LogValue returns a redacted structured-log value.
func (WriteRequest) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON returns a redacted JSON string.
func (WriteRequest) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo writes a redacted JSON string through the streaming hook.
func (WriteRequest) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String(redacted))
}

// MarshalJSON returns a redacted JSON string.
func (KeychainStdinLine) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalJSONTo writes a redacted JSON string through the streaming hook.
func (KeychainStdinLine) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String(redacted))
}
