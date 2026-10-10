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
	json "encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestParseVersion(t *testing.T) {
	tests := map[string]struct {
		text string
		want Version
		ok   bool
	}{
		"success: floor":                 {text: "2.1.287", want: Version{2, 1, 287}, ok: true},
		"success: installed":             {text: "2.1.296", want: Version{2, 1, 296}, ok: true},
		"success: leading zeros":         {text: "02.01.0287", want: Version{2, 1, 287}, ok: true},
		"success: largest component":     {text: "4294967295.0.0", want: Version{Major: 4294967295}, ok: true},
		"error: component overflows":     {text: "4294967296.0.0"},
		"error: two components":          {text: "2.1"},
		"error: four components":         {text: "2.1.287.1"},
		"error: empty":                   {},
		"error: empty component":         {text: "2..287"},
		"error: pre-release suffix":      {text: "2.1.287-beta"},
		"error: sign":                    {text: "+2.1.287"},
		"error: whitespace":              {text: " 2.1.287"},
		"error: non-ASCII digit":         {text: "2.1.٢٨٧"},
		"error: build metadata":          {text: "2.1.287+abc"},
		"error: trailing dot":            {text: "2.1.287."},
		"error: letters in a component":  {text: "2.x.287"},
		"error: negative looking number": {text: "2.-1.287"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := ParseVersion(tt.text)
			if ok != tt.ok {
				t.Fatalf("ParseVersion(%q) ok=%v, want %v", tt.text, ok, tt.ok)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("ParseVersion(%q) mismatch (-want +got):\n%s", tt.text, diff)
			}
		})
	}
}

func TestVersionLess(t *testing.T) {
	tests := map[string]struct {
		a, b Version
		want bool
	}{
		"success: patch below the floor":   {a: Version{2, 1, 286}, b: RemoteControlMinVersion, want: true},
		"success: the floor itself":        {a: Version{2, 1, 287}, b: RemoteControlMinVersion},
		"success: minor decides":           {a: Version{2, 0, 999}, b: Version{2, 1, 0}, want: true},
		"success: major decides":           {a: Version{1, 99, 999}, b: Version{2, 0, 0}, want: true},
		"success: numeric not lexical":     {a: Version{2, 1, 99}, b: Version{2, 1, 287}, want: true},
		"success: newer is not less":       {a: Version{3, 0, 0}, b: RemoteControlMinVersion},
		"success: newer patch is not less": {a: Version{2, 1, 296}, b: RemoteControlMinVersion},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.a.Less(tt.b); got != tt.want {
				t.Fatalf("%v.Less(%v)=%v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
	if got := RemoteControlMinVersion.String(); got != "2.1.287" {
		t.Fatalf("floor renders as %q", got)
	}
}

func TestDecodeRegistryRecord(t *testing.T) {
	type view struct {
		PID                                uint32
		SessionID, Version, Status, Socket string
		Bridged                            bool
	}
	tests := map[string]struct {
		body    string
		want    view
		wantErr bool
	}{
		"success: every member the mod reads": {
			body: `{"pid":25430,"sessionId":"s1","cwd":"/w","version":"2.1.296","kind":"interactive","pidDomain":"host","tmux":null,"messagingSocketPath":"/tmp/x/25430.sock","name":"n","status":"idle","statusUpdatedAt":1,"bridgeSessionId":"secret-bridge"}`,
			want: view{PID: 25430, SessionID: "s1", Version: "2.1.296", Status: "idle", Socket: "/tmp/x/25430.sock", Bridged: true},
		},
		"success: a null bridge is no bridge":   {body: `{"pid":1,"bridgeSessionId":null}`, want: view{PID: 1}},
		"success: an empty bridge is no bridge": {body: `{"pid":1,"bridgeSessionId":""}`, want: view{PID: 1}},
		"success: missing pid decodes as zero":  {body: `{"sessionId":"s"}`, want: view{SessionID: "s"}},
		"error: not an object":                  {body: `[]`, wantErr: true},
		"error: truncated":                      {body: `{"pid":1,`, wantErr: true},
		"error: pid of the wrong type":          {body: `{"pid":"1"}`, wantErr: true},
		"error: negative pid":                   {body: `{"pid":-1}`, wantErr: true},
		"error: bridge of the wrong type":       {body: `{"pid":1,"bridgeSessionId":7}`, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			record, err := DecodeRegistryRecord([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			got := view{PID: record.PID, SessionID: record.SessionID, Version: record.Version, Status: record.Status, Socket: record.MessagingSocketPath, Bridged: record.Bridge.Present()}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("record mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBridgeMarkKeepsNoIDAndTellsBridgesApart(t *testing.T) {
	first, err := DecodeRegistryRecord([]byte(`{"pid":1,"bridgeSessionId":"bridge-one-SENTINEL"}`))
	if err != nil {
		t.Fatal(err)
	}
	same, _ := DecodeRegistryRecord([]byte(`{"pid":1,"bridgeSessionId":"bridge-one-SENTINEL"}`))
	other, _ := DecodeRegistryRecord([]byte(`{"pid":1,"bridgeSessionId":"bridge-two"}`))
	none, _ := DecodeRegistryRecord([]byte(`{"pid":1}`))
	tests := map[string]struct {
		a, b BridgeMark
		want bool
	}{
		"success: same bridge is not replaced":             {a: first.Bridge, b: same.Bridge},
		"success: another bridge is replaced":              {a: first.Bridge, b: other.Bridge, want: true},
		"success: a vanished bridge is not replaced":       {a: first.Bridge, b: none.Bridge},
		"success: a new bridge after none is not replaced": {a: none.Bridge, b: other.Bridge},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.a.Replaced(tt.b); got != tt.want {
				t.Fatalf("Replaced=%v, want %v", got, tt.want)
			}
		})
	}
	if strings.Contains(fmt.Sprintf("%#v %+v", first, first), "SENTINEL") {
		t.Fatal("the decoded record keeps the bridge id")
	}
}

func TestRemoteControlProvenanceTruthTable(t *testing.T) {
	const home = "/Users/someone"
	explicit := LiveService + "-" + SHA8(home+"/.claude")
	literalTilde := LiveService + "-" + SHA8("~/.claude")
	isolated := LiveService + "-" + SHA8("/Volumes/work/cfg")
	set := func(value string) RemoteControlEnvValue { return RemoteControlEnvValue{Set: true, Value: value} }
	base := RemoteControlProvenance{Home: home, Authorized: true}
	with := func(edit func(p *RemoteControlProvenance)) RemoteControlProvenance {
		p := base
		edit(&p)
		return p
	}
	tests := map[string]struct {
		provenance RemoteControlProvenance
		service    string
		concerns   map[string]bool
	}{
		"success: both variables unset is the unsuffixed live item": {
			provenance: base,
			service:    LiveService,
			concerns:   map[string]bool{LiveService: true, explicit: false},
		},
		"success: an explicit home config dir is a suffixed item": {
			provenance: with(func(p *RemoteControlProvenance) { p.ConfigDir = set(home + "/.claude") }),
			service:    explicit,
			concerns:   map[string]bool{LiveService: false, explicit: true},
		},
		"success: a literal tilde config dir is not the home spelling": {
			provenance: with(func(p *RemoteControlProvenance) { p.ConfigDir = set("~/.claude") }),
			service:    literalTilde,
			concerns:   map[string]bool{LiveService: false, explicit: false, literalTilde: true},
		},
		"success: a set but empty config dir is unsuffixed": {
			provenance: with(func(p *RemoteControlProvenance) { p.ConfigDir = set("") }),
			service:    LiveService,
			concerns:   map[string]bool{LiveService: true, explicit: false},
		},
		"success: a set but empty secure storage dir is unsuffixed whatever the config dir": {
			provenance: with(func(p *RemoteControlProvenance) {
				p.SecureStorageDir = set("")
				p.ConfigDir = set("/Volumes/work/cfg")
			}),
			service:  LiveService,
			concerns: map[string]bool{LiveService: true, isolated: false},
		},
		"success: a secure storage dir names its own item": {
			provenance: with(func(p *RemoteControlProvenance) { p.SecureStorageDir = set("/Volumes/work/cfg") }),
			service:    isolated,
			concerns:   map[string]bool{LiveService: false, isolated: true},
		},
		"success: an unset variable's stale value is ignored": {
			provenance: with(func(p *RemoteControlProvenance) {
				p.ConfigDir = RemoteControlEnvValue{Value: "/Volumes/work/cfg"}
			}),
			service:  LiveService,
			concerns: map[string]bool{LiveService: true, isolated: false},
		},
		"error: an OAuth token override is never concerned": {
			provenance: with(func(p *RemoteControlProvenance) { p.OAuthTokenSet = true }),
			service:    LiveService,
			concerns:   map[string]bool{LiveService: false},
		},
		"error: an API key is never concerned": {
			provenance: with(func(p *RemoteControlProvenance) { p.APIKeySet = true }),
			service:    LiveService,
			concerns:   map[string]bool{LiveService: false},
		},
		"error: a base URL is never concerned": {
			provenance: with(func(p *RemoteControlProvenance) { p.BaseURLSet = true }),
			service:    LiveService,
			concerns:   map[string]bool{LiveService: false},
		},
		"error: a session without host authorization is never concerned": {
			provenance: with(func(p *RemoteControlProvenance) { p.Authorized = false }),
			service:    LiveService,
			concerns:   map[string]bool{LiveService: false},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := tt.provenance.EnvView()
			if got := ServiceName(&env); got != tt.service {
				t.Fatalf("derived service %q, want %q", got, tt.service)
			}
			for service, want := range tt.concerns {
				if got := tt.provenance.Concerns(service); got != want {
					t.Errorf("Concerns(%q)=%v, want %v", service, got, want)
				}
			}
		})
	}

	unset := base.EnvView()
	explicitEnv := with(func(p *RemoteControlProvenance) { p.ConfigDir = set(home + "/.claude") }).EnvView()
	if SessionsDir(&unset) != SessionsDir(&explicitEnv) {
		t.Fatalf("the unset and explicit home rows must share one registry: %q vs %q", SessionsDir(&unset), SessionsDir(&explicitEnv))
	}
	if ServiceName(&unset) == ServiceName(&explicitEnv) {
		t.Fatal("the unset and explicit home rows must differ in service")
	}
}

func TestRemoteControlProvenanceDecodesTheModShape(t *testing.T) {
	body := `{"home":"/Users/someone","configDir":{"set":true,"value":"/c"},"secureStorageDir":{"set":false,"value":""},"oauthTokenSet":false,"apiKeySet":false,"baseUrlSet":true,"authorized":true}`
	var got RemoteControlProvenance
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	want := RemoteControlProvenance{Home: "/Users/someone", ConfigDir: RemoteControlEnvValue{Set: true, Value: "/c"}, BaseURLSet: true, Authorized: true}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("provenance mismatch (-want +got):\n%s", diff)
	}
}

func TestRemoteControlCountsJSONCarriesEveryCount(t *testing.T) {
	data, err := json.Marshal(RemoteControlCounts{Reconnected: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"eligible":0,"provenance_skipped":0,"unreachable":0,"unavailable":0,"version_rejected":0,"metadata_rejected":0,"dropped":0,"not_dropped":0,"reconnected":2,"not_confirmed":0,"already_connected":0,"restored":0}`
	if diff := gocmp.Diff(want, string(data)); diff != "" {
		t.Fatalf("counts JSON mismatch (-want +got):\n%s", diff)
	}
}

func TestRemoteControlCountResult(t *testing.T) {
	var got RemoteControlCounts
	for _, result := range []string{RemoteControlReconnected, RemoteControlAlreadyConnected, RemoteControlUnavailable, RemoteControlNotConfirmed, RemoteControlExpired, RemoteControlCancelled, "", "bogus", RemoteControlReconnected} {
		got.CountResult(result)
	}
	want := RemoteControlCounts{Reconnected: 2, AlreadyConnected: 1, Unavailable: 1, NotConfirmed: 5}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("counts mismatch (-want +got):\n%s", diff)
	}
}

func TestRemoteControlActionFor(t *testing.T) {
	tests := map[string]struct {
		outcome SwapOutcomeKind
		config  bool
		want    RemoteControlAction
	}{
		"success: applied with the config reconnects":     {outcome: SwapApplied, config: true, want: RemoteControlReconnect},
		"success: applied without the config recovers it": {outcome: SwapApplied, want: RemoteControlConfigRecovery},
		"success: unknown asks for status first":          {outcome: SwapUnknown, config: true, want: RemoteControlStatusRecovery},
		"success: unknown without config asks for status": {outcome: SwapUnknown, want: RemoteControlStatusRecovery},
		"success: already active keeps":                   {outcome: SwapAlreadyActive, config: true, want: RemoteControlKeep},
		"success: refused keeps":                          {outcome: SwapRefused, want: RemoteControlKeep},
		"success: cancelled keeps":                        {outcome: SwapCancelled, want: RemoteControlKeep},
		"success: failed keeps":                           {outcome: SwapFailed, want: RemoteControlKeep},
		"success: needs refresh keeps":                    {outcome: SwapNeedsRefresh, want: RemoteControlKeep},
		"success: discarded keeps":                        {outcome: SwapDiscarded, want: RemoteControlKeep},
		"success: busy keeps":                             {outcome: SwapBusy, want: RemoteControlKeep},
		"success: an unnamed outcome sends nothing":       {outcome: "later", config: true, want: RemoteControlKeep},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := RemoteControlActionFor(tt.outcome, tt.config); got != tt.want {
				t.Fatalf("action=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoteControlWarnings(t *testing.T) {
	tests := map[string]struct {
		counts  RemoteControlCounts
		results []string
		action  RemoteControlAction
		want    []string
	}{
		"success: all reconnected warns nothing": {
			counts: RemoteControlCounts{Eligible: 2, Dropped: 2, Reconnected: 2},
			action: RemoteControlReconnect,
		},
		"success: nothing eligible warns nothing in any action": {
			action: RemoteControlConfigRecovery,
		},
		"error: one not dropped": {
			counts: RemoteControlCounts{Eligible: 1, NotDropped: 1},
			action: RemoteControlReconnect,
			want:   []string{"1 Claude Code session still had the earlier Remote Control bridge 45 s after the swap, so agentctl did not ask it to start it again; Claude Code stops it on its next account check, then run `/remote-control` there"},
		},
		"error: two not confirmed with one expired": {
			counts:  RemoteControlCounts{Eligible: 2, Dropped: 2, NotConfirmed: 2},
			results: []string{RemoteControlExpired, ""},
			action:  RemoteControlReconnect,
			want:    []string{"2 Claude Code sessions did not confirm that Remote Control started again (1 of them expired before the session was idle); run `/remote-control` there"},
		},
		"error: one not confirmed that expired": {
			counts:  RemoteControlCounts{Eligible: 1, Dropped: 1, NotConfirmed: 1},
			results: []string{RemoteControlExpired},
			action:  RemoteControlReconnect,
			want:    []string{"1 Claude Code session did not confirm that Remote Control started again (its request expired before the session was idle); run `/remote-control` there"},
		},
		"error: unavailable": {
			counts: RemoteControlCounts{Eligible: 1, Dropped: 1, Unavailable: 1},
			action: RemoteControlReconnect,
			want:   []string{"agentctl cannot restart Remote Control in 1 Claude Code session automatically (the command is missing there, or the session answers through a gateway or a third-party provider); if Remote Control stops there, run `/remote-control` there"},
		},
		"error: version and metadata rejections come first": {
			counts: RemoteControlCounts{VersionRejected: 2, MetadataRejected: 1, Eligible: 1, Dropped: 1, Reconnected: 1},
			action: RemoteControlReconnect,
			want: []string{
				"2 Claude Code sessions with Remote Control on run a Claude Code release older than 2.1.287, or record none, so agentctl asked them nothing",
				"1 Claude Code session with Remote Control on refused agentctl's request because the request file's owner or mode was not what the mod expects, so agentctl asked it nothing more",
			},
		},
		"error: config recovery names the recovery first": {
			counts: RemoteControlCounts{Eligible: 3, VersionRejected: 1},
			action: RemoteControlConfigRecovery,
			want:   []string{"the swap applied but Claude Code's configuration was not updated, so agentctl asked no session to start Remote Control again: recover the configuration first, then run `/remote-control` in the 3 Claude Code sessions that had it on"},
		},
		"error: status recovery names status first": {
			counts: RemoteControlCounts{Eligible: 1},
			action: RemoteControlStatusRecovery,
			want:   []string{"the swap's outcome is unknown, so agentctl asked no session to start Remote Control again: run `agentctl claude status` first; if the credential changed, run `/remote-control` in the 1 Claude Code session that had it on once it stops there"},
		},
		"success: keep with every bridge still there warns nothing": {
			counts: RemoteControlCounts{Eligible: 2, Restored: 2},
			action: RemoteControlKeep,
		},
		"error: keep with a lost bridge": {
			counts: RemoteControlCounts{Eligible: 3, Restored: 1},
			action: RemoteControlKeep,
			want:   []string{"2 Claude Code sessions that had Remote Control on no longer show a bridge, although this pass changed no credential; run `/remote-control` there"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := RemoteControlWarnings(tt.counts, tt.results, tt.action)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("warnings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
