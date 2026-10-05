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
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

func TestRefreshStateReadDoesNotCreateAnything(t *testing.T) {
	paths := config.NewPaths(t.TempDir())
	store, err := NewRefreshStateStore(paths, "user", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Load(t.Context()); got.Kind != RefreshStateAbsent {
		t.Fatalf("read absent state: %+v", got)
	}
	if _, err := os.Stat(paths.CodexRoot()); !os.IsNotExist(err) {
		t.Fatal("read-only inspection created Codex root")
	}
}

func TestRefreshStateReadsPrivatePolicyWithoutCredentials(t *testing.T) {
	tests := map[string]struct {
		body string
		want RefreshState
	}{
		"success: old minimal marker": {`{"schema":1}`, NewRefreshState()},
		"success: inflight marker":    {`{"schema":1,"inflight":{"sent_digest8":"0123abcd","sent_at":"2026-01-01T00:00:00Z"},"class":"interrupted"}`, RefreshState{}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureCodexDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			store, err := NewRefreshStateStore(paths, "user", "acct")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.shown, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got := store.Load(t.Context())
			if got.Kind != RefreshStatePresent {
				t.Fatalf("read: %+v", got)
			}
			if got.State.FloorMin != 60 || got.State.Schema != 1 {
				t.Fatal("defaults lost")
			}
			if got.State.Inflight == nil {
				if diff := gocmp.Diff(test.want, got.State); diff != "" {
					t.Fatal(diff)
				}
			} else if got.State.Inflight.SentDigest8 != "0123abcd" || got.State.Class == nil || *got.State.Class != RefreshUnknownInterrupted {
				t.Fatal("inflight facts lost")
			}
		})
	}
}

func TestRefreshStateUnavailableIsNeverAbsent(t *testing.T) {
	tests := map[string]struct {
		body                           string
		symlink, directory, unreadable bool
	}{
		"error: malformed":                  {body: "planted-private-value"},
		"error: empty":                      {},
		"error: missing schema":             {body: `{"floor_min":60}`},
		"error: future schema":              {body: `{"schema":99}`},
		"error: null floor":                 {body: `{"schema":1,"floor_min":null}`},
		"error: missing inflight timestamp": {body: `{"schema":1,"inflight":{"sent_digest8":"0123abcd"}}`},
		"error: null inflight timestamp":    {body: `{"schema":1,"inflight":{"sent_digest8":"0123abcd","sent_at":null}}`},
		"error: missing earliest digest":    {body: `{"schema":1,"earliest_refresh":{"at":"2026-01-01T00:00:00Z"}}`},
		"error: negative floor":             {body: `{"schema":1,"floor_min":-1}`},
		"error: overflowing count":          {body: `{"schema":1,"did_not_help":256}`},
		"error: unknown class":              {body: `{"schema":1,"class":"planted-private-value"}`},
		"error: timestamp":                  {body: `{"schema":1,"last_sent_at":"planted-private-value"}`},
		"error: oversized":                  {body: strings.Repeat("x", int(maxRefreshStateBytes)+1)},
		"error: symlink":                    {symlink: true},
		"error: directory":                  {directory: true},
		"error: unreadable":                 {body: `{"schema":1}`, unreadable: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := config.NewPaths(t.TempDir())
			if err := paths.EnsureCodexDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			store, err := NewRefreshStateStore(paths, "user", "acct")
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case test.symlink:
				err = os.Symlink(paths.ConfigDir(), store.shown)
			case test.directory:
				err = os.Mkdir(store.shown, 0o700)
			default:
				err = os.WriteFile(store.shown, []byte(test.body), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.unreadable {
				if err := os.Chmod(store.shown, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(store.shown, 0o600) }()
			}
			got := store.Load(t.Context())
			if got.Kind != RefreshStateUnavailable {
				t.Fatalf("unsafe marker classified as %+v", got)
			}
			if strings.Contains(got.Reason, "planted-private-value") {
				t.Fatal("parse error quoted marker payload")
			}
		})
	}
}

func TestRefreshStateFieldOrderAndNulls(t *testing.T) {
	data, err := json.Marshal(NewRefreshState())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":1,"inflight":null,"floor_min":60,"did_not_help":0,"ambiguous_since":null,"class":null,"resent":false,"retry_after":null,"last_sent_at":null,"earliest_refresh":null,"dead_digest8":null}`
	if diff := gocmp.Diff(want, string(data)); diff != "" {
		t.Fatal(diff)
	}
}
