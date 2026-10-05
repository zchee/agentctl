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
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
)

// tokenDoc is a credential document whose token leaves sit under "tokens",
// with no expiry recorded in the meta.
type tokenDoc struct {
	// requiresExpiry mirrors the store whose metas always carried one.
	requiresExpiry bool
	// strictUnusable refuses rather than absorbs an unopenable file.
	strictUnusable bool
}

func (d tokenDoc) MetaRequiresExpiry() bool { return d.requiresExpiry }

func (d tokenDoc) UnusableIsAbsent() bool { return !d.strictUnusable }

func (d tokenDoc) Validate(b []byte) bool {
	_, ok := d.Digests(b)
	return ok
}

func (tokenDoc) Digests(b []byte) (Digests, bool) {
	var doc struct {
		Tokens *struct {
			AccessToken  *string `json:"access_token"`
			RefreshToken *string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.Tokens == nil || doc.Tokens.AccessToken == nil {
		return Digests{}, false
	}
	digests := Digests{AccessSHA256: hexSHA256(*doc.Tokens.AccessToken)}
	if doc.Tokens.RefreshToken != nil {
		digests.RefreshSHA256 = hexSHA256(*doc.Tokens.RefreshToken)
	}
	return digests, true
}

func hexSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func tokenDocJSON(access, refresh string) string {
	return fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":"%s","refresh_token":"%s","account_id":"acct-123"},"last_refresh":"2026-09-16T00:00:00Z"}`, access, refresh)
}

func docDigests(t *testing.T, doc string) *Digests {
	t.Helper()
	digests, ok := tokenDoc{}.Digests([]byte(doc))
	if !ok {
		t.Fatalf("the document should parse: %s", doc)
	}
	return &digests
}

func put(t *testing.T, store *secretFileStore, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store.codexNS, name), []byte(body), 0o600); err != nil {
		t.Fatalf("`%s` should be writable: %v", name, err)
	}
}

func metaBody(t *testing.T, prior *Digests) string {
	t.Helper()
	body, err := metaJSON(testSpec(prior))
	if err != nil {
		t.Fatalf("a meta serializes: %v", err)
	}
	return string(body)
}

func resolveDoc(t *testing.T, store *secretFileStore, takenOver bool, cred PendingCredential) (PendingDecision, *PendingWrite) {
	t.Helper()
	decision, write, err := ResolvePendingWith(store.codexFD, store.codexNS, testSpec(nil), takenOver, cred)
	if err != nil {
		t.Fatalf("the case should be decidable: %v", err)
	}
	return decision, write
}

func readMaybe(store *secretFileStore, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(store.codexNS, name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func TestPendingAnUnchangedFileReplaysAndTheRotatedRefreshTokenIsNeverLost(t *testing.T) {
	// The pending file is produced the way the writer produces it: an
	// injected rename failure on a StopComplete write after a refresh
	// answered.
	store := newSecretFileStore(t)
	file := store.codexFile()
	old := tokenDocJSON("at-old", "rt-old")
	if _, err := file.Write(t.Context(), []byte(old), nil, StopComplete); err != nil {
		t.Fatalf("the first write should land: %v", err)
	}

	rotated := tokenDocJSON("at-new", "rt-rotated")
	prior := docDigests(t, old)
	file.faults = &writeFaults{renameErr: errors.New("rename failure injected by the test seam")}
	outcome, err := file.Write(t.Context(), []byte(rotated), testSpec(prior), StopComplete)
	if err != nil || !outcome.SavedToPending {
		t.Fatalf("parked, not failed: %+v, %v", outcome, err)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != old {
		t.Fatalf("the old file is still live, got %q", got)
	}

	decision, write := resolveDoc(t, store, false, tokenDoc{})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingReplayed}, decision); diff != "" {
		t.Errorf("decision mismatch (-want +got):\n%s", diff)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != rotated {
		t.Errorf("the replayed file carries the rotated grant, got %q", got)
	}
	if got := docDigests(t, readText(t, filepath.Join(store.codexNS, testAuth))).RefreshSHA256; got != hexSHA256("rt-rotated") {
		t.Errorf("the rotated refresh token survives the replay, got %s", got)
	}
	want := &PendingWrite{Before: prior, Pending: docDigests(t, rotated)}
	if diff := gocmp.Diff(want, write); diff != "" {
		t.Errorf("the resolution reports what it replaced (-want +got):\n%s", diff)
	}
	for _, name := range []string{testAuthPending, testAuthMeta} {
		if _, present := readMaybe(store, name); present {
			t.Errorf("`%s` must be cleared after the replay", name)
		}
	}
	if mode := modeOf(t, filepath.Join(store.codexNS, testAuth)); mode != 0o600 {
		t.Errorf("the replayed file is owner-only: %04o", mode)
	}
}

func TestPendingAFileReplacedByAnotherCredentialDiscardsBothPendingFiles(t *testing.T) {
	store := newSecretFileStore(t)
	old := tokenDocJSON("at-old", "rt-old")
	external := tokenDocJSON("at-external", "rt-external")
	put(t, store, testAuth, external)
	put(t, store, testAuthPending, tokenDocJSON("at-new", "rt-new"))
	put(t, store, testAuthMeta, metaBody(t, docDigests(t, old)))

	decision, write := resolveDoc(t, store, false, tokenDoc{})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingDiscarded, Reason: PendingFileChanged}, decision); diff != "" {
		t.Errorf("decision mismatch (-want +got):\n%s", diff)
	}
	if got := PendingFileChanged.Label(); got != "file changed" {
		t.Errorf("label = %q", got)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != external {
		t.Errorf("the newer grant is kept, got %q", got)
	}
	for _, name := range []string{testAuthPending, testAuthMeta} {
		if _, present := readMaybe(store, name); present {
			t.Errorf("`%s` must be removed", name)
		}
	}
	if diff := gocmp.Diff(docDigests(t, external), write.Before); diff != "" {
		t.Errorf("the write names what was on disk (-want +got):\n%s", diff)
	}
}

func TestPendingAMissingOrUnparseableMetaOrPendingIsInvalid(t *testing.T) {
	tests := map[string]struct {
		meta    *string
		pending string
	}{
		"error: meta missing":             {meta: nil, pending: tokenDocJSON("at-new", "rt-new")},
		"error: meta not JSON":            {meta: new("{not json"), pending: tokenDocJSON("at-new", "rt-new")},
		"error: meta without created_at":  {meta: new("{}"), pending: tokenDocJSON("at-new", "rt-new")},
		"error: pending not a credential": {meta: ptrOf(t, nil), pending: `{"tokens":{}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			put(t, store, testAuth, tokenDocJSON("at-old", "rt-old"))
			put(t, store, testAuthPending, tt.pending)
			if tt.meta != nil {
				put(t, store, testAuthMeta, *tt.meta)
			}

			decision, _ := resolveDoc(t, store, false, tokenDoc{})
			if diff := gocmp.Diff(PendingDecision{Kind: PendingDiscarded, Reason: PendingInvalid}, decision); diff != "" {
				t.Errorf("decision mismatch (-want +got):\n%s", diff)
			}
			if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != tokenDocJSON("at-old", "rt-old") {
				t.Errorf("the file is untouched, got %q", got)
			}
			for _, leftover := range []string{testAuthPending, testAuthMeta} {
				if _, present := readMaybe(store, leftover); present {
					t.Errorf("`%s` must be removed", leftover)
				}
			}
		})
	}
}

//go:fix inline
func ptr(s string) *string { return new(s) }

// ptrOf builds a valid meta body for the row that corrupts the pending file
// rather than the meta.
func ptrOf(t *testing.T, prior *Digests) *string {
	t.Helper()
	body := metaBody(t, prior)
	return &body
}

func TestPendingASymlinkedPendingFileIsInvalidAndItsTargetIsUntouched(t *testing.T) {
	store := newSecretFileStore(t)
	elsewhere := filepath.Join(store.configDir, "elsewhere.json")
	planted := tokenDocJSON("at-planted", "rt-planted")
	if err := os.WriteFile(elsewhere, []byte(planted), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := tokenDocJSON("at-old", "rt-old")
	put(t, store, testAuth, old)
	if err := os.Symlink(elsewhere, filepath.Join(store.codexNS, testAuthPending)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	put(t, store, testAuthMeta, metaBody(t, docDigests(t, old)))

	decision, _ := resolveDoc(t, store, false, tokenDoc{})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingDiscarded, Reason: PendingInvalid}, decision); diff != "" {
		t.Errorf("decision mismatch (-want +got):\n%s", diff)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != old {
		t.Errorf("nothing was replayed through the link, got %q", got)
	}
	if _, err := os.Lstat(filepath.Join(store.codexNS, testAuthPending)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link itself is removed, lstat err = %v", err)
	}
	if got := readText(t, elsewhere); got != planted {
		t.Errorf("not its target, got %q", got)
	}
}

func TestPendingAnAbsentFileReplaysAFirstWriteAndDiscardsADerivedOne(t *testing.T) {
	store := newSecretFileStore(t)
	first := tokenDocJSON("at-first", "rt-first")
	put(t, store, testAuthPending, first)
	put(t, store, testAuthMeta, metaBody(t, nil))
	decision, write := resolveDoc(t, store, false, tokenDoc{})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingReplayed, FirstWrite: true}, decision); diff != "" {
		t.Errorf("a first write replays (-want +got):\n%s", diff)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != first {
		t.Errorf("the first write landed, got %q", got)
	}
	if diff := gocmp.Diff(&PendingWrite{Pending: docDigests(t, first)}, write); diff != "" {
		t.Errorf("write mismatch (-want +got):\n%s", diff)
	}

	derived := newSecretFileStore(t)
	put(t, derived, testAuthPending, tokenDocJSON("at-new", "rt-new"))
	put(t, derived, testAuthMeta, metaBody(t, docDigests(t, tokenDocJSON("at-old", "rt-old"))))
	decision, _ = resolveDoc(t, derived, false, tokenDoc{})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingDiscarded, Reason: PendingFileRemoved}, decision); diff != "" {
		t.Errorf("a derived pending is discarded (-want +got):\n%s", diff)
	}
	if _, present := readMaybe(derived, testAuth); present {
		t.Errorf("the discard leaves needs-login, not a guessed credential")
	}
}

func TestPendingNothingPendingClearsALoneMetaAndReportsNoWrite(t *testing.T) {
	store := newSecretFileStore(t)
	decision, write := resolveDoc(t, store, false, tokenDoc{})
	if decision.Kind != PendingNone || write != nil {
		t.Fatalf("nothing pending: %+v, %+v", decision, write)
	}

	put(t, store, testAuthMeta, metaBody(t, nil))
	decision, write = resolveDoc(t, store, false, tokenDoc{})
	if decision.Kind != PendingNone || write != nil {
		t.Fatalf("a lone meta is still nothing pending: %+v, %+v", decision, write)
	}
	if _, present := readMaybe(store, testAuthMeta); present {
		t.Errorf("the crash-window meta is removed")
	}
}

func TestPendingATakenOverNamespaceDiscardsAfterValidityAndBeforeTheComparison(t *testing.T) {
	store := newSecretFileStore(t)
	old := tokenDocJSON("at-old", "rt-old")
	put(t, store, testAuth, old)
	put(t, store, testAuthPending, tokenDocJSON("at-new", "rt-new"))
	put(t, store, testAuthMeta, metaBody(t, docDigests(t, old)))
	decision, _ := resolveDoc(t, store, true, tokenDoc{})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingDiscarded, Reason: PendingNamespaceTakenOver}, decision); diff != "" {
		t.Errorf("decision mismatch (-want +got):\n%s", diff)
	}
	if got := PendingNamespaceTakenOver.Label(); got != "namespace taken over" {
		t.Errorf("label = %q", got)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != old {
		t.Errorf("the namespace's own file is left alone, got %q", got)
	}

	// Validity is decided first, exactly as the table orders it.
	invalid := newSecretFileStore(t)
	put(t, invalid, testAuthPending, tokenDocJSON("at-new", "rt-new"))
	decision, _ = resolveDoc(t, invalid, true, tokenDoc{})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingDiscarded, Reason: PendingInvalid}, decision); diff != "" {
		t.Errorf("validity first (-want +got):\n%s", diff)
	}
}

func TestPendingTheExpiryRequirementIsTheCredentialsChoice(t *testing.T) {
	tests := map[string]struct {
		requires bool
		want     PendingDecision
	}{
		"success: no expiry required replays a first write": {
			requires: false,
			want:     PendingDecision{Kind: PendingReplayed, FirstWrite: true},
		},
		"error: a required expiry stays invalid without one": {
			requires: true,
			want:     PendingDecision{Kind: PendingDiscarded, Reason: PendingInvalid},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			put(t, store, testAuthPending, tokenDocJSON("at-first", "rt-first"))
			put(t, store, testAuthMeta, metaBody(t, nil))
			decision, _ := resolveDoc(t, store, false, tokenDoc{requiresExpiry: tt.requires})
			if diff := gocmp.Diff(tt.want, decision); diff != "" {
				t.Errorf("decision mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPendingAnUnreadableTargetIsAnErrorNotADecision(t *testing.T) {
	// A directory or a symbolic link at the target is not something a
	// replay may overwrite, and it is not an absence either: the
	// resolution stops with an error and keeps the pending file.
	old := tokenDocJSON("at-old", "rt-old")
	tests := map[string]struct {
		plant   func(t *testing.T, store *secretFileStore)
		wantErr func(error) bool
	}{
		"error: a directory at the target": {
			plant: func(t *testing.T, store *secretFileStore) {
				if err := os.Mkdir(filepath.Join(store.codexNS, testAuth), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantErr: isErrorType[*NotRegularError],
		},
		"error: a symbolic link at the target": {
			plant: func(t *testing.T, store *secretFileStore) {
				elsewhere := filepath.Join(store.configDir, "elsewhere.json")
				if err := os.WriteFile(elsewhere, []byte(old), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				if err := os.Symlink(elsewhere, filepath.Join(store.codexNS, testAuth)); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantErr: isErrorType[*SymlinkRefusedError],
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			tt.plant(t, store)
			put(t, store, testAuthPending, tokenDocJSON("at-new", "rt-new"))
			put(t, store, testAuthMeta, metaBody(t, docDigests(t, old)))

			_, _, err := ResolvePendingWith(store.codexFD, store.codexNS, testSpec(nil), false, tokenDoc{})
			if !tt.wantErr(err) {
				t.Fatalf("the target is never overwritten by a replay, got %v", err)
			}
			for _, kept := range []string{testAuthPending, testAuthMeta} {
				if _, present := readMaybe(store, kept); !present {
					t.Errorf("`%s` is kept for a later run", kept)
				}
			}
		})
	}
}

func TestPendingAnUnopenableFileKeepsEverythingUnderTheStrictRuleOnly(t *testing.T) {
	// A mode-0000 target, pending file or meta: under the strict rule the
	// resolution is an error and all three files stay; under the lenient
	// rule an unopenable target is absent, so a derived pending file is
	// discarded as file removed.
	if os.Getuid() == 0 {
		t.Skip("mode-0000 files are readable by root")
	}
	old := tokenDocJSON("at-old", "rt-old")
	for _, unopenable := range []string{testAuth, testAuthPending, testAuthMeta} {
		store := newSecretFileStore(t)
		put(t, store, testAuth, old)
		put(t, store, testAuthPending, tokenDocJSON("at-new", "rt-new"))
		put(t, store, testAuthMeta, metaBody(t, docDigests(t, old)))
		if err := os.Chmod(filepath.Join(store.codexNS, unopenable), 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		_, _, err := ResolvePendingWith(store.codexFD, store.codexNS, testSpec(nil), false, tokenDoc{strictUnusable: true})
		if !isErrorType[*errs.IOError](err) {
			t.Fatalf("strict: a 0000 %s is an error, got %v", unopenable, err)
		}
		if err := os.Chmod(filepath.Join(store.codexNS, unopenable), 0o600); err != nil {
			t.Fatalf("chmod back: %v", err)
		}
		for _, name := range []string{testAuth, testAuthPending, testAuthMeta} {
			if _, present := readMaybe(store, name); !present {
				t.Errorf("strict, 0000 %s: `%s` is kept", unopenable, name)
			}
		}
	}

	lenient := newSecretFileStore(t)
	put(t, lenient, testAuth, old)
	put(t, lenient, testAuthPending, tokenDocJSON("at-new", "rt-new"))
	put(t, lenient, testAuthMeta, metaBody(t, docDigests(t, old)))
	if err := os.Chmod(filepath.Join(lenient.codexNS, testAuth), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	decision, _ := resolveDoc(t, lenient, false, tokenDoc{})
	if err := os.Chmod(filepath.Join(lenient.codexNS, testAuth), 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if diff := gocmp.Diff(PendingDecision{Kind: PendingDiscarded, Reason: PendingFileRemoved}, decision); diff != "" {
		t.Errorf("the lenient rule is unchanged (-want +got):\n%s", diff)
	}
}

func TestPendingASpecWithAnEscapingOrAliasedNameIsRefusedBeforeAnythingIsTouched(t *testing.T) {
	// renameat and unlinkat resolve ".." inside a name, so an unchecked
	// spec name would carry the replay or the discard out of the
	// namespace the descriptor was walked to.
	escape := "../../../outside.json"
	tests := map[string]struct {
		target  string
		pending string
		meta    string
	}{
		"error: an escaping target name":        {target: escape, pending: testAuthPending, meta: testAuthMeta},
		"error: an escaping pending name":       {target: testAuth, pending: escape, meta: testAuthMeta},
		"error: a separator in the meta name":   {target: testAuth, pending: testAuthPending, meta: "a/b"},
		"error: pending equal to the target":    {target: testAuth, pending: testAuth, meta: testAuthMeta},
		"error: meta equal to the target":       {target: testAuth, pending: testAuthPending, meta: testAuth},
		"error: meta equal to the pending name": {target: testAuth, pending: testAuthMeta, meta: testAuthMeta},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			outside := filepath.Join(store.configDir, "outside.json")
			if err := os.WriteFile(outside, []byte("not this store's"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			old := tokenDocJSON("at-old", "rt-old")
			put(t, store, testAuth, old)
			put(t, store, testAuthPending, tokenDocJSON("at-new", "rt-new"))
			put(t, store, testAuthMeta, metaBody(t, docDigests(t, old)))

			spec := &PendingSpec{TargetName: tt.target, PendingName: tt.pending, MetaName: tt.meta}
			_, _, err := ResolvePendingWith(store.codexFD, store.codexNS, spec, false, tokenDoc{})
			if !isErrorType[*OutsideRootError](err) {
				t.Fatalf("must be refused, got %v", err)
			}
			if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != old {
				t.Errorf("target untouched, got %q", got)
			}
			for _, kept := range []string{testAuthPending, testAuthMeta} {
				if _, present := readMaybe(store, kept); !present {
					t.Errorf("`%s` untouched", kept)
				}
			}
			if got := readText(t, outside); got != "not this store's" {
				t.Errorf("nothing outside the namespace was written over or removed, got %q", got)
			}
		})
	}
}

func TestPendingMetaBytesKeepTheOnDiskShape(t *testing.T) {
	// The bytes a park writes must not change: existing stores hold them.
	fixed := time.Date(2026, 9, 16, 12, 0, 0, 123456000, time.UTC)
	metaNow = func() time.Time { return fixed }
	t.Cleanup(func() { metaNow = time.Now })

	expires := int64(1_234_567_890_123)
	prior := &Digests{AccessSHA256: strings.Repeat("a", 64), RefreshSHA256: strings.Repeat("b", 64)}
	spec := &PendingSpec{TargetName: CredentialsFile, PendingName: PendingFile, MetaName: PendingMetaFile, Prior: prior, ExpiresAtMS: &expires}
	body, err := metaJSON(spec)
	if err != nil {
		t.Fatalf("serializes: %v", err)
	}
	want := `{"derived_from_access_sha256":"` + strings.Repeat("a", 64) + `","derived_from_refresh_sha256":"` + strings.Repeat("b", 64) + `","created_at":"2026-09-16T12:00:00.123456Z","new_expires_at":1234567890123}`
	if diff := gocmp.Diff(want, string(body)); diff != "" {
		t.Errorf("meta bytes mismatch (-want +got):\n%s", diff)
	}

	first := &PendingSpec{TargetName: CredentialsFile, PendingName: PendingFile, MetaName: PendingMetaFile}
	body, err = metaJSON(first)
	if err != nil {
		t.Fatalf("serializes: %v", err)
	}
	wantPrefix := `{"derived_from_access_sha256":null,"derived_from_refresh_sha256":null,"created_at":"`
	if !strings.HasPrefix(string(body), wantPrefix) {
		t.Errorf("a first write records nulls, as before: %s", body)
	}
	if strings.Contains(string(body), "new_expires_at") {
		t.Errorf("no expiry member when the spec has none: %s", body)
	}
}

func TestPendingAMetaWithoutNewExpiresAtIsStillInvalidForAnExpiryRequiringStore(t *testing.T) {
	// The read path keeps new_expires_at required where it always was: a
	// lenient default there would turn this discard into a replay.
	for _, member := range []string{"", `,"new_expires_at":null`} {
		store := newSecretFileStore(t)
		old := tokenDocJSON("at-old", "rt-old")
		prior := docDigests(t, old)
		baseMeta := fmt.Sprintf(`{"derived_from_access_sha256":"%s","derived_from_refresh_sha256":"%s","created_at":"2026-09-16T12:00:00Z"%s}`, prior.AccessSHA256, prior.RefreshSHA256, member)
		put(t, store, testAuth, old)
		put(t, store, testAuthPending, tokenDocJSON("n", "r"))
		put(t, store, testAuthMeta, baseMeta)

		decision, _ := resolveDoc(t, store, false, tokenDoc{requiresExpiry: true})
		want := PendingDecision{Kind: PendingDiscarded, Reason: PendingInvalid}
		if diff := gocmp.Diff(want, decision); diff != "" {
			t.Errorf("member %q (-want +got):\n%s", member, diff)
		}
	}
}

func TestPendingAMetaWrittenWithTheFixedMemberOrderStillReplays(t *testing.T) {
	// The meta below is byte-for-byte what the established writer parks:
	// members in declaration order, new_expires_at a required integer.
	store := newSecretFileStore(t)
	old := tokenDocJSON("old-access", "old-refresh")
	replacement := tokenDocJSON("new-access", "new-refresh")
	prior := docDigests(t, old)
	baseMeta := fmt.Sprintf(`{"derived_from_access_sha256":"%s","derived_from_refresh_sha256":"%s","created_at":"2026-09-16T12:00:00.123456Z","new_expires_at":9999999999999}`, prior.AccessSHA256, prior.RefreshSHA256)
	put(t, store, testAuth, old)
	put(t, store, testAuthPending, replacement)
	put(t, store, testAuthMeta, baseMeta)

	decision, _ := resolveDoc(t, store, false, tokenDoc{requiresExpiry: true})
	if diff := gocmp.Diff(PendingDecision{Kind: PendingReplayed}, decision); diff != "" {
		t.Errorf("decision mismatch (-want +got):\n%s", diff)
	}
	if got := readText(t, filepath.Join(store.codexNS, testAuth)); got != replacement {
		t.Errorf("the replay landed, got %q", got)
	}
	for _, name := range []string{testAuthPending, testAuthMeta} {
		if _, present := readMaybe(store, name); present {
			t.Errorf("`%s` must be cleared", name)
		}
	}
}

func TestPendingResolutionRefusesASymlinkedNamespaceWalk(t *testing.T) {
	// The thin caller's walk is the store's: a symlinked component on the
	// way to the namespace refuses the resolution before anything inside
	// is read or removed.
	store := newFileStore(t)
	elsewhere := plantDirectoryLink(t, store.tempDir, store.nsDir)
	if err := os.WriteFile(filepath.Join(elsewhere, PendingFile), []byte(tokenDocJSON("smuggled", "r")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := OpenDirUnder(store.paths.NamespaceRoot(), store.nsDir)
	if !isErrorType[*SymlinkRefusedError](err) {
		t.Fatalf("a symlinked namespace must be refused, got %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(elsewhere, PendingFile)); statErr != nil {
		t.Errorf("nothing was deleted through the link: %v", statErr)
	}
}
