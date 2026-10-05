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

package secret

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// recordingReader is a Reader whose answers the test scripts and whose
// reads the test counts, so "no read was issued" is an assertion rather
// than a hope.
type recordingReader struct {
	// answers maps a service name to its read result; a missing entry
	// answers like an absent item.
	answers map[string]error
	// secrets maps a service name to a payload for a successful read.
	secrets map[string]*Secret
	// reads is every service name that was actually read.
	reads []string
}

func (r *recordingReader) Preflight(context.Context) KeychainStatus {
	return KeychainStatus{State: KeychainStateUnlocked}
}

func (r *recordingReader) ListServices(context.Context, string) ([]ServiceEntry, error) {
	return nil, nil
}

func (r *recordingReader) Read(_ context.Context, service string) (*Secret, error) {
	r.reads = append(r.reads, service)
	if err, scripted := r.answers[service]; scripted {
		return nil, err
	}
	if secret, scripted := r.secrets[service]; scripted {
		return secret, nil
	}
	return nil, ErrItemNotFound
}

// readableSecret returns a sealed payload for a scripted successful
// read.
func readableSecret(t *testing.T) *Secret {
	t.Helper()
	secret, err := NewSecret([]byte("a-migrated-credential"))
	if err != nil {
		t.Fatalf("NewSecret() = %v", err)
	}
	return secret
}

func TestForeignServiceNameSpelling(t *testing.T) {
	t.Parallel()

	if LiveKeychainService != "Claude Code-credentials" {
		t.Errorf("LiveKeychainService = %q; the keychain item spelling is fixed", LiveKeychainService)
	}
	if got := ForeignServiceName("0a1b2c3d"); got != "Claude Code-credentials-0a1b2c3d" {
		t.Errorf("ForeignServiceName() = %q", got)
	}
}

func TestACleanNamespaceHasNoActivity(t *testing.T) {
	t.Parallel()

	nsDir := t.TempDir()
	reader := &recordingReader{}
	got := DetectForeignActivity(t.Context(), nsDir, &OwnedMeta{ExportSHA8: "11111111"}, nil, reader)
	if got.Kind != ForeignNone {
		t.Errorf("DetectForeignActivity() = %+v, want none", got)
	}
	if len(reader.reads) != 0 {
		t.Errorf("a clean namespace issued keychain reads: %v", reader.reads)
	}
}

func TestEachLockArtefactIsDetectedByName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		plant string
	}{
		"success: the refresh lock is detected":       {plant: RefreshLockName},
		"success: the storage-write lock is detected": {plant: StorageWriteLockName},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			nsDir := t.TempDir()
			if err := os.Mkdir(filepath.Join(nsDir, tt.plant), 0o700); err != nil {
				t.Fatalf("Mkdir() = %v", err)
			}

			got := DetectForeignActivity(t.Context(), nsDir, &OwnedMeta{ExportSHA8: "11111111"}, nil, &recordingReader{})
			want := ForeignActivity{Kind: ForeignClaudeLock, LockName: tt.plant}
			if diff := gocmp.Diff(want, got, cmpopts.IgnoreFields(ForeignActivity{}, "LockAgeMS")); diff != "" {
				t.Errorf("DetectForeignActivity() mismatch (-want +got):\n%s", diff)
			}
			// Freshly planted, so the age is at most seconds, far under
			// any staleness window.
			if got.LockAgeMS > 60_000 {
				t.Errorf("LockAgeMS = %d, want a fresh artefact's age", got.LockAgeMS)
			}
		})
	}
}

func TestTheLegacyArtefactAloneIsNotAForeignLock(t *testing.T) {
	t.Parallel()

	// The unsuffixed storage-write name is this store's own earlier
	// artefact, never a peer mutex: reporting it would refuse every
	// namespace that still carries one.
	nsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(nsDir, LegacyStorageWriteArtefact), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}

	if got := DetectForeignActivity(t.Context(), nsDir, &OwnedMeta{ExportSHA8: "11111111"}, nil, &recordingReader{}); got.Kind != ForeignNone {
		t.Errorf("DetectForeignActivity() = %+v, want none", got)
	}
}

func TestTheLegacyLockBesideTheNamespaceIsDetected(t *testing.T) {
	t.Parallel()

	// The peer names the legacy lock after the namespace's resolved
	// path, so a namespace reached through a symbolic link must be
	// checked beside its target, not beside the link.
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-namespace")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	linked := filepath.Join(tmp, "linked-namespace")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}
	if err := os.Mkdir(real+LegacyLockSuffix, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}

	got := DetectForeignActivity(t.Context(), linked, &OwnedMeta{ExportSHA8: "11111111"}, nil, &recordingReader{})
	if got.Kind != ForeignClaudeLock {
		t.Fatalf("DetectForeignActivity() = %+v, want the legacy lock", got)
	}
	if want := filepath.Base(real) + LegacyLockSuffix; got.LockName != want {
		t.Errorf("LockName = %q, want %q", got.LockName, want)
	}
}

func TestALockWinsOverAMigration(t *testing.T) {
	t.Parallel()

	// A lock means a session is active right now, which is the more
	// urgent fact — and reporting it costs no keychain read.
	nsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(nsDir, RefreshLockName), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	service := ForeignServiceName("11111111")
	listing := []ServiceEntry{{Service: service}}
	reader := &recordingReader{secrets: map[string]*Secret{service: readableSecret(t)}}

	got := DetectForeignActivity(t.Context(), nsDir, &OwnedMeta{ExportSHA8: "11111111"}, listing, reader)
	if got.Kind != ForeignClaudeLock {
		t.Errorf("DetectForeignActivity() = %+v, want the lock", got)
	}
	if len(reader.reads) != 0 {
		t.Errorf("the lock answer must cost no keychain read, issued %v", reader.reads)
	}
}

func TestAMigratedNamespaceIsDetectedUnderEitherSpelling(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		owned OwnedMeta
		found string
	}{
		"success: the export spelling names the item":    {owned: OwnedMeta{ExportSHA8: "11111111"}, found: ForeignServiceName("11111111")},
		"success: the canonical spelling names the item": {owned: OwnedMeta{ExportSHA8: "22222222", CanonicalSHA8: "33333333"}, found: ForeignServiceName("33333333")},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			listing := []ServiceEntry{{Service: tt.found}}
			reader := &recordingReader{secrets: map[string]*Secret{tt.found: readableSecret(t)}}

			got := DetectForeignActivity(t.Context(), t.TempDir(), &tt.owned, listing, reader)
			want := ForeignActivity{Kind: ForeignMigratedToKeychain, Service: tt.found}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("DetectForeignActivity() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNoReadIsIssuedWhenTheListingDoesNotCarryTheService(t *testing.T) {
	t.Parallel()

	// The common case must cost zero extra subprocesses: a namespace
	// with no migration never spawns a keychain read.
	listing := []ServiceEntry{{Service: ForeignServiceName("99999999")}}
	reader := &recordingReader{}

	got := DetectForeignActivity(t.Context(), t.TempDir(), &OwnedMeta{ExportSHA8: "11111111", CanonicalSHA8: "22222222"}, listing, reader)
	if got.Kind != ForeignNone {
		t.Errorf("DetectForeignActivity() = %+v, want none", got)
	}
	if len(reader.reads) != 0 {
		t.Errorf("an unlisted service must not be read, issued %v", reader.reads)
	}
}

func TestAListedItemThatHasSinceVanishedIsNotAMigration(t *testing.T) {
	t.Parallel()

	service := ForeignServiceName("11111111")
	listing := []ServiceEntry{{Service: service}}
	reader := &recordingReader{answers: map[string]error{service: ErrItemNotFound}}

	got := DetectForeignActivity(t.Context(), t.TempDir(), &OwnedMeta{ExportSHA8: "11111111"}, listing, reader)
	if got.Kind != ForeignNone {
		t.Errorf("DetectForeignActivity() = %+v: an item gone by the time it was read owns nothing", got)
	}
}

func TestAListedItemThatCannotBeReadFailsClosed(t *testing.T) {
	t.Parallel()

	// The listing is evidence enough: an item under this namespace's
	// name exists, so writing stops. Failing closed costs a refresh;
	// failing open costs the user's session.
	service := ForeignServiceName("11111111")
	listing := []ServiceEntry{{Service: service}}
	reader := &recordingReader{answers: map[string]error{service: ErrKeychainLocked}}

	got := DetectForeignActivity(t.Context(), t.TempDir(), &OwnedMeta{ExportSHA8: "11111111"}, listing, reader)
	want := ForeignActivity{Kind: ForeignMigratedToKeychain, Service: service}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("DetectForeignActivity() mismatch (-want +got):\n%s", diff)
	}
}

func TestDetectionSurvivesANamespaceThatDoesNotExistYet(t *testing.T) {
	t.Parallel()

	nsDir := filepath.Join(t.TempDir(), "not-created-yet")
	got := DetectForeignActivity(t.Context(), nsDir, &OwnedMeta{ExportSHA8: "11111111"}, nil, &recordingReader{})
	if got.Kind != ForeignNone {
		t.Errorf("DetectForeignActivity() = %+v, want none", got)
	}
}
