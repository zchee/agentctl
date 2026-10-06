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

package codex

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
)

func TestStatusOutputLineEndings(t *testing.T) {
	tests := map[string]struct {
		json bool
		raw  bool
	}{
		"success: JSON document":          {json: true},
		"success: JSON with raw document": {json: true, raw: true},
		"success: table with raw block":   {raw: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "live")
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`)) + ".status-test"
			auth := fmt.Appendf(nil, `{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"status-test-refresh","account_id":"status-test-account"}}`, jwt)
			if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"rate_limit":{},"last":7}`)
			}))
			defer server.Close()
			var out bytes.Buffer
			status := Status{Env: &codexprovider.Env{CodexHome: home, Home: root}, Client: codexprovider.NewUsageClient(server.URL, "status-test", time.Second), Out: &out}
			if err := status.Run(t.Context(), cli.Globals{ConfigDir: filepath.Join(root, "config")}, cli.CodexStatusOptions{JSON: test.json, Raw: test.raw}); err != nil {
				t.Fatal(err)
			}
			body := out.Bytes()
			if test.json {
				if len(body) < 2 || !bytes.Equal(body[len(body)-2:], []byte("}\n")) {
					t.Fatalf("JSON document must end with exactly one LF: %q", body)
				}
				return
			}
			_, raw, found := bytes.Cut(body, []byte("\n--- raw:"))
			if !found {
				t.Fatalf("raw block missing: %q", body)
			}
			if diff := gocmp.Diff(" live ---\n{\n  \"rate_limit\": {},\n  \"last\": 7\n}\n", string(raw)); diff != "" {
				t.Fatalf("raw block bytes (-want +got):\n%s", diff)
			}
		})
	}
}
