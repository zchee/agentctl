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
)

// everyState enumerates one value per state kind, payload-carrying kinds
// with a representative payload.
func everyState() []AccountState {
	return []AccountState{
		StateOfOK(),
		StateOfExpired(true),
		StateOfExpired(false),
		StateOfNeedsLogin(),
		StateOfIdentityUnknown(),
		StateOfStaleSiblingOfLive(),
		StateOfUnclaimed(),
		StateOfForeign("claude-switcher"),
		StateOfForgotten(),
		StateOfMigratedToKeychain("Claude Code-credentials-5cdc535f"),
		StateOfAdopted("someone@example.com"),
		StateOfClaudeSessionDetected(".oauth_refresh.lock", 12_000),
		StateOfKeychainLocked(""),
		StateOfKeychainLocked("user cancelled"),
		StateOfKeychainTimeout(),
		StateOfBusy(),
		StateOfLockUnavailable(),
		StateOfStale(),
		StateOfNoSubscriptionLimits(),
		StateOfPendingReplayed(),
		StateOfPendingDiscarded("file changed"),
		StateOfRateLimitedAfter(30),
		StateOfRateLimited(),
		StateOfRefreshDiscarded(),
		StateOfEnvToken(),
		StateOfError("something went wrong"),
	}
}

func TestEveryStateHasANonEmptyLabelAndName(t *testing.T) {
	t.Parallel()

	for _, state := range everyState() {
		label := state.Label()
		if label == "" {
			t.Errorf("%s has an empty label", state.Kind)
		}
		if strings.TrimSpace(label) != label {
			t.Errorf("%s has a padded label: %q", state.Kind, label)
		}
		if state.Name() == "" {
			t.Errorf("%s has an empty machine token", state.Kind)
		}
	}
}

func TestLabelsMatchTheTableWording(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state AccountState
		want  string
	}{
		"success: ok":               {state: StateOfOK(), want: "ok"},
		"success: needs login":      {state: StateOfNeedsLogin(), want: "needs login"},
		"success: identity unknown": {state: StateOfIdentityUnknown(), want: "identity unknown"},
		"success: stale sibling":    {state: StateOfStaleSiblingOfLive(), want: "stale sibling of live"},
		"success: unclaimed":        {state: StateOfUnclaimed(), want: "unclaimed"},
		"success: keychain timeout": {state: StateOfKeychainTimeout(), want: "keychain timeout (transient)"},
		"success: busy":             {state: StateOfBusy(), want: "busy"},
		"success: no subscription limits": {
			state: StateOfNoSubscriptionLimits(),
			want:  "no subscription limits (API/console account?)",
		},
		"success: pending replayed": {state: StateOfPendingReplayed(), want: "pending replayed"},
		"success: pending discarded names its reason": {
			state: StateOfPendingDiscarded("file changed"),
			want:  "pending discarded: file changed",
		},
		"success: rate-limited renders its hint": {
			state: StateOfRateLimitedAfter(30),
			want:  "rate-limited (retry in 30s)",
		},
		"success: rate-limited claims no window the server never named": {
			state: StateOfRateLimited(),
			want:  "rate-limited",
		},
		"success: refresh discarded": {
			state: StateOfRefreshDiscarded(),
			want:  "refresh discarded: namespace changed during refresh",
		},
		"success: expired read-only names its owner": {
			state: StateOfExpired(true),
			want:  "expired (read-only; refreshed by its owner)",
		},
		"success: expired refreshable": {state: StateOfExpired(false), want: "expired"},
		"success: foreign names its owner": {
			state: StateOfForeign("claude-switcher"),
			want:  "foreign (claude-switcher)",
		},
		"success: migrated names the service": {
			state: StateOfMigratedToKeychain("Claude Code-credentials-5cdc535f"),
			want:  "migrated to keychain (Claude Code-credentials-5cdc535f)",
		},
		"success: adopted names the occupant": {
			state: StateOfAdopted("someone@example.com"),
			want:  "adopted (its keychain item is held by another identity: someone@example.com)",
		},
		"success: keychain locked without detail": {
			state: StateOfKeychainLocked(""),
			want:  "keychain locked",
		},
		"success: keychain locked with detail": {
			state: StateOfKeychainLocked("user cancelled"),
			want:  "keychain locked (user cancelled)",
		},
		"success: env token": {state: StateOfEnvToken(), want: "env token"},
		"success: error carries its reason": {
			state: StateOfError("something went wrong"),
			want:  "something went wrong",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.Label(); got != tt.want {
				t.Fatalf("Label() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestADetectedSessionNamesTheLockAndItsAgeInSeconds(t *testing.T) {
	t.Parallel()

	state := StateOfClaudeSessionDetected(".oauth_refresh.lock", 12_400)
	want := "claude session detected — refresh refused (lock .oauth_refresh.lock, age 12s)"
	if got := state.Label(); got != want {
		t.Fatalf("Label() = %q, want %q", got, want)
	}
}

func TestMachineTokensAreTheReportVocabulary(t *testing.T) {
	t.Parallel()

	want := map[AccountStateKind]string{
		StateOK:                    "ok",
		StateExpired:               "expired",
		StateNeedsLogin:            "needs_login",
		StateIdentityUnknown:       "identity_unknown",
		StateStaleSiblingOfLive:    "stale_sibling_of_live",
		StateUnclaimed:             "unclaimed",
		StateForeign:               "foreign",
		StateForgotten:             "forgotten",
		StateMigratedToKeychain:    "migrated_to_keychain",
		StateAdopted:               "adopted",
		StateClaudeSessionDetected: "claude_session_detected",
		StateKeychainLocked:        "keychain_locked",
		StateKeychainTimeout:       "keychain_timeout",
		StateBusy:                  "busy",
		StateLockUnavailable:       "lock_unavailable",
		StateStale:                 "stale",
		StateNoSubscriptionLimits:  "no_subscription_limits",
		StatePendingReplayed:       "pending_replayed",
		StatePendingDiscarded:      "pending_discarded",
		StateRateLimited:           "rate_limited",
		StateRefreshDiscarded:      "refresh_discarded",
		StateEnvToken:              "env_token",
		StateError:                 "error",
	}
	for kind, token := range want {
		if string(kind) != token {
			t.Errorf("kind %q must be the report token %q", kind, token)
		}
	}
	for _, state := range everyState() {
		if _, known := want[state.Kind]; !known {
			t.Errorf("state %q is not in the report vocabulary", state.Kind)
		}
	}
}

func TestInformationalStatesAreNotFailures(t *testing.T) {
	t.Parallel()

	// A row that is a true statement about the machine, or that produced
	// numbers, must not make the process exit with the partial status.
	for _, state := range []AccountState{
		StateOfOK(),
		StateOfUnclaimed(),
		StateOfStaleSiblingOfLive(),
		StateOfForeign("claude-switcher"),
		StateOfForgotten(),
		StateOfPendingReplayed(),
		StateOfEnvToken(),
		StateOfMigratedToKeychain("svc"),
		StateOfAdopted("someone@example.com"),
	} {
		if state.IsFailure() {
			t.Errorf("%s should not be a failure", state.Kind)
		}
	}
}

func TestDegradedStatesAreFailures(t *testing.T) {
	t.Parallel()

	for _, state := range []AccountState{
		StateOfExpired(true),
		StateOfExpired(false),
		StateOfNeedsLogin(),
		StateOfIdentityUnknown(),
		StateOfClaudeSessionDetected("l", 0),
		StateOfKeychainLocked(""),
		StateOfKeychainTimeout(),
		StateOfBusy(),
		StateOfLockUnavailable(),
		StateOfStale(),
		StateOfNoSubscriptionLimits(),
		StateOfPendingDiscarded("invalid"),
		StateOfRateLimited(),
		StateOfRefreshDiscarded(),
		StateOfError("boom"),
	} {
		if !state.IsFailure() {
			t.Errorf("%s should be a failure", state.Kind)
		}
	}
}

func TestARateLimitedRowNeverMakesAnotherRequest(t *testing.T) {
	t.Parallel()

	// The point of honouring a retry-after hint is not making the call.
	if StateOfRateLimitedAfter(30).AllowsNetwork() {
		t.Fatal("a hinted rate limit must not reach the network")
	}
	if StateOfRateLimited().AllowsNetwork() {
		t.Fatal("an unhinted rate limit must not reach the network")
	}
}

func TestAReadOnlyExpiredRowDoesNoNetworkButARefreshableOneDoes(t *testing.T) {
	t.Parallel()

	// A row only its owner refreshes has nothing to spend a request on.
	if StateOfExpired(true).AllowsNetwork() {
		t.Fatal("a read-only expired row must not reach the network")
	}
	if !StateOfExpired(false).AllowsNetwork() {
		t.Fatal("a refreshable expired row spends its request on the refresh")
	}
}

func TestADetectedSessionStillAllowsAUsageFetch(t *testing.T) {
	t.Parallel()

	// The refusal is about refreshing, which would rotate a token out
	// from under the session. Reading usage with the token already on
	// disk does not disturb anything.
	state := StateOfClaudeSessionDetected("l", 0)
	if !state.AllowsNetwork() {
		t.Fatal("a detected session still allows a usage fetch")
	}
	if !state.IsFailure() {
		t.Fatal("but the row is still degraded")
	}
}

func TestStatesWithNoCredentialDoNoNetwork(t *testing.T) {
	t.Parallel()

	for _, state := range []AccountState{
		StateOfNeedsLogin(),
		StateOfIdentityUnknown(),
		StateOfUnclaimed(),
		StateOfForgotten(),
		StateOfForeign("claude-switcher"),
		StateOfStaleSiblingOfLive(),
		StateOfKeychainLocked(""),
		StateOfKeychainTimeout(),
		StateOfBusy(),
		StateOfLockUnavailable(),
	} {
		if state.AllowsNetwork() {
			t.Errorf("%s should not reach the network", state.Kind)
		}
	}
}

func TestSourceTokensAreTheReportVocabulary(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		source Source
		want   string
	}{
		"success: keychain": {source: SourceKeychain, want: "keychain"},
		"success: file":     {source: SourceFile, want: "file"},
		"success: env":      {source: SourceEnv, want: "env"},
		"success: none":     {source: SourceNone, want: "none"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tt.source.Name(); got != tt.want {
				t.Fatalf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}
