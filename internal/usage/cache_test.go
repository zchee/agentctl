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

package usage

import (
	"bytes"
	"encoding/json/jsontext"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
)

const (
	testAcct = "11111111-2222-3333-4444-555555555555"
	testOrg  = "66666666-7777-8888-9999-000000000000"
)

// storeDir builds a real store tree under a temporary directory.
func storeDir(t *testing.T) *config.Paths {
	t.Helper()
	paths := config.NewPaths(t.TempDir())
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("the store directories should be creatable: %v", err)
	}
	return paths
}

func cacheBody() jsontext.Value {
	return jsontext.Value(`{"limits":[{"kind":"session","percent":21,"is_active":false}]}`)
}

func TestCachePathIsTheAccountAndOrganizationUnderTheCacheDir(t *testing.T) {
	t.Parallel()

	paths := storeDir(t)
	path := CachePath(paths, testAcct, testOrg)
	if got, want := filepath.Dir(path), paths.CacheDir(); got != want {
		t.Errorf("parent = %q, want %q", got, want)
	}
	name := filepath.Base(path)
	if prefix := testAcct + "." + testOrg + "."; !strings.HasPrefix(name, prefix) {
		t.Errorf("the readable half should be intact: %q", name)
	}
	if !strings.HasSuffix(name, ".json") {
		t.Errorf("name = %q, want a .json suffix", name)
	}
}

func TestCachePathNamesAFileInsideTheCacheDirForAnyIdentifierAtAll(t *testing.T) {
	t.Parallel()

	// Identifiers that are not path segments must still name a file, and
	// that file must be directly inside the cache directory — a ".." or a
	// "/" that survived into the name would be an escape.
	paths := storeDir(t)
	awkward := []string{"..", ".", "", "a/b", "../../etc/passwd", "Claude Code-credentials-6cdd6b98"}
	for _, value := range awkward {
		for _, pair := range [][2]string{{value, testOrg}, {testAcct, value}} {
			path := CachePath(paths, pair[0], pair[1])
			if got, want := filepath.Dir(path), paths.CacheDir(); got != want {
				t.Errorf("%q escaped the cache directory: %q", value, path)
			}
			name := filepath.Base(path)
			if strings.ContainsRune(name, '/') {
				t.Errorf("name contains a separator: %q", name)
			}
			if !strings.HasSuffix(name, ".json") {
				t.Errorf("name = %q, want a .json suffix", name)
			}
			// And it is writable, which is the point of naming it at all.
			if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
				t.Errorf("%q should be writable: %v", path, err)
			}
		}
	}
}

func TestTwoIdentifiersThatSanitizeAlikeStillGetTheirOwnEntry(t *testing.T) {
	t.Parallel()

	// The readable half is lossy on purpose, so the digest is what keeps
	// two rows from reading each other's usage figures.
	paths := storeDir(t)
	first := CachePath(paths, "a b", testOrg)
	second := CachePath(paths, "a/b", testOrg)
	if first == second {
		t.Errorf("the digest should separate them: %q", first)
	}
}

func TestCacheEntryKeyedByBothIdentifiersAndTheDirectory(t *testing.T) {
	t.Parallel()

	// Two rows share a cache entry exactly when they share BOTH ids, and
	// the provider's directory is part of the path, so two providers never
	// collide on one identity.
	paths := storeDir(t)
	claude, codex := paths.CacheDir(), paths.CodexCacheDir()
	if got, want := CachePathIn(codex, "user-a", "acct-1"), CachePathIn(codex, "user-a", "acct-1"); got != want {
		t.Errorf("one identity, one entry: %q != %q", got, want)
	}
	if got := CachePathIn(codex, "user-a", "acct-1"); got == CachePathIn(codex, "user-a", "acct-2") {
		t.Errorf("one user, two accounts must not share: %q", got)
	}
	if got := CachePathIn(codex, "user-a", "acct-1"); got == CachePathIn(codex, "user-b", "acct-1") {
		t.Errorf("two users, one account must not share: %q", got)
	}
	if got := CachePathIn(codex, "user-a", "acct-1"); got == CachePathIn(claude, "user-a", "acct-1") {
		t.Errorf("two providers, one identity must not share: %q", got)
	}
}

func TestAStoredEntryRoundTrips(t *testing.T) {
	t.Parallel()

	paths := storeDir(t)
	path := CachePath(paths, testAcct, testOrg)
	entry := NewCacheEntry(1_757_000_000_000, cacheBody())

	if err := StoreCache(t.Context(), path, &entry); err != nil {
		t.Fatalf("the cache entry should be writable: %v", err)
	}
	loaded := LoadCache(t.Context(), path)
	if loaded == nil {
		t.Fatal("the entry just written should load")
	}
	if diff := gocmp.Diff(entry, *loaded); diff != "" {
		t.Errorf("entry mismatch (-want +got):\n%s", diff)
	}
	if !bytes.Equal(loaded.Body, cacheBody()) {
		t.Errorf("body = %s, want %s", loaded.Body, cacheBody())
	}
}

func TestAStoredEntryIsARegularFileAt0600WithNoTemporaryLeftBehind(t *testing.T) {
	t.Parallel()

	paths := storeDir(t)
	path := CachePath(paths, testAcct, testOrg)
	entry := NewCacheEntry(0, cacheBody())
	if err := StoreCache(t.Context(), path, &entry); err != nil {
		t.Fatalf("writable: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the entry should exist: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Error("the entry should be a regular file")
	}
	if got := info.Mode().Perm(); got != config.FileMode {
		t.Errorf("mode = %o, want %o", got, config.FileMode)
	}

	entries, err := os.ReadDir(paths.CacheDir())
	if err != nil {
		t.Fatalf("the cache directory should be readable: %v", err)
	}
	for _, dirent := range entries {
		if strings.Contains(dirent.Name(), ".tmp.") {
			t.Errorf("temporary file left behind: %q", dirent.Name())
		}
	}
}

func TestARewriteReplacesTheFileAtomically(t *testing.T) {
	t.Parallel()

	paths := storeDir(t)
	path := CachePath(paths, testAcct, testOrg)

	first := NewCacheEntry(1, cacheBody())
	if err := StoreCache(t.Context(), path, &first); err != nil {
		t.Fatalf("writable: %v", err)
	}
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("present: %v", err)
	}

	second := NewCacheEntry(2, cacheBody())
	if err := StoreCache(t.Context(), path, &second); err != nil {
		t.Fatalf("writable: %v", err)
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("present: %v", err)
	}

	// A new inode is the observable signature of tmp-then-rename; a
	// truncate in place would keep the old one and expose a half-written
	// window.
	firstStat, firstOK := firstInfo.Sys().(*syscall.Stat_t)
	secondStat, secondOK := secondInfo.Sys().(*syscall.Stat_t)
	if !firstOK || !secondOK {
		t.Fatal("stat should expose the inode on this platform")
	}
	if firstStat.Ino == secondStat.Ino {
		t.Error("the rewrite kept the inode; the file was truncated in place")
	}
	if got := secondInfo.Mode().Perm(); got != config.FileMode {
		t.Errorf("mode = %o, want %o", got, config.FileMode)
	}
	loaded := LoadCache(t.Context(), path)
	if loaded == nil || loaded.FetchedAtMs != 2 {
		t.Errorf("loaded = %+v, want FetchedAtMs 2", loaded)
	}
}

func TestFreshnessEndsExactlyAtTheTTL(t *testing.T) {
	t.Parallel()

	entry := NewCacheEntry(1_000_000, cacheBody())
	ttlMs := CacheTTL.Milliseconds()

	tests := map[string]struct {
		nowMs int64
		want  bool
	}{
		"success: an entry fetched now is fresh":         {nowMs: 1_000_000, want: true},
		"success: one millisecond short of the TTL":      {nowMs: 1_000_000 + ttlMs - 1, want: true},
		"success: exactly at the TTL is stale":           {nowMs: 1_000_000 + ttlMs, want: false},
		"success: long past the TTL is stale":            {nowMs: 1_000_000 + ttlMs*10, want: false},
		"success: an entry from the future is not fresh": {nowMs: 500_000, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := entry.IsFresh(tt.nowMs, CacheTTL); got != tt.want {
				t.Errorf("IsFresh(%d) = %v, want %v", tt.nowMs, got, tt.want)
			}
		})
	}
}

func TestRateLimitRemainingCountsDownAndThenClears(t *testing.T) {
	t.Parallel()

	entry := NewCacheEntry(0, cacheBody())
	entry.RateLimitedUntilMs = new(int64)
	*entry.RateLimitedUntilMs = 30_000

	tests := map[string]struct {
		nowMs   int64
		want    int64
		wantSet bool
	}{
		"success: the full window remains":               {nowMs: 0, want: 30, wantSet: true},
		"success: a part second still counts as waiting": {nowMs: 29_500, want: 1, wantSet: true},
		"success: the boundary clears the wait":          {nowMs: 30_000, wantSet: false},
		"success: long past the window there is no wait": {nowMs: 60_000, wantSet: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := entry.RateLimitedFor(tt.nowMs)
			if ok != tt.wantSet {
				t.Fatalf("RateLimitedFor(%d) ok = %v, want %v", tt.nowMs, ok, tt.wantSet)
			}
			if ok && got != tt.want {
				t.Errorf("RateLimitedFor(%d) = %d, want %d", tt.nowMs, got, tt.want)
			}
		})
	}

	cleared := NewCacheEntry(0, cacheBody())
	if _, ok := cleared.RateLimitedFor(0); ok {
		t.Error("an entry without a window must report no wait")
	}
}

func TestAnUnreadableEntryLoadsAsNilRatherThanFailing(t *testing.T) {
	t.Parallel()

	paths := storeDir(t)
	path := CachePath(paths, testAcct, testOrg)

	if got := LoadCache(t.Context(), path); got != nil {
		t.Errorf("an absent file loaded: %+v", got)
	}

	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("writable: %v", err)
	}
	if got := LoadCache(t.Context(), path); got != nil {
		t.Errorf("a corrupt file loaded: %+v", got)
	}

	if err := os.WriteFile(path, []byte(`{"version":99,"fetched_at_ms":0,"body":{}}`), 0o600); err != nil {
		t.Fatalf("writable: %v", err)
	}
	if got := LoadCache(t.Context(), path); got != nil {
		t.Errorf("an entry from a future build loaded: %+v", got)
	}

	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), MaxCacheEntryBytes+1), 0o600); err != nil {
		t.Fatalf("writable: %v", err)
	}
	if got := LoadCache(t.Context(), path); got != nil {
		t.Errorf("an oversized file loaded: %+v", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("removable: %v", err)
	}
	if err := os.Mkdir(path, fs.FileMode(0o700)); err != nil {
		t.Fatalf("a directory can take the entry's place: %v", err)
	}
	if got := LoadCache(t.Context(), path); got != nil {
		t.Errorf("a directory where the entry should be loaded: %+v", got)
	}
}

func TestAStaleEntryStillLoadsSoItCanBeRenderedStale(t *testing.T) {
	t.Parallel()

	// The whole point of stale-while-error: LoadCache must not apply the
	// TTL, only IsFresh does, so a failed fetch can still show yesterday's
	// numbers next to a stale badge.
	paths := storeDir(t)
	path := CachePath(paths, testAcct, testOrg)
	entry := NewCacheEntry(0, cacheBody())
	if err := StoreCache(t.Context(), path, &entry); err != nil {
		t.Fatalf("writable: %v", err)
	}

	loaded := LoadCache(t.Context(), path)
	if loaded == nil {
		t.Fatal("a stale entry should still load")
	}
	if loaded.IsFresh(1<<40, CacheTTL) {
		t.Error("an ancient entry reported fresh")
	}
	if !bytes.Equal(loaded.Body, cacheBody()) {
		t.Errorf("body = %s, want %s", loaded.Body, cacheBody())
	}
}

func TestStoredBytesKeepTheSerializedFieldOrder(t *testing.T) {
	t.Parallel()

	// Entries written before this build carry version, fetched_at_ms,
	// rate_limited_until_ms (null when absent), body, in that order; a
	// store that reordered or dropped the null would make the same entry
	// read differently across builds.
	paths := storeDir(t)
	path := CachePath(paths, testAcct, testOrg)
	entry := NewCacheEntry(7, jsontext.Value(`{"a":1}`))
	if err := StoreCache(t.Context(), path, &entry); err != nil {
		t.Fatalf("writable: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readable: %v", err)
	}
	want := `{"version":1,"fetched_at_ms":7,"rate_limited_until_ms":null,"body":{"a":1}}`
	if string(data) != want {
		t.Errorf("stored bytes = %s, want %s", data, want)
	}
}
