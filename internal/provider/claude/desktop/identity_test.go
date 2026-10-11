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

package desktop

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// recordingFS remembers every name opened through it, so a test can prove
// the reader touches nothing in a profile but config.json.
type recordingFS struct {
	fs.FS
	mu     sync.Mutex
	opened []string
}

func (r *recordingFS) Open(name string) (fs.File, error) {
	r.mu.Lock()
	r.opened = append(r.opened, name)
	r.mu.Unlock()
	return r.FS.Open(name)
}

func TestReadDesktopIdentity(t *testing.T) {
	t.Parallel()

	const (
		registered   = "552d914f-aaaa-bbbb-cccc-000000000001"
		unregistered = "4a650b30-aaaa-bbbb-cccc-000000000002"
	)
	tests := map[string]struct {
		// config is written as config.json when non-nil.
		config *string
		// size, when set, writes a file of that many bytes instead.
		size int
		want Identity
		// wantWord is the identity word for the registered account.
		wantWord Word
	}{
		"success: absent file is fresh": {
			want: Identity{State: StateFresh}, wantWord: WordFresh,
		},
		"error: oversize file is unreadable": {
			size: MaxConfigBytes + 1, want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"success: a file of exactly the bound is read": {
			size: MaxConfigBytes, want: Identity{State: StateUnknown}, wantWord: WordUnknown,
		},
		"error: non-object JSON is unreadable": {
			config: new(`["` + registered + `"]`), want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"error: not JSON at all is unreadable": {
			config: new(`lastKnownAccountUuid=` + registered), want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"error: trailing content after the object is unreadable": {
			config: new(`{"lastKnownAccountUuid":"` + registered + `"} {}`), want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"error: duplicated identity member is unreadable": {
			config: new(`{"lastKnownAccountUuid":"` + registered + `","lastKnownAccountUuid":"` + unregistered + `"}`), want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"success: key absent is unknown": {
			config: new(`{"oauth:tokenCache":"v10ciphertext","other":{"nested":[1,2]}}`), want: Identity{State: StateUnknown}, wantWord: WordUnknown,
		},
		"success: key null is unknown": {
			config: new(`{"lastKnownAccountUuid":null}`), want: Identity{State: StateUnknown}, wantWord: WordUnknown,
		},
		"error: key a number is unreadable": {
			config: new(`{"lastKnownAccountUuid":42}`), want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"error: key an empty string is unreadable": {
			config: new(`{"lastKnownAccountUuid":""}`), want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"error: key an invalid UUID is unreadable": {
			config: new(`{"lastKnownAccountUuid":"not-a-uuid"}`), want: Identity{State: StateUnreadable}, wantWord: WordUnreadable,
		},
		"success: key a registered UUID matches": {
			config: new(`{"oauth:tokenCache":"v10x","lastKnownAccountUuid":"` + registered + `"}`), want: Identity{State: StateUUID, UUID: registered}, wantWord: WordMatches,
		},
		"success: key an upper-case registered UUID matches in lower case": {
			config: new(`{"lastKnownAccountUuid":"` + strings.ToUpper(registered) + `"}`), want: Identity{State: StateUUID, UUID: registered}, wantWord: WordMatches,
		},
		"success: key an unregistered UUID is a mismatch": {
			config: new(`{"lastKnownAccountUuid":"` + unregistered + `"}`), want: Identity{State: StateUUID, UUID: unregistered}, wantWord: WordMismatch,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// Siblings the reader must never open.
			for _, sibling := range []string{"Cookies", "Local State"} {
				if err := os.WriteFile(filepath.Join(dir, sibling), []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch {
			case tt.size > 0:
				// A valid object padded with spaces to the size.
				body := `{"a":1}`
				data := body + strings.Repeat(" ", tt.size-len(body))
				if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			case tt.config != nil:
				if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(*tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rec := &recordingFS{FS: os.DirFS(dir)}
			got := ReadIdentityFS(rec)
			t.Logf("state %s uuid %q reason %q", got.State, got.UUID, got.Reason)
			if diff := gocmp.Diff(tt.want, got, cmpopts.IgnoreFields(Identity{}, "Reason")); diff != "" {
				t.Fatalf("ReadIdentityFS mismatch (-want +got):\n%s", diff)
			}
			if got.State == StateUnreadable && got.Reason == "" {
				t.Fatal("an unreadable answer carries no reason")
			}
			if diff := gocmp.Diff([]string{ConfigFile}, rec.opened); diff != "" {
				t.Fatalf("names opened under the profile (-want +got):\n%s", diff)
			}
			if word := got.WordFor(registered); word != tt.wantWord {
				t.Fatalf("WordFor = %s, want %s", word, tt.wantWord)
			}
			// The directory form agrees with the file system form.
			if direct := ReadDesktopIdentity(dir); direct.State != got.State || direct.UUID != got.UUID {
				t.Fatalf("ReadDesktopIdentity = %+v, ReadIdentityFS = %+v", direct, got)
			}
		})
	}
}

func TestReadDesktopIdentityRefusesEscapes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		prepare func(t *testing.T, dir, outside string)
		want    ReaderState
	}{
		"error: config.json linking outside the profile is unreadable": {
			prepare: func(t *testing.T, dir, outside string) {
				if err := os.Symlink(filepath.Join(outside, "config.json"), filepath.Join(dir, ConfigFile)); err != nil {
					t.Fatal(err)
				}
			},
			want: StateUnreadable,
		},
		"error: config.json is a directory": {
			prepare: func(t *testing.T, dir, _ string) {
				if err := os.Mkdir(filepath.Join(dir, ConfigFile), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: StateUnreadable,
		},
		"error: the profile directory does not exist": {
			prepare: func(t *testing.T, dir, _ string) {
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
			},
			want: StateUnreadable,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			dir, outside := filepath.Join(base, "profile"), filepath.Join(base, "outside")
			for _, d := range []string{dir, outside} {
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(outside, "config.json"), []byte(`{"lastKnownAccountUuid":"552d914f-aaaa-bbbb-cccc-000000000001"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			tt.prepare(t, dir, outside)
			got := ReadDesktopIdentity(dir)
			t.Logf("state %s reason %q", got.State, got.Reason)
			if got.State != tt.want {
				t.Fatalf("ReadDesktopIdentity = %+v, want state %s", got, tt.want)
			}
		})
	}
}
