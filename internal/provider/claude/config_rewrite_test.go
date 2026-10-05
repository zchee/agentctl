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
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/secret"
)

func configFixture(t *testing.T, document string) (*EnvView, *Profile) {
	t.Helper()
	env := &EnvView{Home: t.TempDir()}
	if err := os.Mkdir(filepath.Join(env.Home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GlobalConfigPath(env), []byte(document), 0o640); err != nil {
		t.Fatal(err)
	}
	return env, &Profile{AccountUUID: "new-account", OrganizationUUID: "new-org", Email: "private@example.invalid", Document: jsontext.Value(`{"account":{"created_at":null,"display_name":"Name"},"organization":{"billing_type":"stripe"}}`)}
}

func assertNoConfigDebris(t *testing.T, env *EnvView, dir string) {
	t.Helper()
	if _, err := os.Lstat(GlobalConfigPath(env) + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains: %v", err)
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.Contains(name.Name(), ".tmp.") {
			t.Fatalf("temporary remains: %s", name.Name())
		}
	}
}

func TestConfigRewritePreservesBytesAndSymlink(t *testing.T) {
	before := "{\n  \"userID\": \"unchanged\",\n  \"oauthAccount\": {\n    \"extraOldKey\": 1\n  },\n  \"modelAccessCache\": {},\n  \"orgModelDefaultCache\": {},\n  \"cachedExtraUsageDisabledReason\": null,\n  \"cachedUsageUtilization\": {},\n  \"passesEligibilityCache\": {},\n  \"projects\": {\n    \"arbitrary\": [\n      1,\n      2\n    ]\n  }\n}"
	env, profile := configFixture(t, before)
	literal := GlobalConfigPath(env)
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "settings")
	if err := os.Rename(literal, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, literal); err != nil {
		t.Fatal(err)
	}
	report := RewriteConfig(t.Context(), env, profile, nil)
	if report.Outcome != secret.ConfigApplied {
		t.Fatalf("rewrite: %+v", report)
	}
	if report.Backup == nil || report.FromSHA8 == nil || report.ToSHA8 == nil || report.HoldMS == nil {
		t.Fatalf("incomplete report: %+v", report)
	}
	if link, err := os.Readlink(literal); err != nil || link != target {
		t.Fatalf("link %q: %v", link, err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]jsontext.Value
	if err := json.Unmarshal(after, &document); err != nil {
		t.Fatal(err)
	}
	planned, err := Plan([]byte(before), document["oauthAccount"])
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(string(planned.Updated()), string(after)); diff != "" {
		t.Fatal(diff)
	}
	if strings.Contains(string(after), "extraOldKey") || strings.Contains(string(after), "Cache") {
		t.Fatalf("stale fields remain: %s", after)
	}
	backup, err := os.ReadFile(filepath.Join(BackupsDir(env), *report.Backup))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(before, string(backup)); diff != "" {
		t.Fatal(diff)
	}
	for path, want := range map[string]os.FileMode{target: 0o640, filepath.Join(BackupsDir(env), *report.Backup): 0o600, BackupsDir(env): 0o700} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s mode=%o want=%o", path, st.Mode().Perm(), want)
		}
	}
	assertNoConfigDebris(t, env, targetDir)
}

func TestConfigRewriteReadOnlyRefusals(t *testing.T) {
	tests := map[string]struct {
		body    string
		reason  secret.ConfigReason
		outcome secret.ConfigOutcome
		setup   func(*testing.T, *EnvView)
	}{
		"error: compact object": {body: `{"a":1}`, reason: secret.ConfigReasonNotReproducible, outcome: secret.ConfigRefused},
		"error: invalid JSON":   {body: `{`, reason: secret.ConfigReasonUnparseable, outcome: secret.ConfigRefused},
		"error: not object":     {body: `[]`, reason: secret.ConfigReasonNotAnObject, outcome: secret.ConfigRefused},
		"success: absent": {body: `{}`, reason: secret.ConfigReasonAbsent, outcome: secret.ConfigSkipped, setup: func(t *testing.T, e *EnvView) {
			if err := os.Remove(GlobalConfigPath(e)); err != nil {
				t.Fatal(err)
			}
		}},
		"error: fifo": {body: `{}`, reason: secret.ConfigReasonUnreadable, outcome: secret.ConfigRefused, setup: func(t *testing.T, e *EnvView) {
			path := GlobalConfigPath(e)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env, p := configFixture(t, tt.body)
			if tt.setup != nil {
				tt.setup(t, env)
			}
			report := RewriteConfig(t.Context(), env, p, nil)
			if report.Outcome != tt.outcome || report.Reason == nil || *report.Reason != tt.reason {
				t.Fatalf("report=%+v", report)
			}
			if _, err := os.Stat(BackupsDir(env)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preflight created backups: %v", err)
			}
			assertNoConfigDebris(t, env, env.Home)
		})
	}
}

func TestConfigRewriteRechecksUnderLock(t *testing.T) {
	tests := map[string]struct {
		change func(*testing.T, *EnvView)
		reason secret.ConfigReason
		want   string
	}{
		"error: file drift": {reason: secret.ConfigReasonChangedUnderLock, want: "{\n  \"peer\": true\n}", change: func(t *testing.T, e *EnvView) {
			if err := os.WriteFile(GlobalConfigPath(e), []byte("{\n  \"peer\": true\n}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		"error: lock drift": {reason: secret.ConfigReasonCompromised, want: "{}", change: func(t *testing.T, e *EnvView) {
			stamp := time.Now().Add(time.Hour)
			if err := os.Chtimes(GlobalConfigPath(e)+".lock", stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env, p := configFixture(t, "{}")
			check, stop := configPreflight(env, p, false)
			if stop != nil {
				t.Fatal(stop)
			}
			report := configResolveWrite(t.Context(), check, configWriteInput{env: env, profile: p}, configHoldHooks{afterTemp: func() { tt.change(t, env) }})
			if report.Outcome != secret.ConfigAborted || report.Reason == nil || *report.Reason != tt.reason {
				t.Fatalf("report=%+v", report)
			}
			data, err := os.ReadFile(GlobalConfigPath(env))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, string(data)); diff != "" {
				t.Fatal(diff)
			}
			if report.Backup == nil || report.ToSHA8 != nil {
				t.Fatalf("backup/result=%+v", report)
			}
			assertNoConfigDebris(t, env, env.Home)
		})
	}
}

func TestConfigCatchUpIsReadOnlyUntilConfirmed(t *testing.T) {
	tests := map[string]struct {
		body    string
		current bool
	}{
		"success: current compact": {body: `{"oauthAccount":{"accountUuid":"new-account","organizationUuid":"new-org"},"modelAccessCache":{}}`, current: true},
		"success: current full":    {body: "{\n  \"oauthAccount\": {\n    \"accountUuid\": \"new-account\",\n    \"organizationUuid\": \"new-org\"\n  }\n}", current: true},
		"success: stale":           {body: "{}"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env, p := configFixture(t, tt.body)
			check, report := CheckConfigCatchUp(env, p)
			if tt.current {
				if check != nil || report == nil || report.Reason == nil || *report.Reason != secret.ConfigReasonAlreadyCurrent {
					t.Fatalf("check=%v report=%+v", check, report)
				}
			} else if report != nil || check == nil {
				t.Fatalf("check=%v report=%+v", check, report)
			}
			if _, err := os.Stat(BackupsDir(env)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("backups exist: %v", err)
			}
			assertNoConfigDebris(t, env, env.Home)
			if !tt.current {
				report := WriteConfigCatchUp(t.Context(), check, env, p, nil)
				if report.Outcome != secret.ConfigApplied {
					t.Fatalf("write: %+v", report)
				}
			}
		})
	}
}

func TestConfigCatchUpRechecksCurrentAccount(t *testing.T) {
	env, p := configFixture(t, "{}")
	check, report := CheckConfigCatchUp(env, p)
	if report != nil {
		t.Fatal(report)
	}
	current := "{\n  \"oauthAccount\": {\n    \"accountUuid\": \"new-account\",\n    \"organizationUuid\": \"new-org\"\n  }\n}"
	if err := os.WriteFile(GlobalConfigPath(env), []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	got := WriteConfigCatchUp(t.Context(), check, env, p, nil)
	if got.Outcome != secret.ConfigSkipped || got.Reason == nil || *got.Reason != secret.ConfigReasonAlreadyCurrent || got.Backup != nil {
		t.Fatalf("report=%+v", got)
	}
	assertNoConfigDebris(t, env, env.Home)
}

func TestConfigRewriteForeignLockAndBackupRefusals(t *testing.T) {
	tests := map[string]struct {
		setup  func(*testing.T, *EnvView)
		reason secret.ConfigReason
	}{
		"error: fresh lock": {reason: secret.ConfigReasonLockBusy, setup: func(t *testing.T, e *EnvView) {
			if err := os.Mkdir(GlobalConfigPath(e)+".lock", 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		"error: stale lock": {reason: secret.ConfigReasonLockStale, setup: func(t *testing.T, e *EnvView) {
			p := GlobalConfigPath(e) + ".lock"
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().Add(-time.Hour)
			if err := os.Chtimes(p, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}},
		"error: symlink backups": {reason: secret.ConfigReasonBackupUnwritable, setup: func(t *testing.T, e *EnvView) {
			if err := os.Symlink(t.TempDir(), BackupsDir(e)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env, p := configFixture(t, "{}")
			tt.setup(t, env)
			report := RewriteConfig(t.Context(), env, p, nil)
			if report.Reason == nil || *report.Reason != tt.reason {
				t.Fatalf("report=%+v", report)
			}
			data, err := os.ReadFile(GlobalConfigPath(env))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "{}" {
				t.Fatalf("file changed: %s", data)
			}
			if tt.reason != secret.ConfigReasonBackupUnwritable {
				if _, err := os.Stat(GlobalConfigPath(env) + ".lock"); err != nil {
					t.Fatalf("foreign lock removed: %v", err)
				}
			}
		})
	}
}

type configTestClock struct {
	secret.Clock
	offset time.Duration
}

func (c *configTestClock) Monotonic() time.Duration { return c.Clock.Monotonic() + c.offset }

func TestConfigRewriteBudgetGates(t *testing.T) {
	tests := map[string]struct{ term int }{"error: lock": {0}, "error: read": {1}, "error: transform": {2}, "error: backup": {3}, "error: temp": {4}, "error: recheck": {5}, "error: rename": {6}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env, p := configFixture(t, "{}")
			clock := &configTestClock{Clock: secret.SystemClock()}
			seams := secret.RealSeams(clock)
			check, stop := configPreflight(env, p, false)
			if stop != nil {
				t.Fatal(stop)
			}
			report := configResolveWrite(t.Context(), check, configWriteInput{env: env, profile: p, seams: seams}, configHoldHooks{beforeTerm: func(term int) {
				if term == tt.term {
					clock.offset = time.Second * 2
				}
			}})
			if report.Outcome != secret.ConfigAborted || report.Reason == nil || *report.Reason != secret.ConfigReasonBudget {
				t.Fatalf("report=%+v", report)
			}
			data, err := os.ReadFile(GlobalConfigPath(env))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "{}" {
				t.Fatal("budget wrote file")
			}
			assertNoConfigDebris(t, env, env.Home)
		})
	}
}

func TestConfigRewriteCancellationDoesNotRemoveHolder(t *testing.T) {
	env, p := configFixture(t, "{}")
	path := GlobalConfigPath(env) + ".lock"
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report := RewriteConfig(ctx, env, p, nil)
	if report.Reason == nil || *report.Reason != secret.ConfigReasonCancelled {
		t.Fatalf("report=%+v", report)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("holder removed", err)
	}
}

func TestBuildOAuthAccount(t *testing.T) {
	tests := map[string]struct {
		doc  string
		want string
	}{
		"success: defaults":            {doc: `{}`, want: `{"accountUuid":"a","emailAddress":"e","organizationUuid":"o","hasExtraUsageEnabled":false,"ccOnboardingFlags":{},"claudeCodeTrialEndsAt":null,"claudeCodeTrialDurationDays":null,"seatTier":null,"profileFetchedAt":7}`},
		"success: values retain types": {doc: `{"account":{"created_at":null,"display_name":[],"full_name":false},"organization":{"billing_type":null,"has_extra_usage_enabled":0,"cc_onboarding_flags":null,"seat_tier":"x","subscription_created_at":false}}`, want: `{"accountUuid":"a","emailAddress":"e","organizationUuid":"o","hasExtraUsageEnabled":0,"accountCreatedAt":null,"subscriptionCreatedAt":false,"ccOnboardingFlags":{},"claudeCodeTrialEndsAt":null,"claudeCodeTrialDurationDays":null,"seatTier":"x","displayName":[],"profileFetchedAt":7}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := &Profile{AccountUUID: "a", Email: "e", OrganizationUUID: "o", Document: jsontext.Value(tt.doc)}
			if diff := gocmp.Diff(tt.want, string(BuildOAuthAccount(p, 7))); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
