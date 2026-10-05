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
	json "encoding/json/v2"
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestSha8(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw  string
		want string
	}{
		// hex(sha256("abc")) = ba7816bf8f01cfea...
		"success: ascii input takes the digest's first eight": {raw: "abc", want: "ba7816bf"},
		// e3b0c44298fc1c14... is the digest of the empty string.
		"success: empty input hashes the empty string": {raw: "", want: "e3b0c442"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := Sha8(tt.raw); got != tt.want {
				t.Fatalf("Sha8(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSha8NormalizesBeforeHashing(t *testing.T) {
	t.Parallel()

	composed := "café"
	decomposed := "café"
	if got, want := Sha8(decomposed), Sha8(composed); got != want {
		t.Fatalf("Sha8 of the decomposed spelling = %q, composed = %q; the two spellings must hash the same", got, want)
	}
}

func TestExportSpelling(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dir  string
		want string
	}{
		"success: plain path unchanged":       {dir: "/tmp/store", want: "/tmp/store"},
		"success: trailing slash trimmed":     {dir: "/tmp/store/", want: "/tmp/store"},
		"success: many trailing slashes trim": {dir: "/tmp/store///", want: "/tmp/store"},
		"success: root keeps its only slash":  {dir: "/", want: "/"},
		"success: decomposed input composes":  {dir: "/tmp/café", want: "/tmp/café"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := ExportSpelling(tt.dir); got != tt.want {
				t.Fatalf("ExportSpelling(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestMigrationService(t *testing.T) {
	t.Parallel()

	got := MigrationService("/tmp/store")
	want := LiveService + "-" + Sha8("/tmp/store")
	if got != want {
		t.Fatalf("MigrationService = %q, want %q", got, want)
	}
}

func TestCanonicalMigrationServiceResolvesLinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := dir + "/real"
	link := dir + "/link"
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("plant link: %v", err)
	}

	got := CanonicalMigrationService(t, link)
	resolved, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read link: %v", err)
	}
	// The canonical spelling may resolve further links above the fixture,
	// so assert the property rather than the exact string: the link's own
	// spelling must not be what was hashed whenever the two differ.
	direct := MigrationService(link)
	canonicalOfTarget := CanonicalMigrationService(t, resolved)
	if got != canonicalOfTarget {
		t.Fatalf("CanonicalMigrationService(link) = %q, of the target = %q; the two must agree", got, canonicalOfTarget)
	}
	if got == direct {
		t.Fatalf("CanonicalMigrationService(link) equals the unresolved service %q, so the link was not resolved", direct)
	}
}

func TestBlobCarriesTheIdentity(t *testing.T) {
	f := New(t)

	var document struct {
		ClaudeAiOauth struct {
			AccessToken      string   `json:"accessToken"`
			RefreshToken     string   `json:"refreshToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
			TokenAccount     struct {
				UUID             string  `json:"uuid"`
				EmailAddress     string  `json:"emailAddress"`
				OrganizationUUID *string `json:"organizationUuid"`
				OrganizationName string  `json:"organizationName"`
			} `json:"tokenAccount"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(f.Blob("access-1", "refresh-1", 42)), &document); err != nil {
		t.Fatalf("Blob did not produce parseable JSON: %v", err)
	}

	oauth := document.ClaudeAiOauth
	if oauth.AccessToken != "access-1" || oauth.RefreshToken != "refresh-1" || oauth.ExpiresAt != 42 {
		t.Fatalf("Blob tokens = (%q, %q, %d), want (access-1, refresh-1, 42)", oauth.AccessToken, oauth.RefreshToken, oauth.ExpiresAt)
	}
	if diff := gocmp.Diff([]string{"user:inference", "user:profile"}, oauth.Scopes); diff != "" {
		t.Fatalf("Blob scopes mismatch (-want +got):\n%s", diff)
	}
	if oauth.TokenAccount.UUID != Acct || oauth.TokenAccount.OrganizationUUID == nil || *oauth.TokenAccount.OrganizationUUID != Org {
		t.Fatalf("Blob identity = (%q, %v), want (%q, %q)", oauth.TokenAccount.UUID, oauth.TokenAccount.OrganizationUUID, Acct, Org)
	}
}

func TestIdentifiedBlobWithoutOrganization(t *testing.T) {
	f := New(t)

	var document map[string]map[string]any
	if err := json.Unmarshal([]byte(f.IdentifiedBlob("a", "r", 1, Acct, nil)), &document); err != nil {
		t.Fatalf("IdentifiedBlob did not produce parseable JSON: %v", err)
	}
	account, ok := document["claudeAiOauth"]["tokenAccount"].(map[string]any)
	if !ok {
		t.Fatalf("tokenAccount is missing: %v", document)
	}
	value, present := account["organizationUuid"]
	if !present || value != nil {
		t.Fatalf("organizationUuid = (%v, present=%t), want an explicit null", value, present)
	}
}

func TestWriteRegistryShapesTheDocument(t *testing.T) {
	f := New(t)
	f.WriteRegistry([]any{f.OwnedRecord(Acct, Org)})

	data, err := os.ReadFile(f.ConfigFile())
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var document struct {
		Version           int              `json:"version"`
		Accounts          []map[string]any `json:"accounts"`
		ForgottenServices []any            `json:"forgotten_services"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("registry is not parseable JSON: %v", err)
	}
	if document.Version != 1 || len(document.Accounts) != 1 || document.ForgottenServices == nil {
		t.Fatalf("registry = version %d, %d accounts, forgotten %v; want version 1, one account, an empty list", document.Version, len(document.Accounts), document.ForgottenServices)
	}
}

func TestOwnedRecordNamesTheNamespace(t *testing.T) {
	f := New(t)
	record := f.OwnedRecord(Acct, Org)

	kind, ok := record["kind"].(map[string]any)
	if !ok {
		t.Fatalf("record kind is missing: %v", record)
	}
	spelling, _ := kind["export_spelling"].(string)
	sha, _ := kind["export_sha8"].(string)
	if spelling != ExportSpelling(f.NamespaceDir(Acct, Org)) {
		t.Fatalf("export_spelling = %q, want the namespace directory's spelling", spelling)
	}
	if want := Sha8(spelling); sha != want {
		t.Fatalf("export_sha8 = %q, want %q", sha, want)
	}
}

func TestWriteCredentialsSetsPrivateModes(t *testing.T) {
	f := New(t)
	path := f.WriteCredentials(Acct, Org, f.Blob("a", "r", FreshAt()))

	if got := ModeOf(t, path); got != 0o600 {
		t.Fatalf("credential file mode = %04o, want 0600", got)
	}
	if got := ModeOf(t, f.NamespaceDir(Acct, Org)); got != 0o700 {
		t.Fatalf("namespace directory mode = %04o, want 0700", got)
	}
	if diff := gocmp.Diff([]string{".credentials.json"}, f.NamespaceEntries(Acct, Org)); diff != "" {
		t.Fatalf("namespace entries mismatch (-want +got):\n%s", diff)
	}
}
