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
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestCredentialPayloadRedaction(t *testing.T) {
	const marker = "planted-credential-marker-NEVER-LOG"
	read := ReadOutcome{Bytes: []byte(marker)}
	write := WriteRequest{BlobJSON: []byte(marker)}
	line, err := NewKeychainStdinLine("account", "service", []byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ value any }{
		"success: read value":    {value: read},
		"success: read pointer":  {value: &read},
		"success: write value":   {value: write},
		"success: write pointer": {value: &write},
		"success: line value":    {value: *line},
		"success: line pointer":  {value: line},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			outputs := make(map[string]string)
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "% X", "%020.8s"} {
				outputs[verb] = fmt.Sprintf(verb, tt.value)
				outputs["nested "+verb] = fmt.Sprintf(verb, struct{ Payload any }{tt.value})
			}
			for name, handler := range map[string]func(*bytes.Buffer) slog.Handler{
				"text log": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
				"JSON log": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
			} {
				var buf bytes.Buffer
				slog.New(handler(&buf)).InfoContext(t.Context(), "payload", "credential", tt.value)
				outputs[name] = buf.String()
			}
			encoded, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(`"[REDACTED]"`, string(encoded)); diff != "" {
				t.Errorf("ordinary JSON (-want +got):\n%s", diff)
			}
			outputs["ordinary JSON"] = string(encoded)
			marshaler := tt.value.(interface{ MarshalJSON() ([]byte, error) })
			encoded, err = marshaler.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(`"[REDACTED]"`, string(encoded)); diff != "" {
				t.Errorf("MarshalJSON (-want +got):\n%s", diff)
			}
			outputs["MarshalJSON"] = string(encoded)
			var streaming bytes.Buffer
			streamMarshaler := tt.value.(interface{ MarshalJSONTo(*jsontext.Encoder) error })
			if err := streamMarshaler.MarshalJSONTo(jsontext.NewEncoder(&streaming)); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(`"[REDACTED]"`, strings.TrimSpace(streaming.String())); diff != "" {
				t.Errorf("MarshalJSONTo (-want +got):\n%s", diff)
			}
			outputs["MarshalJSONTo"] = streaming.String()
			for path, output := range outputs {
				for _, forbidden := range []string{marker, hex.EncodeToString([]byte(marker)), base64.StdEncoding.EncodeToString([]byte(marker)), fmt.Sprintf("%d", []byte(marker))} {
					if strings.Contains(output, forbidden) {
						t.Errorf("%s exposed credential bytes", path)
					}
				}
			}
		})
	}
}
