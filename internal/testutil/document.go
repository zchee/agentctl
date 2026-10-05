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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"
)

const (
	// LiveService is the keychain service a live session uses when no
	// configuration directory is set.
	LiveService = "Claude Code-credentials"

	// UnknownOrg is the organization directory name a login uses when the
	// exchange named none.
	UnknownOrg = "_unknown-org"

	// Acct is the account uuid every fixture account uses.
	Acct = "11111111-2222-3333-4444-555555555555"

	// Org is the organization uuid every fixture account uses.
	Org = "66666666-7777-8888-9999-000000000000"

	// Email is the email the fixture account is addressed by.
	Email = "owner@example.com"

	// TokenPath is the path the mock OAuth token endpoint is served under.
	TokenPath = "/v1/oauth/token"

	// UsagePath is the usage endpoint's path under the base URL.
	UsagePath = "/api/oauth/usage"

	// ProfilePath is the profile endpoint's path under the base URL.
	ProfilePath = "/api/oauth/profile"
)

// NowMS returns the current time in milliseconds since the epoch.
func NowMS() int64 {
	return time.Now().UnixMilli()
}

// ExpiredAt returns an expiry far enough in the past to be expired under
// any margin.
func ExpiredAt() int64 {
	return NowMS() - 60_000
}

// FreshAt returns an expiry far enough ahead to be fresh under the
// five-minute margin.
func FreshAt() int64 {
	return NowMS() + 3_600_000
}

// mustJSON marshals document compactly with deterministic member order.
func mustJSON(tb testing.TB, document any) []byte {
	tb.Helper()
	data, err := json.Marshal(document, json.Deterministic(true))
	if err != nil {
		tb.Fatalf("marshal document: %v", err)
	}
	return data
}

// mustJSONIndent marshals document with two-space indentation and
// deterministic member order.
func mustJSONIndent(tb testing.TB, document any) []byte {
	tb.Helper()
	data, err := json.Marshal(document, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		tb.Fatalf("marshal document: %v", err)
	}
	return data
}

// Blob returns a credential document in the live store's shape, carrying
// the fixture identity.
func (f *Fixture) Blob(access, refresh string, expiresAtMS int64) string {
	f.tb.Helper()
	return f.IdentifiedBlob(access, refresh, expiresAtMS, Acct, Org)
}

// IdentifiedBlob returns a credential document naming a specific account
// and organization. org may be a string, or nil when the credential names
// no organization.
func (f *Fixture) IdentifiedBlob(access, refresh string, expiresAtMS int64, acct string, org any) string {
	f.tb.Helper()
	document := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      access,
			"refreshToken":     refresh,
			"expiresAt":        expiresAtMS,
			"scopes":           []string{"user:inference", "user:profile"},
			"subscriptionType": "max",
			"tokenAccount": map[string]any{
				"uuid":             acct,
				"emailAddress":     Email,
				"organizationUuid": org,
				"organizationName": "Acme",
			},
		},
	}
	return string(mustJSON(f.tb, document))
}

// WriteRegistry writes a registry holding exactly these accounts.
func (f *Fixture) WriteRegistry(accounts []any) *Fixture {
	f.tb.Helper()
	return f.WriteRegistryDocument(map[string]any{
		"version":            1,
		"accounts":           accounts,
		"forgotten_services": []any{},
	})
}

// WriteRegistryDocument writes a whole registry document, for the cases
// that need a member the builders do not set.
func (f *Fixture) WriteRegistryDocument(document any) *Fixture {
	f.tb.Helper()
	if err := os.WriteFile(f.ConfigFile(), mustJSONIndent(f.tb, document), 0o644); err != nil {
		f.tb.Fatalf("write registry: %v", err)
	}
	return f
}

// OwnedRecord returns an owned registry record for a namespace this store
// holds.
func (f *Fixture) OwnedRecord(acct, org string) map[string]any {
	f.tb.Helper()
	spelling := ExportSpelling(f.NamespaceDir(acct, org))
	return map[string]any{
		"account_uuid":      acct,
		"organization_uuid": org,
		"email":             Email,
		"org_name":          "Acme",
		"label":             nil,
		"kind": map[string]any{
			"kind":            "owned",
			"export_spelling": spelling,
			"export_sha8":     Sha8(spelling),
		},
		"forgotten":  false,
		"created_at": "2026-09-08T00:00:00Z",
	}
}

// ConfigDirRecord returns a read-only registry record naming one keychain
// service.
func (f *Fixture) ConfigDirRecord(acct, org, service string) map[string]any {
	f.tb.Helper()
	return map[string]any{
		"account_uuid":      acct,
		"organization_uuid": org,
		"email":             "read-only@example.com",
		"org_name":          nil,
		"label":             nil,
		"kind": map[string]any{
			"kind":            "config_dir_read_only",
			"dir":             "",
			"service":         service,
			"shares_live_dir": false,
		},
		"forgotten":  false,
		"created_at": "2026-09-08T00:00:00Z",
	}
}

// WriteCredentials writes .credentials.json into a namespace, mode 0600
// inside 0700 directories, and returns the file's path.
func (f *Fixture) WriteCredentials(acct, org, blob string) string {
	f.tb.Helper()
	nsDir := f.NamespaceDir(acct, org)
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		f.tb.Fatalf("create namespace: %v", err)
	}
	if err := os.Chmod(nsDir, 0o700); err != nil {
		f.tb.Fatalf("set namespace mode: %v", err)
	}
	path := filepath.Join(nsDir, ".credentials.json")
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		f.tb.Fatalf("write credentials: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		f.tb.Fatalf("set credential mode: %v", err)
	}
	return path
}

// NamespaceEntries returns everything in a namespace directory, sorted by
// name. An unreadable namespace is reported as empty, which is what a
// caller asserting nothing was left behind wants anyway.
func (f *Fixture) NamespaceEntries(acct, org string) []string {
	entries, err := os.ReadDir(f.NamespaceDir(acct, org))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// Sha8 returns hex(sha256(nfc(raw)))[0:8], the way a live session names a
// keychain item.
func Sha8(raw string) string {
	normalized := norm.NFC.String(raw)
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:])[:8]
}

// ExportSpelling returns how a directory is spelled for naming purposes:
// NFC, no trailing slash.
func ExportSpelling(dir string) string {
	text := norm.NFC.String(dir)
	trimmed := strings.TrimRight(text, "/")
	if trimmed == "" {
		return text
	}
	return trimmed
}

// MigrationService returns the keychain service a live session would
// migrate nsDir to.
func MigrationService(nsDir string) string {
	return LiveService + "-" + Sha8(ExportSpelling(nsDir))
}

// CanonicalMigrationService returns the same service name for the
// directory's canonical spelling.
//
// A second name for one directory, and the reason discovery looks for
// both: a user handing a path through a symbolic link makes the session
// hash the spelling it was given, so the canonical spelling hashes
// differently. Under the temporary directory on macOS the two always
// differ, because /var is a link to /private/var.
func CanonicalMigrationService(tb testing.TB, nsDir string) string {
	tb.Helper()
	resolved, err := filepath.EvalSymlinks(nsDir)
	if err != nil {
		tb.Fatalf("resolve %q: %v", nsDir, err)
	}
	return LiveService + "-" + Sha8(ExportSpelling(resolved))
}
