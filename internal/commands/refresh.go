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

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
)

// TokenRefresher mints a new token pair from a stored refresh token. It
// is the seam the status pass calls at its refresh barriers; the OAuth
// client implements it.
type TokenRefresher interface {
	// Refresh exchanges the credentials' refresh token for a new pair.
	// It sends exactly one POST and classifies the failure; it never
	// writes a store.
	Refresh(ctx context.Context, credentials *claude.Credentials) (*claude.TokenResponse, error)
}

// refreshUnavailableNote is what an expired owned row says while the
// store write path is not wired: minting a token that cannot be written
// back would rotate the stored refresh chain's successor into memory and
// then lose it, so the pass declines the whole barrier instead.
const refreshUnavailableNote = "refresh unavailable in this build"

// refreshOutcome is what one trip through the refresh barrier produced.
type refreshOutcome struct {
	// credentials is what the row carries on with, or nil when the row
	// is finished.
	credentials *claude.Credentials
	// state replaces the row's state when set.
	state *claude.AccountState
	// note is a short explanation for the row, when there is one.
	note string
	// lockState is what the namespace lock did, for the JSON report.
	lockState string
}

// refreshExpired is the refresh barrier: the one place an expired owned
// credential would be re-minted and stored.
//
// It currently declines every refresh. The safe write sequence — take
// the namespace lock, re-check for a peer session under it, POST, check
// the target again, then rename — rests on store primitives that are not
// wired into this pass yet, and a POST whose result cannot be stored
// would spend the old refresh token for nothing: the server may rotate
// it, and the rotated successor would exist only in this process. The
// row therefore reports the expiry and why it was left alone. When the
// locked write path is wired, it goes here, and [TokenRefresher] is the
// client it calls between the lock and the re-check.
func (s *Status) refreshExpired(ctx context.Context, record *config.AccountRecord, current *claude.Credentials) refreshOutcome {
	_, _, _ = ctx, record, current
	expired := claude.StateOfExpired(false)
	return refreshOutcome{state: &expired, note: refreshUnavailableNote, lockState: "none"}
}
