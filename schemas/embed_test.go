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

package schemas_test

import (
	json "encoding/json/v2"
	"testing"

	"github.com/zchee/agentctl/internal/testutil"
	"github.com/zchee/agentctl/schemas"
)

// TestFSContents proves each schema document is embedded and is valid JSON,
// so a truncated or dropped file fails here instead of inside a consumer.
func TestFSContents(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
	}{
		"success: claude status schema embedded":    {path: "status.v1.json"},
		"success: codex status schema embedded":     {path: "status.v2.json"},
		"success: isolation report schema embedded": {path: "doctor.v1.json"},
		"success: codex doctor schema embedded":     {path: "codex-doctor.v1.json"},
		"success: desktop report schema embedded":   {path: "desktop.v1.json"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := schemas.FS.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", tt.path, err)
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("Unmarshal(%q): %v", tt.path, err)
			}
			if _, ok := doc["$schema"]; !ok {
				t.Fatalf("schema %q has no $schema member", tt.path)
			}
		})
	}
}

// TestDesktopSchemaShapes validates every document shape the desktop
// commands emit, and proves the schema rejects the shapes that would hide a
// failure: a count reported as zero text instead of a number or null, a
// status without its errors array, and an unknown member.
func TestDesktopSchemaShapes(t *testing.T) {
	t.Parallel()

	const status = `{
  "version": 1, "command": "status",
  "default": {
    "path": "/Users/u/Library/Application Support/Claude", "present": true,
    "identity": {"state": "uuid", "uuid": "11111111-2222-3333-4444-555555555555"},
    "word": "account", "running": 1, "family": 2,
    "family_rows": [{"pid": 11, "ppid": 10, "start": "2026-10-11T00:00:00Z", "path": "/Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper", "reason": "descendant"}],
    "foreign": [{"pid": 21, "ppid": 20, "start": "2026-10-11T00:00:00Z", "path": "/Applications/Claude.app/Contents/Helpers/chrome-native-host", "parent_path": "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}],
    "other_copies": []
  },
  "profiles": [{"name": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "identity": {"state": "fresh"}, "word": "fresh", "duplicate": false, "in_place": false}],
  "open_intents": [],
  "errors": []
}`
	const partial = `{
  "version": 1, "command": "status",
  "default": {
    "path": "/Users/u/Library/Application Support/Claude", "present": false,
    "identity": {"state": "fresh"}, "word": "fresh", "running": null, "family": null,
    "family_rows": [], "foreign": [], "other_copies": []
  },
  "profiles": [],
  "open_intents": [{"op_id": "0123456789abcdef", "time": "2026-10-11T00:00:00Z", "command": "use", "requested_account": "11111111-2222-3333-4444-555555555555", "outgoing": "fresh-0123456789abcdef", "target": "11111111-2222-3333-4444-555555555555"}],
  "errors": [{"step": "processes", "message": "reading the executable path of process 900: input/output error"}]
}`
	const useApplied = `{
  "version": 1, "command": "use", "outcome": "selected", "op_id": "0123456789abcdef",
  "account": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "identity": "fresh", "journal": "applied",
  "outgoing": {"name": "11111111-2222-3333-4444-555555555555", "identity": {"state": "uuid", "uuid": "11111111-2222-3333-4444-555555555555"}, "destination": "/p/11111111-2222-3333-4444-555555555555", "file": {"dev": 1, "ino": 2}},
  "target": {"name": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "identity": {"state": "none"}, "source": "/p/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "target_existed": false, "park": "fresh-0123456789abcdef-target"},
  "main": {"pid": 1000, "start": "2026-10-11T00:00:00Z"}
}`
	const useRefused = `{"version": 1, "command": "use", "outcome": "desktop_quit_timeout", "detail": "0 main and 2 other processes", "account": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "journal": "aborted"}`
	const repair = `{
  "version": 1, "command": "repair", "outcome": "desktop_repair_needs_choice", "state": "outgoing-moved",
  "layout": [{"role": "data", "path": "/d", "present": false, "file": null}, {"role": "outgoing", "path": "/p/x", "present": true, "file": {"dev": 1, "ino": 3}}]
}`

	tests := map[string]struct {
		document string
		wantErr  bool
	}{
		"success: status with every row":                  {document: status},
		"success: status partial with null counts":        {document: partial},
		"success: use applied":                            {document: useApplied},
		"success: use refused":                            {document: useRefused},
		"success: repair layout":                          {document: repair},
		"error: a count as text":                          {document: `{"version":1,"command":"status","default":{"path":"/d","present":false,"identity":{"state":"fresh"},"word":"fresh","running":"0","family":0,"family_rows":[],"foreign":[],"other_copies":[]},"profiles":[],"open_intents":[],"errors":[]}`, wantErr: true},
		"error: status without its errors array":          {document: `{"version":1,"command":"status","default":{"path":"/d","present":false,"identity":{"state":"fresh"},"word":"fresh","running":0,"family":0,"family_rows":[],"foreign":[],"other_copies":[]},"profiles":[],"open_intents":[]}`, wantErr: true},
		"error: an outcome word the commands never emit":  {document: `{"version": 1, "command": "use", "outcome": "switched"}`, wantErr: true},
		"error: an unknown member":                        {document: `{"version": 1, "command": "use", "outcome": "selected", "token": "x"}`, wantErr: true},
		"error: a family reason this build never emits":   {document: `{"version":1,"command":"status","default":{"path":"/d","present":true,"identity":{"state":"fresh"},"word":"fresh","running":0,"family":1,"family_rows":[{"pid":1,"ppid":1,"start":"","path":"/x","reason":"guess"}],"foreign":[],"other_copies":[]},"profiles":[],"open_intents":[],"errors":[]}`, wantErr: true},
		"error: an account that is not a lower-case uuid": {document: `{"version": 1, "command": "use", "outcome": "selected", "account": "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"}`, wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := testutil.SchemaError("desktop.v1.json", []byte(tt.document))
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("SchemaError() = %v, want error %t\ndocument:\n%s", err, tt.wantErr, tt.document)
			}
		})
	}
}
