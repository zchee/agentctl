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

package codex

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zchee/agentctl/internal/config"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/secret"
)

func readOwned(ctx context.Context, source codexprovider.Source, paths *config.Paths) (rowRead, string, markerView, bool) {
	owned := codexprovider.Owned(source.Record)
	guard, err := codexprovider.AcquireCodex(ctx, paths, owned, time.Second)
	if err != nil {
		switch {
		case errors.Is(err, secret.ErrLockBusy):
			return rowRead{state: codexprovider.State{Kind: codexprovider.StateBusy}}, "busy", markerView{}, false
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return rowRead{state: codexprovider.State{Kind: codexprovider.StateStale}}, "none", markerView{}, false
		default:
			return rowRead{state: codexprovider.State{Kind: codexprovider.StateLockUnavailable}, note: new(err.Error())}, "unavailable", markerView{}, false
		}
	}
	defer func() {
		if err := guard.Release(); err != nil {
			slog.WarnContext(ctx, "the Codex namespace lock could not be released", "error", err)
		}
	}()
	ns, err := codexprovider.OpenOwnedNamespace(paths, owned, guard)
	if err != nil {
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateError, Reason: err.Error()}}, "acquired", markerView{}, false
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.WarnContext(ctx, "the Codex namespace could not be closed", "error", err)
		}
	}()
	read, err := ns.Read()
	if err == nil && read.Kind == codexprovider.ResolvedTorn && waitTorn(guard.Context()) {
		read, err = ns.Read()
	}
	if err != nil {
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateError, Reason: err.Error()}}, "acquired", markerView{}, false
	}
	switch read.Kind {
	case codexprovider.ResolvedAbsent:
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateNeedsLogin}, note: new("no credential; run agentctl codex login")}, "acquired", markerView{}, false
	case codexprovider.ResolvedTorn:
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateTornRead}}, "acquired", markerView{}, false
	}
	store, err := codexprovider.NewRefreshStateStore(paths, owned.User(), owned.Account())
	var view markerView
	floor := false
	if err != nil {
		view = markerView{kind: markerUnavailable, reason: err.Error()}
	} else {
		digest, ok := read.Credentials.RefreshDigest8()
		view, floor = readMarkerView(store.Load(guard.Context()), digest, ok)
	}
	credentials, err := read.Credentials.IntoCredentials()
	if err != nil {
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateError, Reason: err.Error()}}, "acquired", view, floor
	}
	return rowRead{credentials: credentials}, "acquired", view, floor
}

func readMarkerView(read codexprovider.RefreshStateRead, grant string, hasGrant bool) (markerView, bool) {
	switch read.Kind {
	case codexprovider.RefreshStateAbsent:
		return markerView{}, false
	case codexprovider.RefreshStateUnavailable:
		return markerView{kind: markerUnavailable, reason: read.Reason}, false
	}
	state := read.State
	floor := state.DidNotHelp > 0 || state.FloorMin != 60
	if !hasGrant {
		return markerView{}, floor
	}
	if state.DeadDigest8 != nil && *state.DeadDigest8 == grant {
		return markerView{kind: markerDead}, floor
	}
	if state.Inflight == nil || state.Inflight.SentDigest8 != grant {
		return markerView{}, floor
	}
	since := state.Inflight.SentAt
	if state.AmbiguousSince != nil {
		since = *state.AmbiguousSince
	}
	class := codexprovider.RefreshUnknownInterrupted
	if state.Class != nil {
		class = *state.Class
	}
	var resendAt *time.Time
	if !state.Resent {
		resendAt = new(codexprovider.ResendEligibleAt(since, class, state.RetryAfter))
	}
	return markerView{kind: markerUnknown, since: since, class: class, resendAt: resendAt}, floor
}
