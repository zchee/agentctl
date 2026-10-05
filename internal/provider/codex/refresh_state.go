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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
)

const (
	refreshStateSchema   uint32 = 1
	maxRefreshStateBytes int64  = 64 << 10
)

// DefaultRefreshFloorMin is a fresh grant's unauthorized-refresh floor.
const DefaultRefreshFloorMin uint32 = 60

// RefreshUnknownClass is a fixed explanation of an unknown refresh outcome.
type RefreshUnknownClass string

const (
	// RefreshUnknownAmbiguous means a request may have reached the server.
	RefreshUnknownAmbiguous RefreshUnknownClass = "ambiguous"
	// RefreshUnknownServerError means the server returned a 5xx.
	RefreshUnknownServerError RefreshUnknownClass = "server_error"
	// RefreshUnknownRateLimited means the server returned a 429.
	RefreshUnknownRateLimited RefreshUnknownClass = "rate_limited"
	// RefreshUnknownInterrupted means a process left an unclassified send.
	RefreshUnknownInterrupted RefreshUnknownClass = "interrupted"
	// RefreshUnknownTLS means a TLS failure did not prove absence of a send.
	RefreshUnknownTLS RefreshUnknownClass = "tls"
	// RefreshUnknownWriteFailed means a rotated credential could not be saved.
	RefreshUnknownWriteFailed RefreshUnknownClass = "write_failed"
)

// Label returns the fixed word displayed for a recognized outcome.
// An invalid value has no label, so arbitrary marker strings are never rendered.
func (c RefreshUnknownClass) Label() string {
	switch c {
	case RefreshUnknownAmbiguous:
		return "ambiguous"
	case RefreshUnknownServerError:
		return "server_error"
	case RefreshUnknownRateLimited:
		return "rate_limited"
	case RefreshUnknownInterrupted:
		return "interrupted"
	case RefreshUnknownTLS:
		return "tls"
	case RefreshUnknownWriteFailed:
		return "write_failed"
	default:
		return ""
	}
}

// Inflight records the digest prefix and instant of one attempted refresh.
type Inflight struct {
	SentDigest8 string    `json:"sent_digest8"`
	SentAt      time.Time `json:"sent_at"`
}

// EarliestRefresh records a server floor belonging to one grant.
type EarliestRefresh struct {
	GrantDigest8 string    `json:"grant_digest8"`
	At           time.Time `json:"at"`
}

// RefreshState contains durable non-secret policy facts for one namespace.
// RetryAfter is an optional count of whole seconds, not a Go duration.
type RefreshState struct {
	Schema          uint32               `json:"schema"`
	Inflight        *Inflight            `json:"inflight"`
	FloorMin        uint32               `json:"floor_min"`
	DidNotHelp      uint8                `json:"did_not_help"`
	AmbiguousSince  *time.Time           `json:"ambiguous_since"`
	Class           *RefreshUnknownClass `json:"class"`
	Resent          bool                 `json:"resent"`
	RetryAfter      *uint64              `json:"retry_after"`
	LastSentAt      *time.Time           `json:"last_sent_at"`
	EarliestRefresh *EarliestRefresh     `json:"earliest_refresh"`
	DeadDigest8     *string              `json:"dead_digest8"`
}

// NewRefreshState returns the initial policy state, with no send authorized.
func NewRefreshState() RefreshState {
	return RefreshState{Schema: refreshStateSchema, FloorMin: DefaultRefreshFloorMin}
}

// RefreshStateReadKind distinguishes absence from an unusable marker.
type RefreshStateReadKind string

const (
	// RefreshStatePresent means a marker was successfully parsed.
	RefreshStatePresent RefreshStateReadKind = "present"
	// RefreshStateAbsent means the marker or its directory does not exist.
	RefreshStateAbsent RefreshStateReadKind = "absent"
	// RefreshStateUnavailable means no refresh may be sent using this state.
	RefreshStateUnavailable RefreshStateReadKind = "unavailable"
)

// RefreshStateRead reports a read without treating an unreadable marker as absent.
type RefreshStateRead struct {
	Kind   RefreshStateReadKind
	State  RefreshState
	Reason string
}

// RefreshStateStore names one namespace's marker. Reading it creates nothing.
// Its unexported mutations are only available through a live namespace lock.
type RefreshStateStore struct {
	paths   *config.Paths
	root    string
	dir     string
	name    string
	shown   string
	user    string
	account string
}

// NewRefreshStateStore validates the namespace and names its read-only marker.
func NewRefreshStateStore(paths *config.Paths, user, account string) (*RefreshStateStore, error) {
	shown, err := paths.CodexRefreshStatePath(user, account)
	if err != nil {
		return nil, err
	}
	return &RefreshStateStore{paths: paths, root: paths.CodexRoot(), dir: paths.CodexStateDir(), name: filepath.Base(shown), shown: shown, user: user, account: account}, nil
}

// Load reads the marker without creating a directory or following a link.
// Only a missing file is absent; cancellation, malformed data and all other
// failures are unavailable, so they cannot authorize another POST.
func (s *RefreshStateStore) Load(ctx context.Context) RefreshStateRead {
	if err := ctx.Err(); err != nil {
		return RefreshStateRead{Kind: RefreshStateUnavailable, Reason: "cancelled before reading refresh state"}
	}
	dir, err := secret.OpenDirUnder(s.root, s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return RefreshStateRead{Kind: RefreshStateAbsent}
	}
	if err != nil {
		return RefreshStateRead{Kind: RefreshStateUnavailable, Reason: err.Error()}
	}
	defer func() { _ = unix.Close(dir) }()
	fd, err := unix.Openat(dir, s.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return RefreshStateRead{Kind: RefreshStateAbsent}
	}
	if err != nil {
		return RefreshStateRead{Kind: RefreshStateUnavailable, Reason: fmt.Sprintf("could not open `%s`: %v", s.shown, err)}
	}
	file := os.NewFile(uintptr(fd), s.shown)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return RefreshStateRead{Kind: RefreshStateUnavailable, Reason: fmt.Sprintf("could not stat `%s`", s.shown)}
	}
	if !info.Mode().IsRegular() || info.Size() > maxRefreshStateBytes {
		return RefreshStateRead{Kind: RefreshStateUnavailable, Reason: fmt.Sprintf("`%s` is not a small regular file", s.shown)}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRefreshStateBytes+1))
	if err != nil || int64(len(data)) > maxRefreshStateBytes {
		return RefreshStateRead{Kind: RefreshStateUnavailable, Reason: fmt.Sprintf("could not read `%s`", s.shown)}
	}
	state, err := parseRefreshState(data)
	if err != nil {
		return RefreshStateRead{Kind: RefreshStateUnavailable, Reason: fmt.Sprintf("`%s` %s", s.shown, err)}
	}
	return RefreshStateRead{Kind: RefreshStatePresent, State: state}
}

func parseRefreshState(data []byte) (RefreshState, error) {
	var fields map[string]jsontext.Value
	if json.Unmarshal(data, &fields) != nil || fields["schema"].Kind() != '0' {
		return RefreshState{}, errors.New("does not parse")
	}
	for _, name := range []string{"floor_min", "did_not_help", "resent"} {
		if value, exists := fields[name]; exists && value.Kind() == 'n' {
			return RefreshState{}, errors.New("does not parse")
		}
	}
	for name, required := range map[string][]string{"inflight": {"sent_digest8", "sent_at"}, "earliest_refresh": {"grant_digest8", "at"}} {
		if value, exists := fields[name]; exists && value.Kind() != 'n' {
			var nested map[string]jsontext.Value
			if json.Unmarshal(value, &nested) != nil {
				return RefreshState{}, errors.New("does not parse")
			}
			for _, field := range required {
				if nested[field].Kind() != '"' {
					return RefreshState{}, errors.New("does not parse")
				}
			}
		}
	}
	state := NewRefreshState()
	state.Schema = 0
	if err := json.Unmarshal(data, &state); err != nil {
		return RefreshState{}, errors.New("does not parse")
	}
	if state.Schema != refreshStateSchema {
		return RefreshState{}, fmt.Errorf("has schema %d, which this build does not read", state.Schema)
	}
	if state.Class != nil {
		switch *state.Class {
		case RefreshUnknownAmbiguous, RefreshUnknownServerError, RefreshUnknownRateLimited, RefreshUnknownInterrupted, RefreshUnknownTLS, RefreshUnknownWriteFailed:
		default:
			return RefreshState{}, errors.New("does not parse")
		}
	}
	if state.Inflight != nil && !isCodexDigest8(state.Inflight.SentDigest8) {
		return RefreshState{}, errors.New("has an invalid inflight.sent_digest8")
	}
	if state.EarliestRefresh != nil && !isCodexDigest8(state.EarliestRefresh.GrantDigest8) {
		return RefreshState{}, errors.New("has an invalid earliest_refresh.grant_digest8")
	}
	if state.DeadDigest8 != nil && !isCodexDigest8(*state.DeadDigest8) {
		return RefreshState{}, errors.New("has an invalid dead_digest8")
	}
	return state, nil
}
