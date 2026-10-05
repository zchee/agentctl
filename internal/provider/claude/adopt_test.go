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

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestAdoptionMatrix(t *testing.T) {
	tests := map[string]struct {
		existing Existing
		want     Adoption
	}{
		"success: absent":         {Existing{Kind: ExistingAbsent}, Adoption{Kind: AdoptionToStore}},
		"success: same":           {Existing{Kind: ExistingSame}, Adoption{Kind: AdoptionAlreadyPresent}},
		"success: strictly older": {Existing{Kind: ExistingDifferent, ExpiresAtMS: 99}, Adoption{Kind: AdoptionToStore}},
		"error: equal expiry":     {Existing{Kind: ExistingDifferent, ExpiresAtMS: 100}, Adoption{Kind: AdoptionRefused, Refusal: AdoptionNewerCopy}},
		"error: newer expiry":     {Existing{Kind: ExistingDifferent, ExpiresAtMS: 101}, Adoption{Kind: AdoptionRefused, Refusal: AdoptionNewerCopy}},
		"error: unreadable":       {Existing{Kind: ExistingUnreadable}, Adoption{Kind: AdoptionRefused, Refusal: AdoptionUnreadable}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, same := range []bool{false, true} {
				input := AdoptionInput{Existing: tt.existing, SameNamespace: same, IdentityMatches: true, DisplacedExpiresAtMS: 100}
				want := tt.want
				if same && want.Kind == AdoptionToStore {
					want.Kind = AdoptionToAdoptedCopy
				}
				if diff := gocmp.Diff(want, DecideAdoption(input)); diff != "" {
					t.Fatalf("same=%v (-want +got):\n%s", same, diff)
				}
				input.PendingPresent, input.IdentityMatches, input.TargetMigrated = true, false, true
				if got := DecideAdoption(input); got.Refusal != AdoptionPendingPresent {
					t.Fatalf("pending must outrank matrix and identity: %+v", got)
				}
				input.PendingPresent = false
				wantRefusal := AdoptionMigrated
				if same {
					wantRefusal = AdoptionIdentityMismatch
				}
				if got := DecideAdoption(input); got.Refusal != wantRefusal {
					t.Fatalf("guard: %+v, want %s", got, wantRefusal)
				}
			}
		})
	}
}

func TestIncomingAdoptionKeepsOnlyDistinctNonOlderCopies(t *testing.T) {
	tests := map[string]struct {
		incoming  int64
		duplicate bool
		want      AdoptionKind
	}{
		"success: incoming older":     {99, false, AdoptionToAdoptedCopy},
		"success: equal but distinct": {100, false, AdoptionToAdoptedCopy},
		"success: duplicate":          {99, true, AdoptionDiscarded},
		"success: incoming newer":     {101, false, AdoptionDiscarded},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			input := AdoptionInput{DisplacedIsIncoming: true, IncomingExpiresAtMS: tt.incoming, DisplacedIsDuplicate: tt.duplicate, DisplacedExpiresAtMS: 100, TargetMigrated: true}
			if got := DecideAdoption(input); got.Kind != tt.want {
				t.Fatalf("got %+v, want %v", got, tt.want)
			}
			input.PendingPresent = true
			input.ExistingIsAnotherAccount = true
			if got := DecideAdoption(input); tt.want == AdoptionDiscarded {
				if got.Kind != AdoptionDiscarded {
					t.Fatalf("no target needed: %+v", got)
				}
			} else {
				if got.Refusal != AdoptionPendingPresent {
					t.Fatalf("pending first: %+v", got)
				}
				input.PendingPresent = false
				input.Existing = Existing{Kind: ExistingDifferent, ExpiresAtMS: 101}
				if got := DecideAdoption(input); got.Refusal != AdoptionOccupiedByAnother {
					t.Fatalf("identity before expiry: %+v", got)
				}
				input.ExistingIsAnotherAccount = false
				if got := DecideAdoption(input); got.Refusal != AdoptionNewerCopy {
					t.Fatalf("keep guards expiry: %+v", got)
				}
				input.Existing = Existing{Kind: ExistingUnreadable}
				if got := DecideAdoption(input); got.Refusal != AdoptionUnreadable {
					t.Fatalf("keep guards read: %+v", got)
				}
			}
		})
	}
}

func TestUndoAdoptionExchangesTheAlreadyReadCopy(t *testing.T) {
	tests := map[string]struct {
		existing Existing
		want     AdoptionKind
	}{
		"success: absent":     {Existing{Kind: ExistingAbsent}, AdoptionToAdoptedCopy},
		"success: same":       {Existing{Kind: ExistingSame}, AdoptionAlreadyPresent},
		"success: newer":      {Existing{Kind: ExistingDifferent, ExpiresAtMS: 200}, AdoptionToAdoptedCopy},
		"success: unreadable": {Existing{Kind: ExistingUnreadable}, AdoptionToAdoptedCopy},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			input := AdoptionInput{Existing: tt.existing, DisplacedIsIncoming: true, DisplacedIsDuplicate: true, SameNamespace: true, TargetMigrated: true, ExistingIsAnotherAccount: true}
			if got := DecideUndoAdoption(input); got.Kind != tt.want {
				t.Fatalf("got %+v, want %v", got, tt.want)
			}
			input.PendingPresent = true
			if got := DecideUndoAdoption(input); got.Refusal != AdoptionPendingPresent {
				t.Fatalf("pending: %+v", got)
			}
		})
	}
}

func TestAdoptionVocabularyAndExisting(t *testing.T) {
	for _, refusal := range []AdoptionRefusal{AdoptionNewerCopy, AdoptionPendingPresent, AdoptionMigrated, AdoptionIdentityMismatch, AdoptionUnreadable, AdoptionOccupiedByAnother, AdoptionChanged} {
		if !strings.HasPrefix(refusal.Message(), "the outgoing credential cannot be adopted: ") {
			t.Fatalf("missing refusal message: %s", refusal)
		}
		if (Adoption{Kind: AdoptionRefused, Refusal: refusal}).Writes() {
			t.Fatal("refusal writes")
		}
	}
	for _, kind := range []AdoptionKind{AdoptionAlreadyPresent, AdoptionToStore, AdoptionToAdoptedCopy, AdoptionDiscarded} {
		if got := (Adoption{Kind: kind}).Writes(); got != (kind == AdoptionToStore || kind == AdoptionToAdoptedCopy) {
			t.Fatalf("writes(%v)=%v", kind, got)
		}
	}
	displaced := Digests{AccessSHA256: "aaaa"}
	tests := map[string]struct {
		read *Digests
		want Existing
	}{
		"success: absent":            {nil, Existing{Kind: ExistingAbsent}},
		"success: same":              {&displaced, Existing{Kind: ExistingSame}},
		"success: different access":  {&Digests{AccessSHA256: "bbbb"}, Existing{Kind: ExistingDifferent, ExpiresAtMS: 50}},
		"success: different refresh": {&Digests{AccessSHA256: "aaaa", RefreshSHA256: "bbbb"}, Existing{Kind: ExistingDifferent, ExpiresAtMS: 50}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, ExistingFrom(tt.read, 50, displaced)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
