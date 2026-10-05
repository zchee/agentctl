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

package testutil

import (
	"testing"
)

func TestSchemaError(t *testing.T) {
	t.Parallel()

	// One conforming and one non-conforming document per embedded schema.
	// The non-conforming one carries a version no consumer knows and none
	// of the required members, so it must fail whatever else changes.
	tests := map[string]struct {
		schema   string
		document string
		wantErr  bool
	}{
		"success: an empty first status report conforms": {
			schema:   "status.v1.json",
			document: `{"version": 1, "generated_at": "2026-09-08T00:00:00Z", "rows": [], "hidden": 0}`,
		},
		"error: the first status schema refuses an unknown version": {
			schema:   "status.v1.json",
			document: `{"version": 999}`,
			wantErr:  true,
		},
		"success: an empty second status report conforms": {
			schema:   "status.v2.json",
			document: `{"version": 2, "generated_at": "2026-09-08T00:00:00Z", "rows": [], "hidden": 0}`,
		},
		"error: the second status schema refuses the first version": {
			schema:   "status.v2.json",
			document: `{"version": 1, "generated_at": "2026-09-08T00:00:00Z", "rows": [], "hidden": 0}`,
			wantErr:  true,
		},
		"success: an empty isolation report conforms": {
			schema: "doctor.v1.json",
			document: `{
				"version": 1,
				"isolation": [],
				"isolation_policy": {"disable_sideload_flags": null, "backend_observable": false}
			}`,
		},
		"error: the isolation schema refuses an undeclared member": {
			schema: "doctor.v1.json",
			document: `{
				"version": 1,
				"isolation": [],
				"isolation_policy": {"disable_sideload_flags": null, "backend_observable": false},
				"surplus": true
			}`,
			wantErr: true,
		},
		"success: an empty diagnosis report conforms": {
			schema: "codex-doctor.v1.json",
			document: `{
				"version": 1,
				"home": {"path": null, "symlink_chain": [], "error": "unset"},
				"store": {"mode": "file", "read": "file", "coarse_match": false, "profiles_consulted": false, "base_url": null, "config_note": null},
				"environment": [{"name": "CODEX_API_KEY", "present": false}],
				"live": {"state": "absent", "auth_mode": null, "mode_bits": null, "mode_warning": null, "size": null, "access_expiry": null, "last_refresh": null, "matches_namespace": null, "daemon": "none", "missing_known_members": [], "unknown_member_count": 0},
				"foreign": {"multi_auth_present": false, "switcher_items": 0, "codex_auth_items": 0, "unexplained_removals": [], "unexplained_items": 0, "unnameable_items": 0},
				"namespaces": [],
				"orphans": [],
				"unnameable_orphans": 0,
				"audit": [],
				"notes": []
			}`,
		},
		"error: the diagnosis schema refuses a missing member": {
			schema:   "codex-doctor.v1.json",
			document: `{"version": 999}`,
			wantErr:  true,
		},
		"error: an unknown schema name reports the read failure": {
			schema:   "no-such-schema.json",
			document: `{}`,
			wantErr:  true,
		},
		"error: an unparseable document reports the parse failure": {
			schema:   "status.v1.json",
			document: `{not json`,
			wantErr:  true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := SchemaError(tt.schema, []byte(tt.document))
			if (err != nil) != tt.wantErr {
				t.Fatalf("SchemaError(%q) = %v, wantErr %t", tt.schema, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSchemaAcceptsAConformingDocument(t *testing.T) {
	t.Parallel()

	ValidateSchema(t, "status.v1.json", []byte(`{"version": 1, "generated_at": "2026-09-08T00:00:00Z", "rows": [], "hidden": 0}`))
}
