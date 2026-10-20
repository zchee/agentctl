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

package commands

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestAccountsRelocate(t *testing.T) {
	tests := map[string]struct {
		target, profile                      string
		changeSource, removeSource, noSource bool
		code                                 int
		message                              string
	}{
		"success: organization in credential":             {},
		"success: resume identical target":                {target: "same", message: "earlier run"},
		"success: profile with no account or email":       {profile: `{"organization":{"uuid":"` + testutil.Org + `","name":"Learned"}}`},
		"error: target contains another credential":       {target: "other", code: 1, message: "already exists"},
		"error: empty target exists":                      {target: "empty", code: 1, message: "already exists"},
		"error: source changed during profile request":    {profile: `{"organization":{"uuid":"` + testutil.Org + `"}}`, changeSource: true, code: 1, message: "changed during relocate"},
		"error: source removed during profile request":    {profile: `{"organization":{"uuid":"` + testutil.Org + `"}}`, removeSource: true, code: 1, message: "changed during relocate"},
		"error: profile names no organization":            {profile: `{"account":{"uuid":"someone"}}`, code: 1, message: "neither the stored credential"},
		"error: non-object profile names no organization": {profile: `[]`, code: 1, message: "neither the stored credential"},
		"error: organization cannot be a path":            {profile: `{"organization":{"uuid":"../other"}}`, code: 1},
		"error: source holds no credential":               {noSource: true, code: 1, message: "holds no credentials"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := seedStore(t)
			store.fixture.WriteRegistry([]any{store.fixture.OwnedRecord(testutil.Acct, config.UnknownOrg)})
			blob := store.fixture.Blob("sk-ant-oat01-relocate", "sk-ant-ort01-relocate", testutil.FreshAt())
			if tt.profile != "" {
				var document map[string]any
				if err := json.Unmarshal([]byte(blob), &document); err != nil {
					t.Fatal(err)
				}
				delete(document["claudeAiOauth"].(map[string]any)["tokenAccount"].(map[string]any), "organizationUuid")
				encoded, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				blob = string(encoded)
			}
			source := store.paths.NamespaceDir(testutil.Acct, config.UnknownOrg)
			target := store.paths.NamespaceDir(testutil.Acct, testutil.Org)
			if !tt.noSource {
				store.fixture.WriteCredentials(testutil.Acct, config.UnknownOrg, blob)
				for _, file := range []string{secret.PendingFile, secret.PendingMetaFile, secret.CredentialsFile + ".tmp.12345678"} {
					if err := os.WriteFile(filepath.Join(source, file), []byte("private pending"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch tt.target {
			case "same":
				store.fixture.WriteCredentials(testutil.Acct, testutil.Org, blob)
			case "other":
				store.fixture.WriteCredentials(testutil.Acct, testutil.Org, store.fixture.Blob("sk-ant-oat01-occupant", "sk-ant-ort01-occupant", testutil.FreshAt()))
			case "empty":
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var profile OrganizationProfileSource
			if tt.profile != "" {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("profile method=%s", r.Method)
					}
					if tt.changeSource {
						if err := os.WriteFile(filepath.Join(source, secret.CredentialsFile), []byte(strings.ReplaceAll(blob, "relocate", "rotated")), 0o600); err != nil {
							t.Error(err)
						}
					}
					if tt.removeSource {
						if err := os.Remove(filepath.Join(source, secret.CredentialsFile)); err != nil {
							t.Error(err)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tt.profile))
				}))
				t.Cleanup(server.Close)
				client, err := claude.NewOAuthClient(server.URL, server.URL, "test")
				if err != nil {
					t.Fatal(err)
				}
				profile = client
			}
			var out bytes.Buffer
			err := store.accounts(nil, &out).Relocate(t.Context(), cli.ClaudeAccountsRelocateOptions{ID: testutil.Acct, Yes: true}, TerminalPrompt{Out: &out}, profile)
			if code := errs.ExitCode(err); code != tt.code {
				t.Fatalf("exit=%d, want %d; error=%v", code, tt.code, err)
			}
			if strings.Contains(out.String(), "sk-ant-") {
				t.Fatal("credential leaked into report")
			}
			if tt.code != 0 {
				if !strings.Contains(err.Error(), tt.message) {
					t.Fatalf("error=%v, want %s", err, tt.message)
				}
				registry, err := config.LoadRegistry(t.Context(), store.paths)
				if err != nil {
					t.Fatal(err)
				}
				if registry.Get(testutil.Acct, config.UnknownOrg) == nil {
					t.Fatal("refusal changed registry")
				}
				if tt.changeSource || tt.removeSource || tt.target != "" {
					if _, err := os.Stat(filepath.Join(source, secret.PendingFile)); err != nil {
						t.Fatalf("refusal touched pending: %v", err)
					}
				}
				return
			}
			if !strings.Contains(out.String(), tt.message) {
				t.Fatalf("output missing %q: %s", tt.message, out.String())
			}
			if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source survives: %v", err)
			}
			info, err := os.Stat(filepath.Join(target, secret.CredentialsFile))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("credential mode=%#o", info.Mode().Perm())
			}
			registry, err := config.LoadRegistry(t.Context(), store.paths)
			if err != nil {
				t.Fatal(err)
			}
			if registry.Get(testutil.Acct, config.UnknownOrg) != nil {
				t.Fatal("old record survives")
			}
			record := registry.Get(testutil.Acct, testutil.Org)
			if record == nil {
				t.Fatal("new record absent")
			}
			if diff := gocmp.Diff(claude.ExportSpelling(target), record.Kind.Owned.ExportSpelling); diff != "" {
				t.Fatal(diff)
			}
			moved, err := readRelocationNamespace(target)
			if err != nil {
				t.Fatal(err)
			}
			original, err := claude.ParseBlob([]byte(blob))
			if err != nil {
				t.Fatal(err)
			}
			before, err := original.Digests()
			if err != nil {
				t.Fatal(err)
			}
			after, err := moved.Digests()
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(before, after); diff != "" {
				t.Fatalf("credentials changed: %s", diff)
			}
		})
	}
}

func TestAccountsRelocateRefusesANamespaceWhoseItemIsListed(t *testing.T) {
	tests := map[string]struct {
		// listed says which of the two checks finds the item listed.
		listed      [2]bool
		locked      bool
		noSource    bool
		wantRefusal bool
		wantMoved   bool
	}{
		"error: a listed item refuses before the probe of the source":             {listed: [2]bool{true, true}, noSource: true, wantRefusal: true},
		"error: a listed item refuses with the source and the registry unchanged": {listed: [2]bool{true, true}, wantRefusal: true},
		"error: an item listed only at the second check refuses under the guard":  {listed: [2]bool{false, true}, wantRefusal: true},
		"success: an unlisted item moves as before":                               {wantMoved: true},
		"success: a keychain that is not unlocked moves as before":                {listed: [2]bool{true, true}, locked: true, wantMoved: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := seedStore(t)
			store.fixture.WriteRegistry([]any{store.fixture.OwnedRecord(testutil.Acct, config.UnknownOrg)})
			source := store.paths.NamespaceDir(testutil.Acct, config.UnknownOrg)
			target := store.paths.NamespaceDir(testutil.Acct, testutil.Org)
			blob := store.fixture.Blob("sk-ant-oat01-relocate", "sk-ant-ort01-relocate", testutil.FreshAt())
			if !tt.noSource {
				store.fixture.WriteCredentials(testutil.Acct, config.UnknownOrg, blob)
			}
			registryBefore, err := os.ReadFile(store.fixture.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			service := claude.LiveService + "-" + testutil.Sha8(testutil.ExportSpelling(source))
			opened := 0
			command := store.accounts(nil, &bytes.Buffer{})
			command.Keychain = func() secret.Reader {
				opened++
				items := map[string][]byte{}
				if tt.listed[min(opened, 2)-1] {
					items[service] = []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-item","expiresAt":1}}`)
				}
				keychain := unlockedKeychain(items)
				if tt.locked {
					keychain.state = secret.KeychainStateLocked
				}
				return keychain
			}
			var out bytes.Buffer
			err = command.Relocate(t.Context(), cli.ClaudeAccountsRelocateOptions{ID: testutil.Acct, Yes: true}, TerminalPrompt{Out: &out}, nil)
			if strings.Contains(out.String()+fmt.Sprint(err), "sk-ant-") {
				t.Fatal("credential leaked into the report")
			}
			refused := err != nil && strings.Contains(err.Error(), "lives in its keychain item")
			if refused != tt.wantRefusal {
				t.Fatalf("refused = %v, want %v: %v", refused, tt.wantRefusal, err)
			}
			if tt.wantMoved {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(target, secret.CredentialsFile)); err != nil {
					t.Fatalf("the credential did not move: %v", err)
				}
				if opened != 2 {
					t.Errorf("the keychain was opened %d times, want one fresh reader per check", opened)
				}
				return
			}
			if code := errs.ExitCode(err); code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			for _, part := range []string{"`" + source + "`'s credential lives in its keychain item `" + service + "`", "nothing was moved", "`agentctl claude use " + testutil.Acct + "`", "`/login`"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("refusal %q does not contain %q", err, part)
				}
			}
			registryAfter, err := os.ReadFile(store.fixture.ConfigFile())
			if err != nil || !bytes.Equal(registryBefore, registryAfter) {
				t.Errorf("a refused relocate changed the registry: %v", err)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused relocate created the target: %v", err)
			}
			if !tt.noSource {
				if data, err := os.ReadFile(filepath.Join(source, secret.CredentialsFile)); err != nil || string(data) != blob {
					t.Errorf("a refused relocate changed the source: %v", err)
				}
			}
		})
	}
}
