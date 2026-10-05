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

//go:build agentctl_testing

package codex

import (
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRefreshDurabilityFailureNeverMintsSendAuthorization(t *testing.T) {
	tests := map[string]struct {
		phase   string
		renamed bool
	}{
		"error: temporary write": {"codex_refresh_state_write", false},
		"error: file flush":      {"codex_refresh_state_file_sync", false},
		"error: rename":          {"codex_refresh_state_rename", false},
		"error: directory flush": {"codex_refresh_state_dir_sync", true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths, namespace, guard, _ := writerNamespace(t)
			store := refreshWriterStore(t, paths, guard)
			if err := ResetRefreshStateForLogin(t.Context(), paths, guard); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENTCTL_FAULT", test.phase)
			token, err := store.writeInflight(t.Context(), lockedRead(t, namespace))
			if err == nil || token != nil {
				t.Fatal("failed durability authorized POST")
			}
			read := store.Load(t.Context())
			if read.Kind != RefreshStatePresent {
				t.Fatal(read.Reason)
			}
			if (read.State.Inflight != nil) != test.renamed {
				t.Fatalf("wrong durability boundary: %+v", read.State)
			}
			if !test.renamed {
				if diff := gocmp.Diff(NewRefreshState(), read.State); diff != "" {
					t.Fatal(diff)
				}
			}
			entries, err := os.ReadDir(store.dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != store.name {
				t.Fatalf("temporary leaked: %v %v", entries, err)
			}
			t.Setenv("AGENTCTL_FAULT", "")
			if test.renamed {
				if _, err := store.writeInflight(t.Context(), lockedRead(t, namespace)); err == nil {
					t.Fatal("uncertain durable marker rearmed automatic POST")
				}
			}
		})
	}
}
