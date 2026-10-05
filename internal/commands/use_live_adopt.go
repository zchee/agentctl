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

package commands

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

type useSourceKind uint8

const (
	useSourceOwn useSourceKind = iota
	useSourceAdopted
)

type useAdoptionKind uint8

const (
	useAdoptionNothing useAdoptionKind = iota
	useAdoptionCopy
	useAdoptionStaged
	useAdoptionThirdStore
)

type useAdoptionPlan struct {
	kind  useAdoptionKind
	dir   string
	prior *claude.Digests
}

type useAdoptionParties struct {
	paths               *config.Paths
	store               *config.AccountRecord
	subject             *useSubject
	incoming            *config.AccountRecord
	incomingCredentials *claude.Credentials
	third               *config.AccountRecord
	identity            *claude.Identity
	displaced           *claude.Credentials
	live                bool
	direction           secret.WriteDirection
}

func useCannotAdopt(reason claude.AdoptionRefusal, service string) *useReport {
	return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: reason}, service, reason.Message())
}

// useDecideAdoption reads only. Its plan holds digests, never serialized tokens.
func useDecideAdoption(ctx context.Context, parties useAdoptionParties) (useAdoptionPlan, *useReport) {
	nothing := useAdoptionPlan{kind: useAdoptionNothing}
	refuse := func(reason claude.AdoptionRefusal) (useAdoptionPlan, *useReport) {
		return nothing, useCannotAdopt(reason, parties.subject.service)
	}
	displacedDigest, err := parties.displaced.Digests()
	if err != nil {
		return refuse(claude.AdoptionUnreadable)
	}
	if parties.direction == secret.DirectionUndo && !parties.live {
		existing, _, _ := useInspectStored(parties.subject.storeDir, useSourceAdopted, displacedDigest)
		decision := claude.DecideUndoAdoption(claude.AdoptionInput{PendingPresent: usePendingPresent(parties.subject.storeDir), Existing: existing})
		if decision.Kind == claude.AdoptionAlreadyPresent {
			return nothing, nil
		}
		if decision.Kind == claude.AdoptionRefused {
			return refuse(decision.Refusal)
		}
		return useAdoptionPlan{kind: useAdoptionStaged, dir: parties.subject.storeDir}, nil
	}
	same := parties.store != nil && claude.IdentityIs(parties.identity, parties.store)
	displacedIsIncoming := !same && claude.IdentityIs(parties.identity, parties.incoming)
	parkedBeside := parties.live && parties.direction == secret.DirectionForward
	occupiedByAnother := false
	var dir string
	var existing claude.Existing
	var prior *claude.Digests
	switch {
	case displacedIsIncoming:
		dir = parties.subject.storeDir
		if parties.live {
			dir = parties.paths.NamespaceDir(parties.incoming.AccountUUID, parties.incoming.OrganizationUUID)
		}
		var found *claude.Credentials
		existing, _, found = useInspectStored(dir, useSourceAdopted, displacedDigest)
		occupiedByAnother = found != nil && !claude.SameIdentity(found, parties.incoming)
	case same:
		dir = parties.subject.storeDir
		existing, _, _ = useInspectStored(dir, useSourceAdopted, displacedDigest)
	default:
		if parties.third == nil {
			if parties.live && parties.identity != nil {
				return nothing, useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionIdentityMismatch}, parties.subject.service, fmt.Sprintf("the outgoing credential cannot be adopted: the live item holds a credential of `%s`, and no account agentctl owns is that one, so there is no namespace to file it in; log that account in with `agentctl claude login` first", usePrintable(parties.identity.AccountUUID)))
			}
			return refuse(claude.AdoptionIdentityMismatch)
		}
		dir = parties.paths.NamespaceDir(parties.third.AccountUUID, parties.third.OrganizationUUID)
		if parkedBeside {
			if _, digest, _ := useInspectStored(dir, useSourceOwn, displacedDigest); digest != nil && *digest == displacedDigest {
				return nothing, nil
			}
			var found *claude.Credentials
			existing, _, found = useInspectStored(dir, useSourceAdopted, displacedDigest)
			if existing.Kind == claude.ExistingDifferent && found != nil {
				if !claude.SameIdentity(found, parties.third) {
					return refuse(claude.AdoptionOccupiedByAnother)
				}
				if !useParkedByLiveWrite(parties.paths, found) {
					return nothing, useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionOccupiedByAnother}, parties.subject.service, fmt.Sprintf("the outgoing credential cannot be adopted: the adopted copy `%s` was not parked by a live swap agentctl recorded — a namespace swap's undo source, or a copy a live undo refreshed and wrote back but never recorded because it then ended busy, discarded or refused; undo that namespace swap, or move the file aside, then run this again", filepath.Join(dir, secret.AdoptedFile)))
				}
				existing = claude.Existing{Kind: claude.ExistingAbsent}
			}
		} else {
			existing, prior, _ = useInspectStored(dir, useSourceOwn, displacedDigest)
		}
	}
	targetMigrated := false
	if !same && !displacedIsIncoming && !parkedBeside {
		targetMigrated = useMigrated(ctx, parties.paths, parties.third)
	}
	incomingDigest, err := parties.incomingCredentials.Digests()
	if err != nil {
		return refuse(claude.AdoptionUnreadable)
	}
	decision := claude.DecideAdoption(claude.AdoptionInput{DisplacedIsIncoming: displacedIsIncoming, IncomingExpiresAtMS: parties.incomingCredentials.ExpiresAtMillis, DisplacedIsDuplicate: displacedDigest == incomingDigest, ExistingIsAnotherAccount: occupiedByAnother, SameNamespace: same, IdentityMatches: same, PendingPresent: usePendingPresent(dir), TargetMigrated: targetMigrated, Existing: existing, DisplacedExpiresAtMS: parties.displaced.ExpiresAtMillis})
	switch decision.Kind {
	case claude.AdoptionAlreadyPresent, claude.AdoptionDiscarded:
		return nothing, nil
	case claude.AdoptionRefused:
		return refuse(decision.Refusal)
	case claude.AdoptionToAdoptedCopy:
		if parties.live {
			return nothing, useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionNewerCopy}, parties.subject.service, "the outgoing credential cannot be adopted: the live item holds a copy of the incoming account's credential that is not older than the one being installed, and it is not kept beside a store on the live target; nothing was swapped")
		}
		return useAdoptionPlan{kind: useAdoptionCopy, dir: dir}, nil
	case claude.AdoptionToStore:
		if parkedBeside {
			return useAdoptionPlan{kind: useAdoptionCopy, dir: dir}, nil
		}
		return useAdoptionPlan{kind: useAdoptionThirdStore, dir: dir, prior: prior}, nil
	default:
		return refuse(claude.AdoptionUnreadable)
	}
}

func usePerformAdoption(ctx context.Context, paths *config.Paths, plan useAdoptionPlan, displaced *claude.Credentials) (*string, *secret.StagedAdoption, claude.AdoptionRefusal) {
	if displaced == nil || plan.kind == useAdoptionNothing {
		return nil, nil, ""
	}
	switch plan.kind {
	case useAdoptionCopy, useAdoptionStaged:
		blob, err := useSealedBlob(displaced)
		if err != nil {
			return nil, nil, claude.AdoptionUnreadable
		}
		if plan.kind == useAdoptionStaged {
			staged, err := secret.StageAdopted(ctx, paths, plan.dir, blob)
			if err != nil {
				return nil, nil, claude.AdoptionUnreadable
			}
			return nil, staged, ""
		}
		if _, err := secret.WriteAdopted(ctx, paths, plan.dir, blob); err != nil {
			return nil, nil, claude.AdoptionUnreadable
		}
		return new(secret.AdoptedFile), nil, ""
	case useAdoptionThirdStore:
		current, err := useReadStored(plan.dir, useSourceOwn)
		if err != nil {
			return nil, nil, claude.AdoptionUnreadable
		}
		var observed *claude.Digests
		if current != nil {
			digest, err := current.Digests()
			if err != nil {
				return nil, nil, claude.AdoptionUnreadable
			}
			observed = &digest
		}
		if (observed == nil) != (plan.prior == nil) || observed != nil && *observed != *plan.prior {
			return nil, nil, claude.AdoptionChanged
		}
		blob, err := displaced.BlobJSON()
		if err != nil {
			return nil, nil, claude.AdoptionUnreadable
		}
		defer memguard.WipeBytes(blob)
		if _, err := secret.WriteCredentials(ctx, &secret.WriteRequest{Paths: paths, NSDir: plan.dir, BlobJSON: blob, NewExpiresAtMS: displaced.ExpiresAtMillis}); err != nil {
			return nil, nil, claude.AdoptionUnreadable
		}
		return new(secret.CredentialsFile), nil, ""
	default:
		return nil, nil, claude.AdoptionUnreadable
	}
}

func useReadStored(dir string, source useSourceKind) (*claude.Credentials, error) {
	var read secret.ReadOutcome
	var err error
	if source == useSourceAdopted {
		read, err = secret.ReadAdopted(dir)
	} else {
		read, err = secret.ReadCredentials(dir)
	}
	if err != nil || !read.Present {
		return nil, err
	}
	defer memguard.WipeBytes(read.Bytes)
	return claude.ParseBlob(read.Bytes)
}

func useInspectStored(dir string, source useSourceKind, displaced claude.Digests) (claude.Existing, *claude.Digests, *claude.Credentials) {
	found, err := useReadStored(dir, source)
	if err != nil {
		return claude.Existing{Kind: claude.ExistingUnreadable}, nil, nil
	}
	if found == nil {
		return claude.Existing{Kind: claude.ExistingAbsent}, nil, nil
	}
	digests, err := found.Digests()
	if err != nil {
		return claude.Existing{Kind: claude.ExistingUnreadable}, nil, nil
	}
	return claude.ExistingFrom(&digests, found.ExpiresAtMillis, displaced), &digests, found
}

func usePendingPresent(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, secret.PendingFile))
	return err == nil
}

func useMigrated(ctx context.Context, paths *config.Paths, record *config.AccountRecord) bool {
	if record == nil || record.Kind.Owned == nil {
		return false
	}
	subject, refused := useBuildSubject(paths, nil, false, record, claude.ExportSpelling(paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)))
	if refused != nil {
		return false
	}
	credentials, err := useReadKeychain(ctx, secret.NewReader(), subject.service)
	return err == nil && credentials != nil
}

func useParkedByLiveWrite(paths *config.Paths, credentials *claude.Credentials) bool {
	digests, err := credentials.Digests()
	if err != nil {
		return false
	}
	digest8, ok := secret.Digest8(digests.AccessSHA256)
	if !ok {
		return false
	}
	tail, err := secret.TailAuditLog(paths, math.MaxInt)
	if err != nil {
		return false
	}
	for _, entry := range tail.Entries {
		write, ok := entry.Event.(*secret.WriteEvent)
		if !ok || write.Target != secret.TargetLive {
			continue
		}
		if write.Direction == secret.DirectionForward && write.FromDigest8 != nil && *write.FromDigest8 == digest8 || write.Direction == secret.DirectionUndo && write.ToDigest8 == digest8 {
			return true
		}
	}
	return false
}

func useThirdNamespace(registry *config.Registry, store, incoming *config.AccountRecord, identity *claude.Identity, live bool) (*config.AccountRecord, *[2]config.AccountRecord) {
	if store != nil && claude.IdentityIs(identity, store) || claude.IdentityIs(identity, incoming) || identity == nil {
		return nil, nil
	}
	var found *config.AccountRecord
	for _, record := range registry.Accounts {
		if !live {
			if record.AccountUUID == identity.AccountUUID {
				return new(record), nil
			}
			continue
		}
		if record.Kind.Owned == nil || !claude.IdentityIs(identity, &record) {
			continue
		}
		if found != nil {
			return nil, &[2]config.AccountRecord{*found, record}
		}
		found = new(record)
	}
	return found, nil
}
