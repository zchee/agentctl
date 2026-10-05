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

package cli

import (
	"os"
	"syscall"
	"testing"
)

func TestSwapExitCodeTable(t *testing.T) {
	t.Parallel()

	// Each case name documents the condition the code reports, so the
	// table doubles as the human-readable contract.
	tests := map[string]struct {
		name string
		code int
	}{
		"success: 10 refused_a fires on a compromised hold or a non-unreachable acquisition failure": {
			name: "refused_a", code: SwapExitRefusedA,
		},
		"success: 11 refused_c fires when CLAUDE_CODE_OAUTH_TOKEN is non-empty": {
			name: "refused_c", code: SwapExitRefusedC,
		},
		"success: 12 refused_d fires when the encoded keychain line exceeds 4032 bytes with its newline": {
			name: "refused_d", code: SwapExitRefusedD,
		},
		"success: 13 refused_e fires on a live-target undo while the environment selects a namespace": {
			name: "refused_e", code: SwapExitRefusedE,
		},
		"success: 14 refused_f fires when the outgoing credential cannot be adopted safely": {
			name: "refused_f", code: SwapExitRefusedF,
		},
		"success: 15 precondition fires when the inherited namespace is not owned by this registry": {
			name: "precondition", code: SwapExitPrecondition,
		},
		"success: 16 busy fires when peer locks are held and not broken": {
			name: "busy", code: SwapExitBusy,
		},
		"success: 17 discarded fires when the item changed under the hold or the hold budget ran out": {
			name: "discarded", code: SwapExitDiscarded,
		},
		"success: 18 unknown fires when a timed-out write leaves verification inconclusive": {
			name: "unknown", code: SwapExitUnknown,
		},
		"success: 19 write_failed fires on a definite non-timeout write failure": {
			name: "write_failed", code: SwapExitWriteFailed,
		},
		"success: 20 cancelled fires when confirmation is declined or unavailable without --yes": {
			name: "cancelled", code: SwapExitCancelled,
		},
		"success: 21 needs_refresh fires on an expired incoming credential in a migrated store": {
			name: "needs_refresh", code: SwapExitNeedsRefresh,
		},
		"success: 22 audit_refused fires when the durable audit precondition fails": {
			name: "audit_refused", code: SwapExitAuditRefused,
		},
		"success: 23 live_unreachable fires when the live store is absent or dangles": {
			name: "live_unreachable", code: SwapExitLiveUnreachable,
		},
		"success: 24 live_item_absent fires when the live keychain item is absent": {
			name: "live_item_absent", code: SwapExitLiveItemAbsent,
		},
		"success: 27 live_undo_item_changed fires when an undo sees a third account's credential": {
			name: "live_undo_item_changed", code: SwapExitLiveUndoItemChanged,
		},
		"success: 29 identity_unavailable fires when nothing can say whose credential the live item holds": {
			name: "identity_unavailable", code: SwapExitIdentityUnavailable,
		},
		"success: 30 remote_control_not_disconnected fires when the remote-control preflight fails": {
			name: "remote_control_not_disconnected", code: SwapExitRCNotDisconnected,
		},
	}

	seen := make(map[string]int, len(SwapExitCodes))
	for _, entry := range SwapExitCodes {
		seen[entry.Name] = entry.Code
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			code, ok := seen[tt.name]
			if !ok {
				t.Fatalf("SwapExitCodes misses %q", tt.name)
			}
			if code != tt.code {
				t.Fatalf("SwapExitCodes[%q] = %d, want %d", tt.name, code, tt.code)
			}
		})
	}

	if len(tests) != len(SwapExitCodes) {
		t.Fatalf("the table holds %d entries, the test documents %d", len(SwapExitCodes), len(tests))
	}
}

func TestSwapExitCodesAreUniqueAndRetireTwentyFiveTwentySixAndTwentyEight(t *testing.T) {
	t.Parallel()

	if len(SwapExitCodes) != 18 {
		t.Fatalf("the table's size = %d, want 18", len(SwapExitCodes))
	}

	byCode := make(map[int]string, len(SwapExitCodes))
	for _, entry := range SwapExitCodes {
		if other, dup := byCode[entry.Code]; dup {
			t.Fatalf("%q and %q share code %d", other, entry.Name, entry.Code)
		}
		byCode[entry.Code] = entry.Name
		// 0, 1 and 2 are taken by the ordinary exit contract, and the
		// block starts at 10 so it reads as one.
		if entry.Code < 10 {
			t.Fatalf("%q uses code %d below the block's start", entry.Name, entry.Code)
		}
	}

	for _, retired := range []int{25, 26, 28} {
		if name, found := byCode[retired]; found {
			t.Fatalf("code %d is retired, not reused, but %q holds it", retired, name)
		}
	}

	if SwapExitIdentityUnavailable != 29 {
		t.Fatalf("identity_unavailable = %d, want the fresh 29", SwapExitIdentityUnavailable)
	}
	if SwapExitRCNotDisconnected != 30 {
		t.Fatalf("remote_control_not_disconnected = %d, want 30", SwapExitRCNotDisconnected)
	}
}

func TestSignalExitCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		signal os.Signal
		want   int
	}{
		"success: TERM exits 143":                 {signal: syscall.SIGTERM, want: 143},
		"success: HUP exits 129":                  {signal: syscall.SIGHUP, want: 129},
		"success: INT exits 130":                  {signal: syscall.SIGINT, want: 130},
		"error: a signal with no number is fatal": {signal: namelessSignal{}, want: 1},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := SignalExitCode(tt.signal); got != tt.want {
				t.Errorf("SignalExitCode(%v) = %d, want %d", tt.signal, got, tt.want)
			}
		})
	}
}

// namelessSignal is an os.Signal that is not a POSIX signal, the shape a
// platform-specific notification would take.
type namelessSignal struct{}

func (namelessSignal) String() string { return "nameless" }
func (namelessSignal) Signal()        {}
