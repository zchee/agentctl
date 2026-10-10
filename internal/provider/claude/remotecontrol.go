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

// Remote Control restart after a live swap.
//
// Claude Code drops a session's Remote Control bridge by itself once the
// credential it reads changes. The agentctl Remote Control mod, running
// inside each session, can start a new bridge in the same process when
// agentctl asks it to. This file holds the rules both halves of that
// exchange are judged by: what a registry record says, which sessions a
// swap concerns, what the counts mean and which warnings they produce.
// Every statement about a bridge is an observation of the registry, never
// a claim about the account or about the conversation history claude.ai
// keeps.

package claude

import (
	"crypto/sha256"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// RemoteControlMinVersion is the oldest Claude Code release whose mod API
// the agentctl Remote Control mod is built against.
var RemoteControlMinVersion = Version{Major: 2, Minor: 1, Patch: 287}

// RemoteControlDropWaitSeconds is how long agentctl waits, after a swap,
// for a session's old bridge to disappear before it gives up on that
// session. It is named in a warning, so it lives beside the warning text.
const RemoteControlDropWaitSeconds = 45

// Version is a Claude Code release number.
type Version struct {
	Major, Minor, Patch uint32
}

// ParseVersion parses exactly three dot-separated ASCII decimal components.
// Anything else — a sign, a pre-release suffix, a missing or an extra
// component — reports false, because a version the floor cannot be
// compared against is treated as too old.
func ParseVersion(text string) (Version, bool) {
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var numbers [3]uint32
	for i, part := range parts {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return Version{}, false
		}
		number, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return Version{}, false
		}
		numbers[i] = uint32(number)
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2]}, true
}

// Less reports whether v is an earlier release than other.
func (v Version) Less(other Version) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor < other.Minor
	}
	return v.Patch < other.Patch
}

// String renders the version as Claude Code prints it.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// BridgeMark records whether a registry record names a Remote Control
// bridge, and a digest that tells two bridges apart. The bridge id itself
// is not kept, so it cannot reach a log, a warning or a report.
type BridgeMark struct {
	present bool
	digest  [sha256.Size]byte
}

// Present reports whether the record named a bridge.
func (m BridgeMark) Present() bool { return m.present }

// Replaced reports whether both marks name a bridge and the bridges differ.
func (m BridgeMark) Replaced(other BridgeMark) bool {
	return m.present && other.present && m.digest != other.digest
}

// RegistryRecord is the part of a Claude Code session registry record
// agentctl reads.
type RegistryRecord struct {
	// PID is the session's process id; zero when the record names none.
	PID uint32
	// SessionID is the session's own id.
	SessionID string
	// Version is the Claude Code release the session runs, as recorded.
	Version string
	// Status is the session's own activity word.
	Status string
	// Bridge marks the session's Remote Control bridge, when it has one.
	Bridge BridgeMark
	// MessagingSocketPath is where the session listens for local messages.
	MessagingSocketPath string
}

// ErrRegistryRecord reports a record that is not a JSON object of the
// expected shape.
var ErrRegistryRecord = errors.New("the session registry record is malformed")

// DecodeRegistryRecord decodes one registry record. Members other than
// the ones agentctl reads are ignored; a member of the wrong type rejects
// the record.
func DecodeRegistryRecord(body []byte) (RegistryRecord, error) {
	var document struct {
		PID                 *uint32 `json:"pid"`
		SessionID           *string `json:"sessionId"`
		Version             *string `json:"version"`
		Status              *string `json:"status"`
		Bridge              *string `json:"bridgeSessionId"`
		MessagingSocketPath *string `json:"messagingSocketPath"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return RegistryRecord{}, ErrRegistryRecord
	}
	text := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	record := RegistryRecord{
		SessionID:           text(document.SessionID),
		Version:             text(document.Version),
		Status:              text(document.Status),
		MessagingSocketPath: text(document.MessagingSocketPath),
	}
	if document.PID != nil {
		record.PID = *document.PID
	}
	if bridge := text(document.Bridge); bridge != "" {
		record.Bridge = BridgeMark{present: true, digest: sha256.Sum256([]byte(bridge))}
	}
	return record, nil
}

// RemoteControlEnvValue is one environment variable as the mod reports it:
// whether it is set at all, and its value when it is.
type RemoteControlEnvValue struct {
	Set   bool   `json:"set"`
	Value string `json:"value"`
}

// RemoteControlProvenance is the environment a running session reports
// for itself. The values are directory spellings and presence flags, never
// secrets.
type RemoteControlProvenance struct {
	Home             string                `json:"home"`
	ConfigDir        RemoteControlEnvValue `json:"configDir"`
	SecureStorageDir RemoteControlEnvValue `json:"secureStorageDir"`
	OAuthTokenSet    bool                  `json:"oauthTokenSet"`
	APIKeySet        bool                  `json:"apiKeySet"`
	BaseURLSet       bool                  `json:"baseUrlSet"`
	Authorized       bool                  `json:"authorized"`
}

// EnvView rebuilds the naming rule's inputs from the reported environment,
// so the session's keychain service comes from the same [ServiceName]
// agentctl applies to itself.
func (p RemoteControlProvenance) EnvView() EnvView {
	env := EnvView{Home: p.Home, OAuthTokenSet: p.OAuthTokenSet}
	if p.ConfigDir.Set {
		env.ConfigDir = new(p.ConfigDir.Value)
	}
	if p.SecureStorageDir.Set {
		env.SecureStorageDir = new(p.SecureStorageDir.Value)
	}
	return env
}

// Overridden reports whether the session reads its credential from
// somewhere other than a keychain item, or answers through a gateway or a
// third-party provider: then a swap of any item cannot be what drops its
// bridge, and a reconnect is not agentctl's to ask for.
func (p RemoteControlProvenance) Overridden() bool {
	return p.OAuthTokenSet || p.APIKeySet || p.BaseURLSet || !p.Authorized
}

// Concerns reports whether a swap of the keychain item named service is
// the one this session reads.
func (p RemoteControlProvenance) Concerns(service string) bool {
	if p.Overridden() {
		return false
	}
	env := p.EnvView()
	return ServiceName(&env) == service
}

// RemoteControlCounts is the Remote Control part of a swap's outcome:
// numbers of sessions only, never names, ids or paths.
type RemoteControlCounts struct {
	// Eligible sessions had a bridge, answered, and read the swapped item.
	Eligible int `json:"eligible"`
	// ProvenanceSkipped sessions read another item, an override, or a gateway.
	ProvenanceSkipped int `json:"provenance_skipped"`
	// Unreachable sessions did not answer through the mod, or refused the
	// request for a reason other than the file's metadata or the release.
	Unreachable int `json:"unreachable"`
	// Unavailable sessions answered that Remote Control cannot start there.
	Unavailable int `json:"unavailable"`
	// VersionRejected sessions run a release older than the mod supports.
	VersionRejected int `json:"version_rejected"`
	// MetadataRejected sessions refused the request file's owner or mode.
	MetadataRejected int `json:"metadata_rejected"`
	// Dropped sessions lost their old bridge after the swap.
	Dropped int `json:"dropped"`
	// NotDropped sessions still had their old bridge when the wait ended.
	NotDropped int `json:"not_dropped"`
	// Reconnected sessions reported a new bridge.
	Reconnected int `json:"reconnected"`
	// NotConfirmed sessions gave no final answer, or one that is not success.
	NotConfirmed int `json:"not_confirmed"`
	// AlreadyConnected sessions had a bridge again before any request ran.
	AlreadyConnected int `json:"already_connected"`
	// Restored sessions still had a bridge after a pass that changed nothing.
	Restored int `json:"restored"`
}

// Result words a reconnect request can end in, as the mod writes them.
const (
	RemoteControlReconnected      = "reconnected"
	RemoteControlAlreadyConnected = "already_connected"
	RemoteControlUnavailable      = "unavailable"
	RemoteControlNotConfirmed     = "not_confirmed"
	RemoteControlExpired          = "expired"
	RemoteControlCancelled        = "cancelled"
)

// CountResult adds one reconnect result word to the counts. An unknown or
// missing word is never success: it counts as not confirmed.
func (c *RemoteControlCounts) CountResult(result string) {
	switch result {
	case RemoteControlReconnected:
		c.Reconnected++
	case RemoteControlAlreadyConnected:
		c.AlreadyConnected++
	case RemoteControlUnavailable:
		c.Unavailable++
	default:
		c.NotConfirmed++
	}
}

// RemoteControlAction is what the follow-up does after a swap pass.
type RemoteControlAction uint8

const (
	// RemoteControlReconnect waits for each old bridge to drop, then asks
	// for a new one: the item changed and so did the configuration.
	RemoteControlReconnect RemoteControlAction = iota + 1
	// RemoteControlConfigRecovery sends nothing: the item changed but the
	// configuration did not, which has to be recovered first.
	RemoteControlConfigRecovery
	// RemoteControlStatusRecovery sends nothing: nobody knows whether the
	// item changed, which `agentctl claude status` has to settle first.
	RemoteControlStatusRecovery
	// RemoteControlKeep sends nothing: the pass changed no credential, so
	// it only counts the sessions that still have a bridge.
	RemoteControlKeep
)

// RemoteControlActionFor maps a swap outcome, and whether its
// configuration step applied, to the follow-up. Only an applied outcome
// with an applied configuration sends requests, so an outcome added later
// falls into sending nothing.
func RemoteControlActionFor(outcome SwapOutcomeKind, configApplied bool) RemoteControlAction {
	switch outcome {
	case SwapApplied:
		if configApplied {
			return RemoteControlReconnect
		}
		return RemoteControlConfigRecovery
	case SwapUnknown:
		return RemoteControlStatusRecovery
	default:
		// Already active, refused, cancelled, failed, needs refresh,
		// discarded and busy all leave the credential as it was.
		return RemoteControlKeep
	}
}

// RemoteControlWarnings derives the follow-up's warnings from the counts,
// the action, the multiset of reconnect result words (only the number of
// expired requests is read from it), and the reasons of the reconnect
// requests the mod refused after the old bridge dropped. A session that was
// never eligible is the plain completion warning's to name, not these.
func RemoteControlWarnings(counts RemoteControlCounts, results, rejections []string, action RemoteControlAction) []string {
	var warnings []string
	add := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	switch action {
	case RemoteControlReconnect:
		// A refused reconnect is counted under its reason's class, but the
		// session lost its bridge to the swap, so it needs its own warning
		// telling the user to start Remote Control by hand. A metadata
		// refusal is the one the metadata warning already words for both
		// moments; every other reason is named here.
		byReason := map[string]int{}
		for _, reason := range rejections {
			if reason != "metadata" {
				byReason[RemoteControlRejectionReason(reason)]++
			}
		}
		// The release-floor warning says agentctl asked those sessions
		// nothing, which is false for a session refused after the drop.
		if n := max(counts.VersionRejected-byReason["version"], 0); n != 0 {
			add("%s with Remote Control on %s a Claude Code release older than %s, or %s none, so agentctl asked %s nothing", sessionCount(n), verb(n, "runs", "run"), RemoteControlMinVersion, verb(n, "records", "record"), pronoun(n))
		}
		if counts.MetadataRejected != 0 {
			add("%s with Remote Control on refused agentctl's request because the request file's owner or mode was not what the mod expects, so agentctl asked %s nothing more", sessionCount(counts.MetadataRejected), pronoun(counts.MetadataRejected))
		}
		for _, reason := range slices.Sorted(maps.Keys(byReason)) {
			n := byReason[reason]
			add("Remote Control was not restarted in %s that refused agentctl's request (reason: %s); run `/remote-control` there", sessionCount(n), reason)
		}
		if counts.NotDropped != 0 {
			add("%s still had the earlier Remote Control bridge %d s after the swap, so agentctl did not ask %s to start it again; Claude Code stops it on its next account check, then run `/remote-control` there", sessionCount(counts.NotDropped), RemoteControlDropWaitSeconds, pronoun(counts.NotDropped))
		}
		if counts.Unavailable != 0 {
			add("agentctl cannot restart Remote Control in %s automatically (the command is missing there, or the session answers through a gateway or a third-party provider); if Remote Control stops there, run `/remote-control` there", sessionCount(counts.Unavailable))
		}
		if counts.NotConfirmed != 0 {
			expired := 0
			for _, result := range results {
				if result == RemoteControlExpired {
					expired++
				}
			}
			clause := ""
			if expired != 0 {
				clause = fmt.Sprintf(" (%d of them expired before the session was idle)", expired)
				if counts.NotConfirmed == 1 {
					clause = " (its request expired before the session was idle)"
				}
			}
			add("%s did not confirm that Remote Control started again%s; run `/remote-control` there", sessionCount(counts.NotConfirmed), clause)
		}
	case RemoteControlConfigRecovery:
		if counts.Eligible != 0 {
			add("the swap applied but Claude Code's configuration was not updated, so agentctl asked no session to start Remote Control again: recover the configuration first, then run `/remote-control` in the %s that had it on", sessionCount(counts.Eligible))
		}
	case RemoteControlStatusRecovery:
		if counts.Eligible != 0 {
			add("the swap's outcome is unknown, so agentctl asked no session to start Remote Control again: run `agentctl claude status` first; if the credential changed, run `/remote-control` in the %s that had it on once it stops there", sessionCount(counts.Eligible))
		}
	case RemoteControlKeep:
		if lost := counts.Eligible - counts.Restored; lost > 0 {
			add("%s that had Remote Control on no longer %s a bridge, although this pass changed no credential; run `/remote-control` there", sessionCount(lost), verb(lost, "shows", "show"))
		}
	}
	return warnings
}

// sessionCount spells "N Claude Code session(s)".
func sessionCount(n int) string {
	if n == 1 {
		return "1 Claude Code session"
	}
	return fmt.Sprintf("%d Claude Code sessions", n)
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func verb(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// RemoteControlRejectionReason is the reason word a warning or a refusal
// prints. The mod writes it into a file agentctl only reads, so anything
// but a short lowercase word is replaced rather than echoed to the
// terminal.
func RemoteControlRejectionReason(reason string) string {
	if reason == "" {
		return "none given"
	}
	if len(reason) > 32 || strings.ContainsFunc(reason, func(r rune) bool { return (r < 'a' || r > 'z') && r != '_' }) {
		return "unrecognized"
	}
	return reason
}
