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
	"fmt"
	"path/filepath"
	"time"

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
