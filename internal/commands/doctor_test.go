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
	"bytes"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestDoctorReport(t *testing.T) {
	tests := map[string]struct {
		owned bool
		want  []string
	}{
		"success: empty store still renders all sections":                                       {want: []string{"store\n", "keychain\n", "accounts\n", "foreign items (never read)\n", "namespace locks\n", "held locks\n", "namespaces\n", "worth knowing\n", "isolation\n", "has no isolated sessions", "no config write recorded"}},
		"success: owned namespace reports pending and adopted credentials without reading them": {owned: true, want: []string{"pending write", "pending meta", "adopted copy", "stray tmp", doctorAnomalousFile, "Legacy agentctl artefact", "not a Claude Code mutex"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			paths := config.NewPaths(fixture.ConfigDir())
			if err := paths.EnsureDirs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tt.owned {
				fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
				ns := fixture.NamespaceDir(testutil.Acct, testutil.Org)
				if err := os.MkdirAll(ns, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, file := range []string{secret.AdoptedFile, secret.PendingFile, secret.PendingMetaFile, secret.RefreshLockName, secret.LegacyStorageWriteArtefact, ".credentials.json.tmp.1234abcd"} {
					doctorWrite(t, filepath.Join(ns, file), "sk-ant-private-marker")
				}
			}
			var out bytes.Buffer
			env := claude.EnvWithHome(fixture.Home())
			command := Doctor{Paths: paths, Env: &env, Reader: secret.DisabledReader{}, Out: &out}
			if err := command.Report(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("report missing %q:\n%s", want, &out)
				}
			}
			if strings.Contains(out.String(), "sk-ant-") {
				t.Fatalf("report leaked secret:\n%s", &out)
			}
			if !strings.HasSuffix(out.String(), "\n") {
				t.Fatal("report lacks trailing newline")
			}
		})
	}
}

func doctorWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorIsolation(t *testing.T) {
	fixture := testutil.New(t)
	fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
	paths := config.NewPaths(fixture.ConfigDir())
	registry, err := config.LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	env := claude.EnvWithHome(fixture.Home())
	live := claude.LiveStoreDir(&env)
	session := filepath.Join(paths.SessionRoot(), testutil.Acct, testutil.Org)
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	doctorWrite(t, filepath.Join(live, "settings.json"), `{"policySettings":{"disableSideloadFlags":true}}`)
	doctorWrite(t, filepath.Join(live, "history.jsonl"), "")
	doctorWrite(t, filepath.Join(live, "statsig"), "")
	doctorWrite(t, filepath.Join(live, "todos.json"), "[]")
	doctorWrite(t, claude.ClaudeJSONPath(&env), `{"mcpServers":{"a":{"env":{"TOKEN":"sk-ant-env-marker"}},"b":{"headers":{"Authorization":"sk-ant-header-marker"}},"c":{"env":{}}}}`)
	doctorWrite(t, filepath.Join(session, ".claude.json"), `{"theme":"dark","oauthAccount":{"uuid":"sk-ant-leaked-value"}}`)
	doctorWrite(t, filepath.Join(session, "CLAUDE.md"), "occupied")
	for name, target := range map[string]string{"settings.json": filepath.Join(live, "settings.json"), "mcp.json": claude.ClaudeJSONPath(&env), "skills": filepath.Join(live, "absent")} {
		if err := os.Symlink(target, filepath.Join(session, name)); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(session, ".claude.json"), old, old); err != nil {
		t.Fatal(err)
	}
	command := Doctor{Paths: paths, Env: &env}
	report := command.collectIsolation(registry, nil)
	document, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "doctor.v1.json", document)
	row := report.Rows[0]
	if diff := gocmp.Diff([]string{"statsig", "todos.json"}, row.Unexposed); diff != "" {
		t.Errorf("unexposed (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff([]string{"oauthAccount"}, row.LeakedKeys); diff != "" {
		t.Errorf("leaked keys (-want +got):\n%s", diff)
	}
	if row.MCP.CredentialEntries == nil || *row.MCP.CredentialEntries != 2 || !row.Drift.ChangedSinceSeed || !row.Exports.SHA8Match {
		t.Errorf("unexpected diagnostic metadata: %+v", row)
	}
	if strings.Contains(string(document), "sk-ant-") {
		t.Fatal("isolation model leaked a credential value")
	}
	rendered := strings.Join(doctorIsolationSection(&report, paths.SessionRoot()), "\n")
	for _, want := range []string{"credential_entries=2", "changed_since_seed=true", "occupied", "missing-target", "sha8_match=true", "leaked keys      oauthAccount"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q:\n%s", want, rendered)
		}
	}
	empty := doctorIsolation{Version: 1}
	document, err = json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "doctor.v1.json", document)
}

func TestDoctorConfigVerdict(t *testing.T) {
	tests := map[string]struct {
		file    string
		absent  bool
		account *secret.IncomingIdentity
		want    string
	}{
		"success: matching account":         {file: `{"oauthAccount":{"accountUuid":"account","organizationUuid":"org"}}`, account: &secret.IncomingIdentity{AccountUUID: "account", OrganizationUUID: new("org")}, want: "its account agrees with the file"},
		"success: mismatching account":      {file: `{"oauthAccount":{"accountUuid":"other"}}`, account: &secret.IncomingIdentity{AccountUUID: "account"}, want: "its account differs from the file"},
		"success: missing file":             {absent: true, account: &secret.IncomingIdentity{AccountUUID: "account"}, want: "the file is absent"},
		"success: absent recorded identity": {absent: true, want: "it recorded no account"},
		"success: no account":               {file: `{}`, account: &secret.IncomingIdentity{AccountUUID: "account"}, want: "the file names no account"},
		"error: unreadable JSON":            {file: `broken`, account: &secret.IncomingIdentity{AccountUUID: "account"}, want: "the file cannot be read"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".claude.json")
			if !tt.absent {
				doctorWrite(t, path, tt.file)
			}
			if diff := gocmp.Diff(tt.want, doctorConfigAgreement(path, tt.account)); diff != "" {
				t.Errorf("agreement (-want +got):\n%s", diff)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "missing")
	tail := secret.AuditTail{Entries: []secret.AuditEntry{{TS: "write", PID: 1, Event: &secret.WriteEvent{Target: secret.TargetLive, Outcome: secret.WriteApplied}}, {TS: "config", PID: 1, Event: &secret.ConfigWriteRecord{Outcome: secret.ConfigSkipped, Reason: new(secret.ConfigReasonAbsent), After: new("write#1")}}}}
	if got := doctorConfigVerdict(&tail, path); got != "last config write skipped (absent) at config#1 after write#1; it recorded no account" {
		t.Errorf("unexpected config verdict: %s", got)
	}
	tail.Entries = append(tail.Entries, secret.AuditEntry{TS: "newest", PID: 2, Event: &secret.WriteEvent{Target: secret.TargetLive, Outcome: secret.WriteUnknown}})
	if got := doctorConfigVerdict(&tail, path); got != "the newest live write (newest#2) has no config write after it" {
		t.Errorf("unexpected newest write verdict: %s", got)
	}
}

func TestDoctorHeldRecordAndNamespaceBody(t *testing.T) {
	fixture := testutil.New(t)
	paths := config.NewPaths(fixture.ConfigDir())
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatal(err)
	}
	guard, err := secret.Acquire(t.Context(), paths.LocksDir(), "account.org.lock", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := guard.Release(); err != nil {
			t.Error(err)
		}
	}()
	env := claude.EnvWithHome(fixture.Home())
	command := Doctor{Paths: paths, Env: &env}
	output := strings.Join(command.lockSection(t.Context()), "\n")
	if !strings.Contains(output, "account.org.lock  pid") || !strings.Contains(output, "(alive)") {
		t.Errorf("missing real holder:\n%s", output)
	}
	record := secret.HeldLockRecord{WriterPID: uint32(os.Getpid()), Tree: secret.TreeLive, StoreDir: fixture.Home(), Paths: []string{filepath.Join(fixture.Home(), secret.RefreshLockName)}, TakenAt: time.Now().UTC().Format(time.RFC3339Nano)}
	document, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	doctorWrite(t, filepath.Join(secret.HeldLocksDir(paths), "held.json"), string(document))
	if err := os.Mkdir(record.Paths[0], 0o700); err != nil {
		t.Fatal(err)
	}
	output = strings.Join(command.heldLocksSection(t.Context()), "\n")
	if !strings.Contains(output, "held by pid") || strings.Contains(output, "leaked") {
		t.Errorf("wrong held record:\n%s", output)
	}
	if err := os.Remove(record.Paths[0]); err != nil {
		t.Fatal(err)
	}
	output = strings.Join(command.heldLocksSection(t.Context()), "\n")
	if !strings.Contains(output, "stale record, holding nothing") {
		t.Errorf("missing stale record:\n%s", output)
	}
}
