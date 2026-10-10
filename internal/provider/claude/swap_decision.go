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
	"fmt"
	"strconv"
	"strings"

	"github.com/zchee/agentctl/internal/config"
)

// SwapPhase separates read-only decisions, preparation and the peer-lock hold.
type SwapPhase uint8

const (
	// SwapReadOnly reads the environment, registry and current item.
	SwapReadOnly SwapPhase = iota
	// SwapPrepare may prompt, refresh and preserve the displaced credential.
	SwapPrepare
	// SwapHeld admits a write inside the peer-lock time budget.
	SwapHeld
)

// HoldsLocks reports whether Claude Code's peer locks are held.
func (p SwapPhase) HoldsLocks() bool { return p == SwapHeld }

// Name returns the stable phase token used by structured output.
func (p SwapPhase) Name() string {
	switch p {
	case SwapReadOnly:
		return "a"
	case SwapPrepare:
		return "b"
	case SwapHeld:
		return "c"
	default:
		return ""
	}
}

// DecidedIn identifies the latest phase allowed to decide this refusal.
// Line size is checked before a write child exists; lock drift only under a hold.
func (r SwapRefusal) DecidedIn() SwapPhase {
	switch r.Kind {
	case SwapCompromisedHold:
		return SwapHeld
	case SwapLineTooLong, SwapCannotAdopt, SwapAuditRefused:
		return SwapPrepare
	default:
		return SwapReadOnly
	}
}

// SwapDecisionOrder returns independent values in the refusal precedence order.
func SwapDecisionOrder() []SwapRefusal {
	return []SwapRefusal{
		{Kind: SwapRemoteControlUnsupportedPlatform}, {Kind: SwapNotOwned}, {Kind: SwapLiveNamespaceEnv}, {Kind: SwapLiveUnreachable}, {Kind: SwapEnvToken}, {Kind: SwapRemoteControlUnreachable}, {Kind: SwapLiveItemAbsent}, {Kind: SwapProfileUnavailable}, {Kind: SwapLiveUndoForeignLogin}, {Kind: SwapLineTooLong}, {Kind: SwapAuditRefused}, {Kind: SwapCannotAdopt, Adoption: AdoptionNewerCopy}, {Kind: SwapCompromisedHold},
	}
}

// SameIdentity accepts old credentials without identity and rejects contradictions.
func SameIdentity(credentials *Credentials, record *config.AccountRecord) bool {
	return IdentityIs(credentials.Identity(), record)
}

// IdentityIs compares the account and any organization known on both sides.
func IdentityIs(identity *Identity, record *config.AccountRecord) bool {
	if identity == nil {
		return true
	}
	if identity.AccountUUID != record.AccountUUID {
		return false
	}
	return identity.OrganizationUUID == nil || record.OrganizationUUID == config.UnknownOrg || *identity.OrganizationUUID == record.OrganizationUUID
}

// IdentitiesAgree compares account UUIDs and organizations when both are known.
func IdentitiesAgree(a, b *Identity) bool {
	return a.AccountUUID == b.AccountUUID && (a.OrganizationUUID == nil || b.OrganizationUUID == nil || *a.OrganizationUUID == *b.OrganizationUUID)
}

// OccupantOf names the identity without exposing any credential material.
func OccupantOf(credentials *Credentials) string {
	identity := credentials.Identity()
	if identity == nil {
		return "an unidentified credential"
	}
	if identity.Email != nil {
		return *identity.Email
	}
	return identity.AccountUUID
}

// SwapBusyNote explains contention without attributing a stopped process to a store.
func SwapBusyNote(holderAlive bool, stoppedPIDs []int32) string {
	if len(stoppedPIDs) != 0 {
		pids := make([]string, len(stoppedPIDs))
		for i, pid := range stoppedPIDs {
			pids[i] = strconv.FormatInt(int64(pid), 10)
		}
		return fmt.Sprintf("a stopped claude process is present (pid %s). agentctl will not break this lock while one is, because it cannot tell whether that process is the holder. Resume or end it, or run `agentctl claude doctor --remove-stale <path> --yes`", strings.Join(pids, ", "))
	}
	if holderAlive {
		return "another process is refreshing this store's credentials"
	}
	return "this store's refresh lock is held; agentctl did not break it"
}
