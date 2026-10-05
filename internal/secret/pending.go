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
	json "encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

// Digests are fingerprints of the token material, safe to write to disk and
// to compare. They answer "is this the same credential?" without holding the
// credential: folding a keychain entry into the live row, and deciding
// whether a pending credential still applies to the file it was derived
// from.
type Digests struct {
	// AccessSHA256 is sha256(access_token), lowercase hex.
	AccessSHA256 string
	// RefreshSHA256 is sha256(refresh_token), lowercase hex; empty when the
	// credential carries no refresh token.
	RefreshSHA256 string
}

// PendingSpec names one pending credential and what it was derived from.
//
// The three names are single path components inside the namespace directory
// the operation is handed as a descriptor.
type PendingSpec struct {
	// TargetName is the credential file a replay renames onto.
	TargetName string
	// PendingName is where a credential waits after a failed rename.
	PendingName string
	// MetaName is what the pending credential was derived from.
	MetaName string
	// Prior holds the digests of the file the credential was derived from;
	// nil on a first write. Recorded in the meta when a write parks a
	// credential.
	Prior *Digests
	// ExpiresAtMS is the new access token's expiry in milliseconds since
	// the epoch, when the vendor has one to record; nil otherwise.
	ExpiresAtMS *int64
}

// pendingMetaOut is the meta as a writer emits it. The member order and
// spelling are fixed by the stores already on disk, and new_expires_at is
// omitted rather than written as null when there is no expiry, so a write
// produces exactly the bytes existing stores hold.
type pendingMetaOut struct {
	DerivedFromAccessSHA256  *string `json:"derived_from_access_sha256"`
	DerivedFromRefreshSHA256 *string `json:"derived_from_refresh_sha256"`
	CreatedAt                string  `json:"created_at"`
	NewExpiresAt             *int64  `json:"new_expires_at,omitzero"`
}

// pendingMetaIn is the meta as the resolver reads it. Pointer fields let a
// missing member be told from a present one: created_at is required for the
// meta to be valid, and whether new_expires_at is required is the
// credential's choice.
type pendingMetaIn struct {
	DerivedFromAccessSHA256  *string `json:"derived_from_access_sha256"`
	DerivedFromRefreshSHA256 *string `json:"derived_from_refresh_sha256"`
	CreatedAt                *string `json:"created_at"`
	NewExpiresAt             *int64  `json:"new_expires_at"`
}

// PendingCredential is what the pending protocol needs to know about one
// vendor's credential file. The protocol only ever has bytes in hand, so
// every question is asked of bytes.
type PendingCredential interface {
	// MetaRequiresExpiry says whether a pending meta must carry
	// new_expires_at to be valid. A store whose metas have always required
	// it keeps discarding a meta without one as invalid; a vendor whose
	// credential has no expiry to record answers false.
	MetaRequiresExpiry() bool
	// UnusableIsAbsent says whether a file that is present but unusable
	// counts as absent: a target that does not parse, or a target, pending
	// file or meta that exists and cannot be opened. A vendor whose own
	// writer rewrites the file in place can leave it torn for a moment,
	// and whose pending file may be the only copy of a rotated grant,
	// answers false: the resolver then reads strictly and returns an error
	// for any such file, keeping everything for the next run. A pending
	// file that is a link, not a regular file, or oversized is still
	// invalid either way — no writer of ours leaves one.
	UnusableIsAbsent() bool
	// Validate says whether the bytes are a credential this vendor's
	// writer would have written.
	Validate(b []byte) bool
	// Digests returns the digests of the credential in the bytes, or
	// false when they do not parse.
	Digests(b []byte) (Digests, bool)
}

// PendingDecisionKind is what resolving a pending file decided.
type PendingDecisionKind int

const (
	// PendingNone means there was nothing pending.
	PendingNone PendingDecisionKind = iota + 1
	// PendingReplayed means the pending credentials were moved into place.
	PendingReplayed
	// PendingDiscarded means the pending credentials were deleted unused.
	PendingDiscarded
)

// PendingDecision is the outcome of one resolution.
type PendingDecision struct {
	// Kind says which row of the table applied.
	Kind PendingDecisionKind
	// FirstWrite reports whether a replayed credential was parked before
	// any file existed; meaningful only for [PendingReplayed].
	FirstWrite bool
	// Reason says why a credential was discarded; meaningful only for
	// [PendingDiscarded].
	Reason PendingDiscardReason
}

// PendingDiscardReason is why a pending file was discarded.
type PendingDiscardReason int

const (
	// PendingInvalid means the metadata was missing, unparseable, or one
	// of the files was not a plain file of a sane size.
	PendingInvalid PendingDiscardReason = iota + 1
	// PendingNamespaceTakenOver means a session or a keychain migration
	// has taken the namespace over since the pending file was written.
	PendingNamespaceTakenOver
	// PendingFileChanged means the credential file changed since the
	// pending file was derived from it, so replaying would undo that
	// change.
	PendingFileChanged
	// PendingFileRemoved means the credential file was removed, and the
	// pending file was derived from one that existed.
	PendingFileRemoved
)

// Label is the reason as it appears in the state column.
func (r PendingDiscardReason) Label() string {
	switch r {
	case PendingInvalid:
		return "invalid"
	case PendingNamespaceTakenOver:
		return "namespace taken over"
	case PendingFileChanged:
		return "file changed"
	case PendingFileRemoved:
		return "file removed"
	default:
		return ""
	}
}

// PendingWrite is what a resolution changed on disk, for a caller that
// records every write. A nil result from [ResolvePendingWith] means no
// credential file was touched: nothing was pending, or only a lone meta
// was cleared.
type PendingWrite struct {
	// Before holds the digests of the target file before the resolution,
	// when it parsed.
	Before *Digests
	// Pending holds the digests of the pending credential that was
	// replayed or dropped, when it parsed.
	Pending *Digests
}

// ResolvePendingWith applies the pending decision table to one namespace
// directory.
//
// The table, in check order — validity first, then foreign activity, then
// the digest comparison:
//
//	| on disk                                                | decision                |
//	|--------------------------------------------------------|-------------------------|
//	| no pending file (a lone meta is removed)               | nothing pending         |
//	| pending or meta unreadable, a link, big, or unparsable | discarded, invalid      |
//	| the namespace was taken over by somebody else          | discarded, taken over   |
//	| target present, digests equal the meta's derived ones  | replayed                |
//	| target present, digests differ                         | discarded, file changed |
//	| target absent, meta has no derived digests             | replayed, first write   |
//	| target absent, meta has derived digests                | discarded, file removed |
//
// dir is a descriptor the caller's no-follow walk produced, and every
// operation is relative to it. shown is that directory as the user would
// recognise it, used only in error sentences. foreignTakenOver is the
// caller's own judgement of whether somebody else now manages the
// namespace; how it is reached differs by vendor, and the table only needs
// the answer. Errors are returned only for failures that leave the
// namespace in an unknown state — an unreadable target, or a replay rename
// that failed; every decidable outcome, including a corrupt or hostile
// pending file, is a [PendingDecision].
func ResolvePendingWith(dir int, shown string, spec *PendingSpec, foreignTakenOver bool, cred PendingCredential) (PendingDecision, *PendingWrite, error) {
	if err := checkSpecNames(spec, shown); err != nil {
		return PendingDecision{}, nil, err
	}
	pendingShown := filepath.Join(shown, spec.PendingName)
	metaShown := filepath.Join(shown, spec.MetaName)
	targetShown := filepath.Join(shown, spec.TargetName)

	// A meta with no pending file is the crash window between writing the
	// meta and parking the credential: there is nothing to replay.
	kind, err := entryAt(dir, spec.PendingName, pendingShown)
	if err != nil {
		return PendingDecision{}, nil, err
	}
	if kind == entryAbsent {
		_ = unlinkAt(dir, spec.MetaName)
		return PendingDecision{Kind: PendingNone}, nil, nil
	}

	// The reader follows the vendor's rule. Under the lenient rule an
	// unopenable file is absent, which collapses into invalid below; under
	// the strict rule an open failure is an error that keeps every file.
	read := func(name string, limit int64, fileShown string) (ReadOutcome, error) {
		if cred.UnusableIsAbsent() {
			return readFileAt(dir, name, limit, fileShown)
		}
		return readFileAtStrict(dir, name, limit, fileShown)
	}
	var pendingBytes []byte
	switch outcome, err := read(spec.PendingName, MaxCredentialsBytes, pendingShown); {
	case err != nil && !cred.UnusableIsAbsent() && isOpenFailure(err):
		return PendingDecision{}, nil, err
	case err == nil && outcome.Present:
		pendingBytes = outcome.Bytes
	}
	var pendingDigests *Digests
	if pendingBytes != nil {
		if digests, ok := cred.Digests(pendingBytes); ok {
			pendingDigests = &digests
		}
	}
	var meta *pendingMetaIn
	switch outcome, err := read(spec.MetaName, MaxMetaBytes, metaShown); {
	case err != nil && !cred.UnusableIsAbsent() && isOpenFailure(err):
		return PendingDecision{}, nil, err
	case err == nil && outcome.Present:
		var parsed pendingMetaIn
		if json.Unmarshal(outcome.Bytes, &parsed) == nil && parsed.CreatedAt != nil {
			meta = &parsed
		}
	}

	discard := func(reason PendingDiscardReason, before *Digests) (PendingDecision, *PendingWrite, error) {
		_ = unlinkAt(dir, spec.PendingName)
		_ = unlinkAt(dir, spec.MetaName)
		return PendingDecision{Kind: PendingDiscarded, Reason: reason}, &PendingWrite{Before: before, Pending: pendingDigests}, nil
	}
	replay := func(firstWrite bool, before *Digests) (PendingDecision, *PendingWrite, error) {
		// The mode is set on the pending file before the rename, because
		// a rename carries the inode and its mode across, and a chmod
		// afterwards would be a second lookup of a name that is now the
		// live credential file.
		if err := chmod0600At(dir, spec.PendingName, pendingShown); err != nil {
			return PendingDecision{}, nil, err
		}
		if err := unix.Renameat(dir, spec.PendingName, dir, spec.TargetName); err != nil {
			return PendingDecision{}, nil, errs.NewIO(fmt.Sprintf("could not replay `%s`", pendingShown), err)
		}
		_ = unlinkAt(dir, spec.MetaName)
		return PendingDecision{Kind: PendingReplayed, FirstWrite: firstWrite}, &PendingWrite{Before: before, Pending: pendingDigests}, nil
	}

	if pendingBytes == nil || meta == nil {
		return discard(PendingInvalid, nil)
	}
	if !cred.Validate(pendingBytes) || (cred.MetaRequiresExpiry() && meta.NewExpiresAt == nil) {
		return discard(PendingInvalid, nil)
	}
	if foreignTakenOver {
		return discard(PendingNamespaceTakenOver, nil)
	}

	var current *Digests
	switch outcome, err := read(spec.TargetName, MaxCredentialsBytes, targetShown); {
	// An unreadable current file is not something to overwrite blindly.
	case err != nil:
		return PendingDecision{}, nil, err
	case outcome.Present:
		digests, ok := cred.Digests(outcome.Bytes)
		switch {
		case ok:
			current = &digests
		case cred.UnusableIsAbsent():
		default:
			return PendingDecision{}, nil, errs.NewConfig(fmt.Sprintf("`%s` is present but does not parse; the pending credential is kept for the next run", targetShown))
		}
	}

	switch {
	case current != nil:
		matches := meta.DerivedFromAccessSHA256 != nil && *meta.DerivedFromAccessSHA256 == current.AccessSHA256 && refreshMatches(meta.DerivedFromRefreshSHA256, current.RefreshSHA256)
		if matches {
			return replay(false, current)
		}
		return discard(PendingFileChanged, current)
	case meta.DerivedFromAccessSHA256 == nil && meta.DerivedFromRefreshSHA256 == nil:
		return replay(true, nil)
	default:
		return discard(PendingFileRemoved, nil)
	}
}

// refreshMatches compares the meta's derived refresh digest — absent or a
// value — with the current file's, where an empty string means the
// credential has no refresh token.
func refreshMatches(derived *string, current string) bool {
	if derived == nil {
		return current == ""
	}
	return *derived == current
}

// isOpenFailure says whether a strict read failed to open or read a file
// that is there, as opposed to finding a link, a non-regular file or an
// oversized one.
func isOpenFailure(err error) bool {
	_, ok := errors.AsType[*errs.IOError](err)
	return ok
}

// checkSpecNames refuses a spec whose names could leave the directory they
// are resolved in, or could alias one another.
//
// Each of the three names is handed to openat, renameat and unlinkat
// relative to a walked directory descriptor, and those calls resolve ".."
// and "/" inside a name; so every name must be one plain component, or the
// root the descriptor stands for is gone. The three must also differ: a
// pending name equal to the target would park over the live file, and a
// meta name equal to either would be unlinked as "the meta". Checked before
// any file is examined or staged. dirShown is the directory as the user
// would recognise it, for the error.
func checkSpecNames(spec *PendingSpec, dirShown string) error {
	for _, name := range []string{spec.TargetName, spec.PendingName, spec.MetaName} {
		if !config.IsSingleComponent(name) {
			return &OutsideRootError{Path: filepath.Join(dirShown, name)}
		}
	}
	if spec.PendingName == spec.TargetName || spec.MetaName == spec.TargetName {
		return &OutsideRootError{Path: filepath.Join(dirShown, spec.TargetName)}
	}
	if spec.MetaName == spec.PendingName {
		return &OutsideRootError{Path: filepath.Join(dirShown, spec.PendingName)}
	}
	return nil
}

// metaNow is the clock the meta's created_at comes from, replaceable only
// by a test in this package.
var metaNow = time.Now

// metaJSON is the meta a failed rename parks beside the pending credential.
func metaJSON(spec *PendingSpec) ([]byte, error) {
	meta := pendingMetaOut{
		CreatedAt:    metaNow().UTC().Format(time.RFC3339Nano),
		NewExpiresAt: spec.ExpiresAtMS,
	}
	if spec.Prior != nil {
		meta.DerivedFromAccessSHA256 = &spec.Prior.AccessSHA256
		if spec.Prior.RefreshSHA256 != "" {
			meta.DerivedFromRefreshSHA256 = &spec.Prior.RefreshSHA256
		}
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return nil, errs.NewConfig(fmt.Sprintf("could not serialize pending metadata: %v", err))
	}
	return body, nil
}
