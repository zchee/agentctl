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
	"fmt"
	"strings"
)

const doctorStateWarning = "removing agentctl's Codex refresh-marker directory re-arms one refresh send per namespace; agentctl cannot tell a deleted marker from a refresh that was never sent"

// Field order is the wire order. Optional facts remain explicit JSON nulls.
type doctorReport struct {
	Version           uint32            `json:"version"`
	Home              doctorHome        `json:"home"`
	Store             doctorStore       `json:"store"`
	Environment       []doctorEnv       `json:"environment"`
	Live              doctorLive        `json:"live"`
	Foreign           doctorForeign     `json:"foreign"`
	Namespaces        []doctorNamespace `json:"namespaces"`
	Orphans           []doctorOrphan    `json:"orphans"`
	UnnameableOrphans int               `json:"unnameable_orphans"`
	Audit             []string          `json:"audit"`
	Notes             []string          `json:"notes"`
}

type doctorHome struct {
	Path         *string  `json:"path"`
	SymlinkChain []string `json:"symlink_chain"`
	Error        *string  `json:"error"`
}

type doctorStore struct {
	Mode              string  `json:"mode"`
	Read              string  `json:"read"`
	CoarseMatch       bool    `json:"coarse_match"`
	ProfilesConsulted bool    `json:"profiles_consulted"`
	BaseURL           *string `json:"base_url"`
	ConfigNote        *string `json:"config_note"`
}

type doctorEnv struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
}

type doctorLive struct {
	State               string   `json:"state"`
	AuthMode            *string  `json:"auth_mode"`
	ModeBits            *string  `json:"mode_bits"`
	ModeWarning         *string  `json:"mode_warning"`
	Size                *uint64  `json:"size"`
	AccessExpiry        *string  `json:"access_expiry"`
	LastRefresh         *string  `json:"last_refresh"`
	MatchesNamespace    *string  `json:"matches_namespace"`
	Daemon              string   `json:"daemon"`
	MissingKnownMembers []string `json:"missing_known_members"`
	UnknownMemberCount  int      `json:"unknown_member_count"`
}

type doctorForeign struct {
	MultiAuthPresent    bool     `json:"multi_auth_present"`
	SwitcherItems       int      `json:"switcher_items"`
	CodexAuthItems      int      `json:"codex_auth_items"`
	UnexplainedRemovals []string `json:"unexplained_removals"`
	UnexplainedItems    int      `json:"unexplained_items"`
	UnnameableItems     int      `json:"unnameable_items"`
}

type doctorNamespace struct {
	User               string       `json:"user"`
	Acct               string       `json:"acct"`
	Path               string       `json:"path"`
	CredentialsPresent bool         `json:"credentials_present"`
	RefreshPolicy      string       `json:"refresh_policy"`
	Lock               *doctorLock  `json:"lock"`
	Artefacts          []string     `json:"artefacts"`
	Marker             doctorMarker `json:"marker"`
	Notes              []string     `json:"notes"`
}

type doctorLock struct {
	PID        uint32 `json:"pid"`
	AcquiredAt string `json:"acquired_at"`
	Holder     string `json:"holder"`
}

type doctorMarker struct {
	State           string  `json:"state"`
	Unavailable     *string `json:"unavailable"`
	InflightDigest8 *string `json:"inflight_digest8"`
	InflightAge     *string `json:"inflight_age"`
	Class           *string `json:"class"`
	FloorMin        *uint32 `json:"floor_min"`
	DidNotHelp      *uint8  `json:"did_not_help"`
	Resent          *bool   `json:"resent"`
	AmbiguousSince  *string `json:"ambiguous_since"`
	ResendEligible  *bool   `json:"resend_eligible"`
}

type doctorOrphan struct {
	Kind    string  `json:"kind"`
	Subject string  `json:"subject"`
	Age     *string `json:"age"`
}

func renderDoctor(report doctorReport) string {
	var out strings.Builder
	line := func(indent int, text string) {
		out.WriteString(strings.Repeat("  ", indent))
		out.WriteString(text)
		out.WriteByte('\n')
	}
	line(0, "codex home")
	switch {
	case report.Home.Path != nil:
		line(1, *report.Home.Path)
	case report.Home.Error != nil:
		line(1, *report.Home.Error)
	default:
		line(1, "unresolved")
	}
	for _, hop := range report.Home.SymlinkChain {
		line(1, "via "+hop)
	}
	store := report.Store
	line(0, "credential store")
	line(1, fmt.Sprintf("mode %s (%s)", store.Mode, store.Read))
	if store.CoarseMatch {
		line(1, "coarse match: the keychain listing has no account column")
	}
	if !store.ProfilesConsulted {
		line(1, "profiles not consulted")
	}
	if store.BaseURL != nil {
		line(1, "chatgpt_base_url "+*store.BaseURL)
	}
	if store.ConfigNote != nil {
		line(1, *store.ConfigNote)
	}
	line(0, "environment")
	present := false
	for _, env := range report.Environment {
		state := "unset"
		if env.Present {
			state = "present"
			present = true
		}
		line(1, env.Name+" "+state)
	}
	if present {
		line(1, "a Codex session started with these may not be using the row above")
	}
	live := report.Live
	line(0, "live credential")
	line(1, live.State)
	if live.AuthMode != nil {
		line(1, "auth_mode "+*live.AuthMode)
	}
	if live.ModeBits != nil && live.Size != nil {
		line(1, fmt.Sprintf("mode %s, %d bytes", *live.ModeBits, *live.Size))
	}
	if live.ModeWarning != nil {
		line(1, *live.ModeWarning)
	}
	if live.AccessExpiry != nil {
		line(1, "access token "+*live.AccessExpiry)
	}
	if live.LastRefresh != nil {
		line(1, "last refresh "+*live.LastRefresh)
	}
	if live.MatchesNamespace != nil {
		line(1, "the same grant as the owned namespace "+*live.MatchesNamespace)
	}
	line(1, "daemon evidence: "+live.Daemon)
	if len(live.MissingKnownMembers) > 0 {
		line(1, "fields absent: "+strings.Join(live.MissingKnownMembers, ", "))
	}
	if live.UnknownMemberCount > 0 {
		line(1, fmt.Sprintf("%d field(s) this build does not know; a Codex upgrade may have added them", live.UnknownMemberCount))
	}
	foreign := report.Foreign
	line(0, "other credentials on this machine (never read)")
	line(1, "`multi-auth/` "+doctorPresence(foreign.MultiAuthPresent))
	line(1, fmt.Sprintf("codex-switcher keychain items: %d", foreign.SwitcherItems))
	line(1, fmt.Sprintf("`Codex Auth` keychain items: %d", foreign.CodexAuthItems))
	for _, command := range foreign.UnexplainedRemovals {
		line(1, "left by a refused agentctl login; remove it yourself with: "+command)
	}
	if foreign.UnexplainedItems > 0 {
		line(1, fmt.Sprintf("%d further `Codex Auth` item(s) are not this home's and are not agentctl's doing; one of them is likely another Codex home of yours, so agentctl offers no removal command for it", foreign.UnexplainedItems))
	}
	if foreign.UnnameableItems > 0 {
		line(1, fmt.Sprintf("%d further `Codex Auth` item(s) are listed under an account agentctl would not have written; open Keychain Access and look at them yourself — agentctl will not print an account string it did not make", foreign.UnnameableItems))
	}
	line(0, "owned namespaces")
	if len(report.Namespaces) == 0 {
		line(1, "none")
	}
	for _, ns := range report.Namespaces {
		line(1, ns.User+"+"+ns.Acct)
		line(2, ns.Path)
		line(2, "credentials "+doctorPresence(ns.CredentialsPresent)+", refresh "+ns.RefreshPolicy)
		if ns.Lock != nil {
			line(2, fmt.Sprintf("lock: pid %d since %s, %s", ns.Lock.PID, ns.Lock.AcquiredAt, ns.Lock.Holder))
		}
		for _, artifact := range ns.Artefacts {
			line(2, artifact)
		}
		marker := ns.Marker
		switch marker.State {
		case "absent":
			line(2, "refresh marker: none")
		case "unavailable":
			reason := "the marker could not be read"
			if marker.Unavailable != nil {
				reason = *marker.Unavailable
			}
			line(2, "refresh state unavailable: "+reason)
		default:
			line(2, "refresh marker:")
		}
		if marker.InflightDigest8 != nil && marker.InflightAge != nil {
			line(3, "send outstanding for "+*marker.InflightDigest8+", "+*marker.InflightAge+" ago")
		}
		if marker.Class != nil {
			line(3, "class "+*marker.Class)
		}
		if marker.AmbiguousSince != nil {
			line(3, "ambiguous refresh outstanding since "+*marker.AmbiguousSince)
		}
		if marker.FloorMin != nil {
			line(3, fmt.Sprintf("401 floor %d min", *marker.FloorMin))
		}
		if marker.DidNotHelp != nil {
			line(3, fmt.Sprintf("refresh did not help %d time(s)", *marker.DidNotHelp))
		}
		if marker.Resent != nil {
			line(3, fmt.Sprintf("re-send spent: %t", *marker.Resent))
		}
		if marker.ResendEligible != nil && *marker.ResendEligible {
			line(3, "--resend eligible")
		}
		for _, note := range ns.Notes {
			line(2, note)
		}
	}
	if len(report.Namespaces) > 0 {
		line(1, doctorStateWarning)
	}
	line(0, "left behind")
	if len(report.Orphans) == 0 && report.UnnameableOrphans == 0 {
		line(1, "nothing")
	}
	for _, orphan := range report.Orphans {
		text := orphan.Kind + " (" + orphan.Subject
		if orphan.Age != nil {
			text += ", " + *orphan.Age
		}
		line(1, text+")")
	}
	if report.UnnameableOrphans > 0 {
		line(1, fmt.Sprintf("%d further entr(y/ies) are named in a way agentctl would not have written; list the Codex directory yourself — agentctl will not print a name it did not make", report.UnnameableOrphans))
	}
	line(0, "write log")
	if len(report.Audit) == 0 {
		line(1, "no Codex write has been recorded")
	}
	for _, record := range report.Audit {
		line(1, record)
	}
	for _, note := range report.Notes {
		line(0, note)
	}
	return out.String()
}

func doctorPresence(present bool) string {
	if present {
		return "present"
	}
	return "absent"
}
