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

package claude

// ExistingKind describes the credential at an adoption target.
type ExistingKind uint8

const (
	ExistingAbsent ExistingKind = iota
	ExistingSame
	ExistingDifferent
	ExistingUnreadable
)

// Existing is the already-read state at an adoption target.
type Existing struct {
	Kind        ExistingKind
	ExpiresAtMS int64
}

// ExistingFrom classifies an optional stored digest against the displaced one.
func ExistingFrom(read *Digests, expiresAtMS int64, displaced Digests) Existing {
	if read == nil {
		return Existing{Kind: ExistingAbsent}
	}
	if *read == displaced {
		return Existing{Kind: ExistingSame}
	}
	return Existing{Kind: ExistingDifferent, ExpiresAtMS: expiresAtMS}
}

// AdoptionRefusal is why preserving a displaced credential is unsafe.
type AdoptionRefusal string

const (
	AdoptionNewerCopy         AdoptionRefusal = "newer_copy"
	AdoptionPendingPresent    AdoptionRefusal = "pending_present"
	AdoptionMigrated          AdoptionRefusal = "migrated"
	AdoptionIdentityMismatch  AdoptionRefusal = "identity_mismatch"
	AdoptionUnreadable        AdoptionRefusal = "unreadable"
	AdoptionOccupiedByAnother AdoptionRefusal = "occupied_by_another"
	AdoptionChanged           AdoptionRefusal = "changed"
)

// Message explains why the outgoing credential cannot be preserved.
func (r AdoptionRefusal) Message() string {
	const prefix = "the outgoing credential cannot be adopted: "
	switch r {
	case AdoptionNewerCopy:
		return prefix + "the copy already stored is newer, and may hold a refresh token the server has rotated away from this one"
	case AdoptionPendingPresent:
		return prefix + "an unresolved pending write is parked in that namespace; run `agentctl claude status` to settle it first"
	case AdoptionMigrated:
		return prefix + "that namespace has migrated into the keychain, and restoring a plaintext store there would be shadowed by it"
	case AdoptionIdentityMismatch:
		return prefix + "the keychain item belongs to a different identity than the account that owns this store"
	case AdoptionUnreadable:
		return prefix + "the copy already stored could not be read"
	case AdoptionOccupiedByAnother:
		return prefix + "the adopted copy beside the store belongs to another account, and replacing it would destroy that account's only copy"
	case AdoptionChanged:
		return prefix + "the copy already stored changed while this swap was preparing, so it was never weighed against the one being adopted"
	default:
		return prefix + "the adoption decision is invalid"
	}
}

// AdoptionKind names the action that preserves the displaced credential.
type AdoptionKind uint8

const (
	AdoptionAlreadyPresent AdoptionKind = iota
	AdoptionToStore
	AdoptionToAdoptedCopy
	AdoptionDiscarded
	AdoptionRefused
)

// Adoption is a pure decision; the caller owns locking and persistence.
type Adoption struct {
	Kind    AdoptionKind
	Refusal AdoptionRefusal
}

// Writes reports whether the decision needs a file write.
func (a Adoption) Writes() bool { return a.Kind == AdoptionToStore || a.Kind == AdoptionToAdoptedCopy }

// AdoptionInput contains only facts already read before the decision.
type AdoptionInput struct {
	DisplacedIsIncoming      bool
	IncomingExpiresAtMS      int64
	DisplacedIsDuplicate     bool
	ExistingIsAnotherAccount bool
	SameNamespace            bool
	IdentityMatches          bool
	PendingPresent           bool
	TargetMigrated           bool
	Existing                 Existing
	DisplacedExpiresAtMS     int64
}

// DecideAdoption preserves a displaced grant or refuses without writing.
func DecideAdoption(input AdoptionInput) Adoption {
	if input.DisplacedIsIncoming {
		if input.DisplacedIsDuplicate || input.IncomingExpiresAtMS > input.DisplacedExpiresAtMS {
			return Adoption{Kind: AdoptionDiscarded}
		}
		if input.PendingPresent {
			return Adoption{Kind: AdoptionRefused, Refusal: AdoptionPendingPresent}
		}
		if input.ExistingIsAnotherAccount {
			return Adoption{Kind: AdoptionRefused, Refusal: AdoptionOccupiedByAnother}
		}
		return placeAdoption(input, AdoptionToAdoptedCopy)
	}
	if input.PendingPresent {
		return Adoption{Kind: AdoptionRefused, Refusal: AdoptionPendingPresent}
	}
	if input.SameNamespace {
		if !input.IdentityMatches {
			return Adoption{Kind: AdoptionRefused, Refusal: AdoptionIdentityMismatch}
		}
		return placeAdoption(input, AdoptionToAdoptedCopy)
	}
	if input.TargetMigrated {
		return Adoption{Kind: AdoptionRefused, Refusal: AdoptionMigrated}
	}
	return placeAdoption(input, AdoptionToStore)
}

// DecideUndoAdoption exchanges the adopted copy with the item, regardless of expiry.
// The copy being replaced has already been read for restoration, so it is not lost.
func DecideUndoAdoption(input AdoptionInput) Adoption {
	if input.PendingPresent {
		return Adoption{Kind: AdoptionRefused, Refusal: AdoptionPendingPresent}
	}
	if input.Existing.Kind == ExistingSame {
		return Adoption{Kind: AdoptionAlreadyPresent}
	}
	return Adoption{Kind: AdoptionToAdoptedCopy}
}

func placeAdoption(input AdoptionInput, write AdoptionKind) Adoption {
	switch input.Existing.Kind {
	case ExistingAbsent:
		return Adoption{Kind: write}
	case ExistingSame:
		return Adoption{Kind: AdoptionAlreadyPresent}
	case ExistingDifferent:
		if input.Existing.ExpiresAtMS < input.DisplacedExpiresAtMS {
			return Adoption{Kind: write}
		}
		return Adoption{Kind: AdoptionRefused, Refusal: AdoptionNewerCopy}
	default:
		return Adoption{Kind: AdoptionRefused, Refusal: AdoptionUnreadable}
	}
}
