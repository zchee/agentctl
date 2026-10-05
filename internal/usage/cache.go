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

// The per-account usage cache: <cache_dir>/<acct>.<org>.<sha8>.json.
//
// Two jobs:
//
//   - Do not hammer an undocumented endpoint. Inside [CacheTTL] a repeated
//     status answers from disk and makes no request at all.
//   - Stale beats blank. When a fetch fails — a 429, a dead network, a
//     keychain that went away — the last good numbers are rendered with the
//     row marked stale or rate-limited. A user who can see yesterday's 35%
//     next to a rate-limited badge is better served than one who sees an
//     empty cell. [LoadCache] therefore never applies the TTL; only
//     [CacheEntry.IsFresh] does.
//
// The entry stores the untouched response body and re-parses it on load,
// rather than serializing a [UsageSnapshot]. That costs one JSON parse per
// cached row and buys three things: --raw round-trips from the cache
// exactly as it does from the network; a build that learns to read a new
// field immediately understands entries written by an older build; and
// nothing about the on-disk format leaks into the vocabulary the renderer
// uses.
//
// The file is written the same way credentials are — a temporary file,
// then a rename, mode 0600 — because it sits in the same store and a
// half-written cache entry would be indistinguishable from a corrupt one.
// It holds no token material; the mode is for consistency and because
// usage figures are still the user's business alone.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/zchee/agentctl/internal/config"
)

// CacheTTL is how long a cached response is served without asking the API
// again.
const CacheTTL = 300 * time.Second

// CacheEntryVersion is the format version of an entry. A file written by a
// future build with a higher version is ignored rather than misread.
const CacheEntryVersion = 1

// MaxCacheEntryBytes is the largest cache file this build will read.
//
// A usage body is a couple of kilobytes; the ceiling is here so a corrupt
// or hostile file cannot make a status run allocate without bound.
const MaxCacheEntryBytes = 1 << 20

// CacheEntry is one account's cached usage response.
//
// The field order is the serialization order, which existing stores
// already use.
type CacheEntry struct {
	// Version is the format version, [CacheEntryVersion].
	Version int `json:"version"`
	// FetchedAtMs is when the response was received, milliseconds since
	// the epoch.
	FetchedAtMs int64 `json:"fetched_at_ms"`
	// RateLimitedUntilMs is when a retry-after the server sent expires,
	// or nil when it sent none.
	//
	// Persisted rather than kept in memory so that a second status
	// invocation inside the window also declines to call — no retry
	// within the window, not merely no retry within the pass.
	RateLimitedUntilMs *int64 `json:"rate_limited_until_ms"`
	// Body is the untouched response body.
	Body jsontext.Value `json:"body"`
}

// NewCacheEntry builds an entry around a freshly received body.
func NewCacheEntry(fetchedAtMs int64, body jsontext.Value) CacheEntry {
	return CacheEntry{Version: CacheEntryVersion, FetchedAtMs: fetchedAtMs, Body: body}
}

// IsFresh reports whether this entry may be served without asking the API.
//
// A clock that moved backwards makes the entry look like it was fetched in
// the future; that is treated as not fresh, so the run refetches rather
// than serving an entry it cannot date.
func (e *CacheEntry) IsFresh(nowMs int64, ttl time.Duration) bool {
	age := nowMs - e.FetchedAtMs
	if subOverflowed(nowMs, e.FetchedAtMs, age) {
		return false
	}
	return age >= 0 && age < ttl.Milliseconds()
}

// RateLimitedFor reports how many seconds of a server-imposed rate limit
// are still to run. False means the account is free to call again.
func (e *CacheEntry) RateLimitedFor(nowMs int64) (int64, bool) {
	if e.RateLimitedUntilMs == nil {
		return 0, false
	}
	remaining := *e.RateLimitedUntilMs - nowMs
	if subOverflowed(*e.RateLimitedUntilMs, nowMs, remaining) || remaining <= 0 {
		return 0, false
	}
	// Round up, so a 500 ms remainder is reported as "1s to go" rather
	// than as "no wait left". remaining is at least 1 here, so the
	// rearranged ceiling cannot overflow.
	return (remaining-1)/1000 + 1, true
}

// CachePath returns the cache file for one Claude row,
// <acct>.<org>.<sha8>.json under the Claude cache directory.
func CachePath(paths *config.Paths, acct, org string) string {
	return CachePathIn(paths.CacheDir(), acct, org)
}

// CachePathIn returns the cache file for one row under an explicit cache
// directory, so every provider keys its entries the same way: the two
// identifiers reduced to characters that are safe in a file name, plus an
// eight-hex-digit digest taken over the untouched pair.
//
// Identifiers are not required to be path segments: a row keyed by a
// keychain service name holds a space, and refusing to name a file was the
// wrong answer to that — it made every pass over such a row a fresh
// request against the API for a row that cannot even be refreshed.
//
// The digest is what makes the sanitized name safe rather than merely
// pretty: two identifiers that sanitize to the same string still differ in
// the digest, so no two rows can collide on one entry and read each
// other's usage figures. The readable prefix is kept only so a human
// looking in the cache directory can tell which file is whose.
func CachePathIn(cacheDir, first, second string) string {
	digest := sha8(first + "/" + second)
	return filepath.Join(cacheDir, fmt.Sprintf("%s.%s.%s.json", fileSafe(first), fileSafe(second), digest))
}

// LoadCache reads an entry, or nil when there is nothing usable to read.
//
// Every failure — absent, oversized, corrupt, a version from the future —
// is nil. A cache is an optimisation: refusing to run because one is
// unreadable would turn a harmless stale file into an outage. The TTL is
// deliberately not applied here, so a failed fetch can still render the
// stale numbers.
func LoadCache(ctx context.Context, path string) *CacheEntry {
	if ctx.Err() != nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxCacheEntryBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entry CacheEntry
	if err := json.Unmarshal(data, &entry, jsontext.AllowDuplicateNames(true)); err != nil {
		return nil
	}
	if entry.Version != CacheEntryVersion {
		return nil
	}
	return &entry
}

// StoreCache writes an entry atomically at mode 0600.
//
// It returns the underlying error; callers treat a cache write failure as
// a warning, because the numbers were still fetched and rendered.
func StoreCache(ctx context.Context, path string, entry *CacheEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, config.DirMode); err != nil {
		return err
	}

	data, err := json.Marshal(entry, jsontext.AllowDuplicateNames(true))
	if err != nil {
		return err
	}

	// O_EXCL so a stray temporary file from a crashed run is never
	// silently reused, and an explicit mode so the process umask cannot
	// widen it — the same rule the credential store follows.
	tmp := filepath.Join(parent, "."+filepath.Base(path)+".tmp."+hex8())
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, config.FileMode)
	if err != nil {
		return err
	}
	if err := writeAndSync(file, data); err != nil {
		// The write already failed; the temporary is best-effort debris.
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// A pre-existing file keeps its own mode through a rename onto it only
	// on some filesystems, so the mode is asserted afterwards as well.
	return os.Chmod(path, config.FileMode)
}

// writeAndSync writes data, flushes it to stable storage and closes the
// file, reporting the first failure.
func writeAndSync(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		// The write error is the one worth reporting; the close is
		// cleanup on an already-failed file.
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// maxNamePart is the longest run of one identifier that reaches a cache
// file name.
//
// Only the readable half is truncated; uniqueness lives in the digest. The
// bound exists because two identifiers of unbounded length would otherwise
// build a name longer than the filesystem's limit, which is an error the
// caller could do nothing about.
const maxNamePart = 48

// fileSafe reduces one identifier to characters that are safe in a file
// name.
//
// Anything outside [A-Za-z0-9._-] becomes '-', which covers the
// separators, the shell-significant characters, and the space a keychain
// service name carries. An identifier that is empty, or that sanitizes to
// "." or "..", becomes "_": those three would otherwise build a name that
// either hides the file or does not name a file at all.
func fileSafe(value string) string {
	cleaned := make([]rune, 0, maxNamePart)
	for _, r := range value {
		if len(cleaned) == maxNamePart {
			break
		}
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			cleaned = append(cleaned, r)
		default:
			cleaned = append(cleaned, '-')
		}
	}
	safe := string(cleaned)
	if safe == "" || safe == "." || safe == ".." {
		return "_"
	}
	return safe
}

// sha8 returns the first eight hex digits of the SHA-256 of raw, taken
// over its NFC normalization so two spellings of one identifier hash
// alike.
func sha8(raw string) string {
	digest := sha256.Sum256([]byte(norm.NFC.String(raw)))
	return hex.EncodeToString(digest[:])[:8]
}

// hex8 returns eight random hex digits for a temporary file name.
func hex8() string {
	var buf [4]byte
	// rand.Read on the crypto source never fails; it panics instead, and
	// a process that cannot read randomness cannot do anything else safely
	// either.
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// subOverflowed reports whether diff = a - b wrapped around the int64
// range: that happens exactly when the operands have opposite signs and
// the result's sign differs from a's.
func subOverflowed(a, b, diff int64) bool {
	return (a^b) < 0 && (a^diff) < 0
}
