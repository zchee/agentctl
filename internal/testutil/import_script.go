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

package testutil

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"
)

func init() { registerScriptCmd("importfixture", importFixture) }

func importFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: importfixture <keychain|owned-import|alias|owned|readonly|relocate|occupied|hidden>")
	}
	kind := args[0]
	root := ts.MkAbs(filepath.Join("write-cases", kind))
	for _, dir := range []string{"config", "home", "items"} {
		ts.Check(os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	for key, value := range map[string]string{
		"AGENTCTL_CONFIG_DIR": filepath.Join(root, "config"), "HOME": filepath.Join(root, "home"),
		"AGCTL_FAKE_SECURITY_ITEMS": filepath.Join(root, "items"), "AGCTL_FAKE_SECURITY_DUMP": filepath.Join(root, "dump"), "AGCTL_FAKE_SECURITY_LOG": filepath.Join(root, "security.log"),
		"AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT": "0", "AGCTL_FAKE_SECURITY_FIND_EXIT": "", "AGCTL_FAKE_SECURITY_SLEEP": "", "CLAUDE_CONFIG_DIR": "",
		"ACCT": Acct, "ORG": Org, "UNKNOWN_ORG": UnknownOrg, "EMAIL": Email,
	} {
		ts.Setenv(key, value)
	}
	write := func(path string, value any) {
		body, err := json.Marshal(value)
		ts.Check(err)
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, body, 0o600))
	}
	blob := func(name, acct, org string) map[string]any {
		return map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": "sk-ant-oat01-SENTINEL-" + name, "refreshToken": "sk-ant-ort01-SENTINEL-" + name, "expiresAt": FreshAt(), "scopes": []string{"user:inference", "user:profile"},
			"tokenAccount": map[string]any{"uuid": acct, "organizationUuid": org, "emailAddress": Email, "organizationName": "Acme"},
		}}
	}
	var services []string
	item := func(service string, value any) {
		services = append(services, service)
		write(filepath.Join(root, "items", KeychainAccount, ItemFileName(service)), value)
	}
	owned := func(org string) map[string]any {
		spelling := ExportSpelling(filepath.Join(root, "config", "claude", Acct, org))
		return map[string]any{
			"account_uuid": Acct, "organization_uuid": org, "email": Email, "org_name": "Acme", "kind": map[string]any{"kind": "owned", "export_spelling": spelling, "export_sha8": Sha8(spelling)}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339),
		}
	}
	var records []map[string]any
	service := LiveService + "-deadbeef"
	switch kind {
	case "keychain":
		item(LiveService, blob("live", Acct, Org))
		item(LiveService+"-11112222", blob("first", "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", "cccccccc-3333-4333-8333-cccccccccccc"))
		item(LiveService+"-33334444", blob("second", "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb", "dddddddd-4444-4444-8444-dddddddddddd"))
	case "owned-import", "alias":
		dir := filepath.Join(root, "home", "other-claude")
		ts.Check(os.MkdirAll(dir, 0o700))
		spelling := ExportSpelling(dir)
		ts.Setenv("IMPORT_DIR", spelling)
		service = LiveService + "-" + Sha8(spelling)
		item(service, blob("import", Acct, Org))
		if kind == "alias" {
			ts.Check(os.Symlink(dir, filepath.Join(root, "home", ".claude")))
		} else {
			records = append(records, owned(Org))
		}
	case "owned", "relocate", "occupied":
		org := Org
		if kind != "owned" {
			org = UnknownOrg
		}
		records = append(records, owned(org))
		ns := filepath.Join(root, "config", "claude", Acct, org)
		write(filepath.Join(ns, ".credentials.json"), blob("owned", Acct, Org))
		write(filepath.Join(ns, ".credentials.json.pending"), map[string]any{})
		write(filepath.Join(ns, ".pending.meta"), map[string]any{})
		ts.Setenv("NAMESPACE", ns)
		ts.Setenv("TARGET", filepath.Join(root, "config", "claude", Acct, Org))
		lock := filepath.Join(root, "config", "claude", ".locks", Acct+"."+org+".lock")
		ts.Setenv("NAMESPACE_LOCK", lock)
		if kind == "occupied" {
			write(filepath.Join(root, "config", "claude", Acct, Org, ".credentials.json"), blob("occupant", Acct, Org))
		}
	case "readonly", "hidden":
		item(service, blob("readonly", Acct, Org))
		if kind == "readonly" {
			records = append(records, map[string]any{"account_uuid": Acct, "organization_uuid": Org, "kind": map[string]any{"kind": "config_dir_read_only", "dir": "", "service": service, "shares_live_dir": false}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339)})
		}
	default:
		ts.Fatalf("unknown import fixture %q", kind)
	}
	ts.Setenv("SERVICE", service)
	if records != nil {
		write(filepath.Join(root, "config", "config.json"), map[string]any{"version": 1, "accounts": records, "forgotten_services": []string{}})
	}
	ts.Check(os.WriteFile(filepath.Join(root, "dump"), []byte(dumpListing(services...)), 0o600))
	ts.Check(os.WriteFile(filepath.Join(root, "security.log"), nil, 0o600))
	originals := map[string]string{}
	for _, s := range services {
		path := filepath.Join(root, "items", KeychainAccount, ItemFileName(s))
		originals[path] = ts.ReadFile(path)
	}
	ts.SetCmd("write-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: write-check <read-only|empty|readonly|owned|alias|relocated|unchanged>")
		}
		log := ts.ReadFile(ts.Getenv("AGCTL_FAKE_SECURITY_LOG"))
		for line := range strings.SplitSeq(log, "\n") {
			verb, _, _ := strings.Cut(line, " ")
			switch verb {
			case "", "show-keychain-info", "dump-keychain", "find-generic-password":
			default:
				ts.Fatalf("unexpected keychain command: %s", line)
			}
		}
		for path, before := range originals {
			if ts.ReadFile(path) != before {
				ts.Fatalf("keychain item changed: %s", path)
			}
		}
		if args[0] == "read-only" {
			return
		}
		var registry struct {
			Accounts []struct {
				OrganizationUUID string `json:"organization_uuid"`
				Kind             struct {
					Kind           string `json:"kind"`
					Dir            string `json:"dir"`
					Service        string `json:"service"`
					SharesLiveDir  bool   `json:"shares_live_dir"`
					ExportSpelling string `json:"export_spelling"`
				} `json:"kind"`
			} `json:"accounts"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(root, "config", "config.json"))), &registry))
		switch args[0] {
		case "empty":
			if len(registry.Accounts) != 0 {
				ts.Fatalf("remaining records: %d", len(registry.Accounts))
			}
		case "readonly":
			if len(registry.Accounts) != 2 {
				ts.Fatalf("records=%d, want 2", len(registry.Accounts))
			}
			for _, record := range registry.Accounts {
				if record.Kind.Kind != "config_dir_read_only" || !strings.HasPrefix(record.Kind.Service, LiveService) {
					ts.Fatalf("not a read-only keychain record: %+v", record)
				}
			}
		case "owned":
			if len(registry.Accounts) != 1 || registry.Accounts[0].Kind.Kind != "owned" {
				ts.Fatalf("owned record was changed")
			}
		case "alias":
			if len(registry.Accounts) != 1 || !registry.Accounts[0].Kind.SharesLiveDir || registry.Accounts[0].Kind.Dir != ts.Getenv("IMPORT_DIR") {
				ts.Fatalf("alias metadata not retained")
			}
		case "relocated":
			if len(registry.Accounts) != 1 || registry.Accounts[0].OrganizationUUID != Org || registry.Accounts[0].Kind.ExportSpelling != ExportSpelling(ts.Getenv("TARGET")) {
				ts.Fatalf("relocated record has wrong organization or spelling")
			}
			info, err := os.Stat(filepath.Join(ts.Getenv("TARGET"), ".credentials.json"))
			ts.Check(err)
			if info.Mode().Perm() != 0o600 {
				ts.Fatalf("credential mode=%#o", info.Mode().Perm())
			}
		case "unchanged":
			if len(registry.Accounts) != 1 {
				ts.Fatalf("refusal removed record")
			}
		default:
			ts.Fatalf("unknown write check %q", args[0])
		}
	})
	ts.SetCmd("namespace-lock", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 || args[0] != "hold" {
			ts.Fatalf("usage: namespace-lock hold")
		}
		path := ts.Getenv("NAMESPACE_LOCK")
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		ts.Check(err)
		ts.Check(unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB))
		ts.Defer(func() { _ = file.Close() })
		started := time.Now()
		ts.SetCmd("lock-waited", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 0 || time.Since(started) < 4*time.Second {
				ts.Fatalf("namespace lock did not wait its bounded interval")
			}
		})
	})
	ts.SetCmd("log-reset", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: log-reset")
		}
		ts.Check(os.WriteFile(filepath.Join(root, "security.log"), nil, 0o600))
	})
	ts.SetCmd("no-service-read", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: no-service-read")
		}
		if strings.Contains(ts.ReadFile(filepath.Join(root, "security.log")), service) {
			ts.Fatalf("forgotten item was read: %s", service)
		}
	})
}
