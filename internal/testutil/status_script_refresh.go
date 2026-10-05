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
	"crypto/sha256"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/fixtures"
)

func init() { registerScriptCmd("refreshfixture", refreshFixture) }

func refreshFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: refreshfixture <scenario>")
	}
	kind := args[0]
	root, err := os.MkdirTemp(ts.MkAbs("."), "refresh-")
	ts.Check(err)
	ns := filepath.Join(root, "config", "claude", Acct, Org)
	credential := filepath.Join(ns, ".credentials.json")
	lock := filepath.Join(root, "config", "claude", ".locks", Acct+"."+Org+".lock")
	service := LiveService + "-" + Sha8(ExportSpelling(ns))
	item := filepath.Join(root, "items", KeychainAccount, ItemFileName(service))
	for _, dir := range []string{ns, filepath.Dir(lock), filepath.Dir(item), filepath.Join(root, "home")} {
		ts.Check(os.MkdirAll(dir, 0o700))
	}
	if kind == "migrated-canonical" {
		ts.Check(os.Rename(filepath.Join(root, "config"), filepath.Join(root, "real-config")))
		ts.Check(os.Symlink(filepath.Join(root, "real-config"), filepath.Join(root, "config")))
		canonical, err := filepath.EvalSymlinks(ns)
		ts.Check(err)
		service = MigrationService(canonical)
		item = filepath.Join(root, "items", KeychainAccount, ItemFileName(service))
	}
	for key, value := range map[string]string{
		"AGENTCTL_CONFIG_DIR": filepath.Join(root, "config"), "HOME": filepath.Join(root, "home"), "USER": KeychainAccount, "LOGNAME": KeychainAccount,
		"AGCTL_FAKE_SECURITY_ITEMS": filepath.Join(root, "items"), "AGCTL_FAKE_SECURITY_DUMP": filepath.Join(root, "dump"), "AGCTL_FAKE_SECURITY_LOG": filepath.Join(root, "security.log"),
		"AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT": "0", "AGCTL_FAKE_SECURITY_FIND_EXIT": "", "AGCTL_FAKE_SECURITY_SLEEP": "", "AGCTL_FAKE_SECURITY_WRITE_EXIT": "",
		"AGENTCTL_FAULT": "", "AGENTCTL_LOG": "warn", "AGENTCTL_FAULT_RESUME": filepath.Join(root, "resume"), "CLAUDE_SECURESTORAGE_CONFIG_DIR": "", "CLAUDE_CONFIG_DIR": "", "REFRESH_NS": ns, "REFRESH_LOCK": lock, "REFRESH_ITEM": item, "REFRESH_ROOT": root,
	} {
		ts.Setenv(key, value)
	}
	write := func(path string, value any) {
		data, err := json.Marshal(value)
		ts.Check(err)
		ts.Check(os.MkdirAll(filepath.Dir(path), 0o700))
		ts.Check(os.WriteFile(path, data, 0o600))
	}
	blob := func(name string, expiry int64) map[string]any {
		return map[string]any{"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat01-" + name, "refreshToken": "sk-ant-ort01-" + name, "expiresAt": expiry, "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max", "rateLimitTier": "default_claude_max_5x", "tokenAccount": map[string]any{"uuid": Acct, "organizationUuid": Org, "emailAddress": Email, "organizationName": "Acme"}}}
	}
	value := blob("old", ExpiredAt())
	if strings.Contains(kind, "plan") || kind == "short-budget" || kind == "profile-failure" || kind == "tier-refused" {
		inner := value["claudeAiOauth"].(map[string]any)
		delete(inner, "subscriptionType")
		delete(inner, "rateLimitTier")
	}
	migrated := strings.HasPrefix(kind, "migrated") || strings.HasPrefix(kind, "peer") || kind == "large-line" || kind == "invalid-grant-peer"
	if kind == "migrated-fresh" || kind == "migrated-with-file" {
		value = blob("old", FreshAt())
	}
	write(filepath.Join(root, "config", "config.json"), map[string]any{"version": 1, "accounts": []any{map[string]any{"account_uuid": Acct, "organization_uuid": Org, "email": Email, "org_name": "Acme", "kind": map[string]any{"kind": "owned", "export_spelling": ExportSpelling(ns), "export_sha8": Sha8(ExportSpelling(ns))}, "forgotten": false, "created_at": time.Now().UTC().Format(time.RFC3339)}}, "forgotten_services": []string{}})
	target := credential
	services := []string{}
	if migrated {
		target = item
		services = append(services, service)
		ts.Check(os.WriteFile(filepath.Join(root, "items", ".allowed-services"), []byte(service+"\n"), 0o600))
	}
	write(target, value)
	if kind == "migrated-with-file" {
		write(credential, blob("file", ExpiredAt()))
	}
	configFile := filepath.Join(root, "home", ".claude.json")
	write(configFile, map[string]any{"unrelated": "preserve"})
	configBefore := ts.ReadFile(configFile)
	before, err := os.Stat(credential)
	if err != nil && !os.IsNotExist(err) {
		ts.Check(err)
	}
	var fileBefore string
	if before != nil {
		fileBefore = ts.ReadFile(credential)
	}
	started := time.Now()
	var terminated *exec.Cmd
	switch kind {
	case "hanging-owned":
		decoy := blob("plaintext", FreshAt())
		decoy["claudeAiOauth"].(map[string]any)["tokenAccount"] = map[string]any{"uuid": "99999999-9999-9999-9999-999999999999", "emailAddress": "decoy@example.com"}
		write(filepath.Join(root, "home", ".claude", ".credentials.json"), decoy)
		ts.Setenv("AGCTL_FAKE_SECURITY_SLEEP", "30")
		services = append(services, LiveService)
	case "locked-owned":
		ts.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT", "36")
		services = append(services, LiveService)
	case "lock-symlink":
		ts.Check(os.Symlink(filepath.Join(root, "elsewhere"), lock))
	case "flock-unavailable":
		ts.Setenv("AGENTCTL_FAULT", "flock_enotsup")
	case "session-after-post":
		ts.Setenv("AGENTCTL_FAULT", "pause_before_refresh_recheck")
	case "session":
		ts.Check(os.Mkdir(filepath.Join(ns, ".oauth_refresh.lock"), 0o700))
	case "legacy", "legacy-session":
		ts.Check(os.WriteFile(filepath.Join(ns, ".storage-write"), []byte("unchanged\n"), 0o600))
		if kind == "legacy-session" {
			ts.Check(os.Mkdir(filepath.Join(ns, ".storage-write.lock"), 0o700))
		}
	case "migrated-contended":
		ts.Setenv("AGENTCTL_FAULT", "lock_contended")
	case "peer-before-post":
		ts.Setenv("AGENTCTL_FAULT", "pause_before_migrated_reread")
	case "peer-after-post":
		ts.Setenv("AGENTCTL_FAULT", "pause_before_migrated_write,swap_lock_leak")
	case "migrated-plan":
		ts.Setenv("AGENTCTL_FAULT", "pause_before_migrated_write")
	case "plan-locked":
		ts.Setenv("AGENTCTL_FAULT", "pause_before_rename")
	case "tier-refused":
		ts.Setenv("AGENTCTL_LOG", "debug")
	}
	ts.Check(os.WriteFile(filepath.Join(root, "dump"), []byte(dumpListing(services...)), 0o600))
	if kind == "pending-first" || kind == "pending-removed" {
		pending := blob("pending", FreshAt())
		var access, refresh any = fmt.Sprintf("%x", sha256.Sum256([]byte("sk-ant-oat01-old"))), fmt.Sprintf("%x", sha256.Sum256([]byte("sk-ant-ort01-old")))
		if kind == "pending-first" {
			access, refresh = nil, nil
		}
		write(filepath.Join(ns, ".credentials.json.pending"), pending)
		write(filepath.Join(ns, ".pending.meta"), map[string]any{"created_at": time.Now().UTC().Format(time.RFC3339), "derived_from_access_sha256": access, "derived_from_refresh_sha256": refresh, "new_expires_at": pending["claudeAiOauth"].(map[string]any)["expiresAt"]})
		ts.Check(os.Remove(credential))
	}
	usageBody, err := fixtures.FS.ReadFile("claude/usage-2026-09-08.json")
	ts.Check(err)
	var usageCalls, tokenCalls, profileCalls atomic.Int64
	var profileMapped atomic.Bool
	profileMapped.Store(kind != "unmapped-plan")
	posted := make(chan struct{})
	release := make(chan struct{})
	var postedOnce, releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case UsagePath:
			usageCalls.Add(1)
			_, _ = w.Write(usageBody)
		case TokenPath:
			tokenCalls.Add(1)
			postedOnce.Do(func() { close(posted) })
			if kind == "blocked" {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			if kind == "delayed" {
				select {
				case <-time.After(1500 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			if kind == "invalid-grant-peer" {
				data, err := json.Marshal(blob("peer", FreshAt()))
				if err != nil {
					http.Error(w, "cannot encode peer credential", 500)
					return
				}
				if err := os.WriteFile(item, data, 0o600); err != nil {
					http.Error(w, "cannot install peer credential", 500)
					return
				}
			}
			if kind == "invalid-grant-peer" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			access := "sk-ant-oat01-rotated"
			if kind == "large-line" {
				access += strings.Repeat("x", 3000)
			}
			expiresIn := 3600
			if kind == "pending-expired" && tokenCalls.Load() == 1 {
				expiresIn = 1
			}
			body, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": "sk-ant-ort01-rotated", "expires_in": expiresIn, "scope": "user:inference user:profile"})
			_, _ = w.Write(body)
		case ProfilePath:
			if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-rotated" {
				http.Error(w, "profile did not use rotated access", 400)
				return
			}
			profileCalls.Add(1)
			if kind == "profile-failure" {
				w.WriteHeader(500)
				return
			}
			plan := "claude_pro"
			if !profileMapped.Load() {
				plan = "future_plan"
			}
			tier := "default_claude_pro"
			if kind == "tier-refused" {
				tier = "default_sentinel_" + strings.Repeat("x", 48)
			}
			body, _ := json.Marshal(map[string]any{"account": map[string]any{"uuid": Acct, "email": Email}, "organization": map[string]any{"uuid": Org, "organization_type": plan, "rate_limit_tier": tier}})
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	ts.Defer(func() { releaseOnce.Do(func() { close(release) }); server.Close() })
	ts.Setenv("AGENTCTL_CLAUDE_USAGE_URL", server.URL)
	ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+TokenPath)
	ts.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", server.URL+ProfilePath)
	ts.SetCmd("refresh-calls", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 3 {
			ts.Fatalf("usage: refresh-calls <usage> <token> <profile>")
		}
		for i, got := range []int64{usageCalls.Load(), tokenCalls.Load(), profileCalls.Load()} {
			want, err := strconv.ParseInt(args[i], 10, 64)
			ts.Check(err)
			if got != want {
				ts.Fatalf("request class %d = %d, want %d", i, got, want)
			}
		}
	})
	ts.SetCmd("refresh-action", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: refresh-action <expire|mapped|release|wait-post|terminate|peer>")
		}
		switch args[0] {
		case "session":
			ts.Check(os.Mkdir(filepath.Join(ns, ".oauth_refresh.lock"), 0o700))
		case "pending":
			switch kind {
			case "pending-changed":
				write(credential, blob("changed", ExpiredAt()))
			case "pending-invalid":
				ts.Check(os.Remove(filepath.Join(ns, ".pending.meta")))
			case "pending-symlink":
				ts.Check(os.Remove(filepath.Join(ns, ".credentials.json.pending")))
				ts.Check(os.WriteFile(filepath.Join(root, "untouched"), []byte("untouched\n"), 0o600))
				ts.Check(os.Symlink(filepath.Join(root, "untouched"), filepath.Join(ns, ".credentials.json.pending")))
			case "pending-migrated":
				write(item, blob("peer", FreshAt()))
				ts.Check(os.WriteFile(filepath.Join(root, "dump"), []byte(dumpListing(service)), 0o600))
			default:
				ts.Fatalf("scenario %q has no pending mutation", kind)
			}
		case "expire":
			var value map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(target)), &value))
			value["claudeAiOauth"].(map[string]any)["expiresAt"] = ExpiredAt()
			write(target, value)
		case "fresh":
			var value map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(target)), &value))
			value["claudeAiOauth"].(map[string]any)["expiresAt"] = FreshAt()
			write(target, value)
		case "resume":
			ts.Check(os.WriteFile(filepath.Join(root, "resume"), nil, 0o600))
		case "wait-pause":
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(filepath.Join(root, "resume.reached")); err == nil {
					break
				}
				select {
				case <-tick.C:
				case <-deadline.C:
					ts.Fatalf("pause was not reached")
				}
			}
		case "mapped":
			profileMapped.Store(true)
		case "release":
			releaseOnce.Do(func() { close(release) })
		case "wait-post":
			select {
			case <-posted:
			case <-time.After(5 * time.Second):
				ts.Fatalf("token POST was not reached")
			}
		case "terminate":
			background := ts.BackgroundCmds()
			if len(background) == 0 {
				ts.Fatalf("no background process")
			}
			terminated = background[0]
			ts.Check(terminated.Process.Signal(syscall.SIGTERM))
		case "peer":
			write(item, blob("peer", FreshAt()))
		default:
			ts.Fatalf("unknown refresh action %q", args[0])
		}
	})
	ts.SetCmd("refresh-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) == 0 {
			ts.Fatalf("usage: refresh-check <stored|plan|no-pending|read-only|write|audit|lock-body|unlocked|row> [arguments]")
		}
		switch args[0] {
		case "plan-cell":
			if len(args) != 3 {
				ts.Fatalf("plan-cell requires a report and value")
			}
			matched := false
			for line := range strings.SplitSeq(ts.ReadFile(args[1]), "\n") {
				if !strings.Contains(line, Email) {
					continue
				}
				cells := strings.Split(line, "|")
				if len(cells) < 3 || strings.TrimSpace(cells[2]) != args[2] {
					ts.Fatalf("unexpected plan cell in %q", line)
				}
				matched = true
			}
			if !matched {
				ts.Fatalf("report contains no owned row")
			}
		case "signal-exit":
			if terminated == nil || terminated.ProcessState == nil || terminated.ProcessState.ExitCode() != 143 {
				ts.Fatalf("terminated process did not exit 143")
			}
		case "bounded":
			if time.Since(started) >= 20*time.Second {
				ts.Fatalf("keychain timeout did not bound the pass")
			}
		case "file-unchanged", "atomic":
			after, err := os.Stat(credential)
			ts.Check(err)
			if before == nil {
				ts.Fatalf("credential did not exist before the pass")
			}
			if args[0] == "atomic" {
				if os.SameFile(before, after) {
					ts.Fatalf("credential was rewritten rather than replaced")
				}
			} else if !os.SameFile(before, after) || ts.ReadFile(credential) != fileBefore {
				ts.Fatalf("plaintext credential changed")
			}
		case "config-unchanged":
			if ts.ReadFile(configFile) != configBefore {
				ts.Fatalf("unrelated configuration changed")
			}
		case "namespace":
			entries, err := os.ReadDir(ns)
			ts.Check(err)
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if strings.Join(names, " ") != strings.Join(args[1:], " ") {
				ts.Fatalf("namespace entries = %v, want %v", names, args[1:])
			}
		case "stored":
			if len(args) != 2 {
				ts.Fatalf("stored requires a token name")
			}
			var stored map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(target)), &stored))
			if stored["claudeAiOauth"].(map[string]any)["accessToken"] != "sk-ant-oat01-"+args[1] {
				ts.Fatalf("unexpected stored token")
			}
			info, err := os.Stat(target)
			ts.Check(err)
			if info.Mode().Perm() != 0o600 {
				ts.Fatalf("credential mode = %v", info.Mode())
			}
		case "plan":
			if len(args) != 2 {
				ts.Fatalf("plan requires a value")
			}
			var stored map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(target)), &stored))
			inner := stored["claudeAiOauth"].(map[string]any)
			got, _ := inner["subscriptionType"].(string)
			if got != args[1] {
				ts.Fatalf("stored plan = %q, want %q", got, args[1])
			}
			if kind == "tier-refused" && inner["rateLimitTier"] != nil {
				ts.Fatalf("invalid tier was persisted")
			}
		case "no-pending":
			for _, name := range []string{".credentials.json.pending", ".pending.meta"} {
				if _, err := os.Lstat(filepath.Join(ns, name)); !os.IsNotExist(err) {
					ts.Fatalf("pending material remains: %s (%v)", name, err)
				}
			}
		case "keychain-calls":
			if len(args) != 3 {
				ts.Fatalf("keychain-calls requires item reads and total calls")
			}
			log := strings.TrimSpace(ts.ReadFile(filepath.Join(root, "security.log")))
			reads, total := 0, 0
			for line := range strings.SplitSeq(log, "\n") {
				total++
				if strings.HasPrefix(line, "find-generic-password") && strings.HasSuffix(line, "-s "+service) {
					reads++
					if !strings.Contains(line, "-a "+KeychainAccount+" ") {
						ts.Fatalf("read and write accounts differ")
					}
				}
			}
			for i, got := range []int{reads, total} {
				want, err := strconv.Atoi(args[i+1])
				ts.Check(err)
				if got != want {
					ts.Fatalf("keychain call class %d = %d, want %d: %s", i, got, want, log)
				}
			}
		case "read-only", "write":
			log := ts.ReadFile(filepath.Join(root, "security.log"))
			writes := 0
			for line := range strings.SplitSeq(log, "\n") {
				if strings.HasPrefix(line, "add-generic-password") {
					writes++
					if !strings.Contains(line, `-a "`+KeychainAccount+`" -s "`+service+`" -X <REDACTED:`) {
						ts.Fatalf("keychain argv mismatches read account/service")
					}
				}
				if strings.Contains(line, "delete-generic-password") || strings.Contains(line, "sk-ant") {
					ts.Fatalf("unsafe keychain command log")
				}
			}
			if (args[0] == "write" && writes != 1) || (args[0] == "read-only" && writes != 0) {
				ts.Fatalf("keychain writes = %d", writes)
			}
			if migrated && kind != "migrated-with-file" {
				if _, err := os.Lstat(credential); !os.IsNotExist(err) {
					ts.Fatalf("migrated file was recreated: %v", err)
				}
			}
		case "audit":
			if len(args) != 2 {
				ts.Fatalf("audit requires outcome")
			}
			log := ts.ReadFile(filepath.Join(root, "config", "claude", "keychain-writes.jsonl"))
			var entry map[string]any
			ts.Check(json.Unmarshal([]byte(strings.TrimSpace(log)), &entry))
			if entry["outcome"] != args[1] || entry["event"] != "write" || entry["target"] != "namespace:"+strings.TrimPrefix(service, LiveService+"-") {
				ts.Fatalf("incorrect audit entry: %v", entry)
			}
			for _, field := range []string{"from_digest8", "to_digest8"} {
				digest, _ := entry[field].(string)
				if len(digest) != 8 || strings.Trim(digest, "0123456789abcdef") != "" {
					ts.Fatalf("audit field %s is not a lowercase digest prefix", field)
				}
			}
			if entry["from_digest8"] == entry["to_digest8"] {
				ts.Fatalf("audit does not record rotation")
			}
			if strings.Contains(log, "sk-ant") {
				ts.Fatalf("audit leaked credential")
			}
		case "lock-body":
			var body map[string]any
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(lock)), &body))
			background := ts.BackgroundCmds()
			if len(background) == 0 || body["pid"] != float64(background[0].Process.Pid) || body["pid_start_time"] == "" || body["acquired_at"] == "" {
				ts.Fatalf("lock does not identify its holder: %v", body)
			}
		case "held":
			file, err := os.OpenFile(lock, os.O_RDWR, 0)
			ts.Check(err)
			defer func() { ts.Check(file.Close()) }()
			if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
				ts.Fatalf("namespace lock not held: %v", err)
			}
		case "peers-absent", "peers-leaked":
			paths := []string{filepath.Join(ns, ".oauth_refresh.lock"), ns + ".lock", filepath.Join(ns, ".storage-write.lock")}
			for _, path := range paths {
				info, err := os.Lstat(path)
				if args[0] == "peers-absent" {
					if !os.IsNotExist(err) {
						ts.Fatalf("peer lock remains: %s (%v)", path, err)
					}
				} else if err != nil || !info.IsDir() {
					ts.Fatalf("peer lock missing: %s (%v)", path, err)
				}
			}
			if args[0] == "peers-leaked" {
				records, err := os.ReadDir(filepath.Join(root, "config", "claude", "held-locks"))
				ts.Check(err)
				if len(records) != 1 {
					ts.Fatalf("held records = %d", len(records))
				}
				var record struct {
					Paths    []string `json:"paths"`
					StoreDir string   `json:"store_dir"`
					Tree     string   `json:"tree"`
				}
				ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(root, "config", "claude", "held-locks", records[0].Name()))), &record))
				if strings.Join(record.Paths, "\n") != strings.Join(paths, "\n") || record.StoreDir != ns || record.Tree != "agctl" {
					ts.Fatalf("incorrect held record: %v", record)
				}
			}
		case "same-keys":
			if len(args) != 3 {
				ts.Fatalf("same-keys needs two reports")
			}
			var a, b struct {
				Rows []map[string]any `json:"rows"`
			}
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &a))
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[2])), &b))
			if len(a.Rows) != 1 || len(b.Rows) != 1 || len(a.Rows[0]) != len(b.Rows[0]) {
				ts.Fatalf("row shapes differ")
			}
			for key := range a.Rows[0] {
				if _, ok := b.Rows[0][key]; !ok {
					ts.Fatalf("missing row key %s", key)
				}
			}
		case "unlocked":
			file, err := os.OpenFile(lock, os.O_RDWR, 0)
			ts.Check(err)
			defer func() { ts.Check(file.Close()) }()
			ts.Check(unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB))
			ts.Check(unix.Flock(int(file.Fd()), unix.LOCK_UN))
		case "row":
			if len(args) != 4 {
				ts.Fatalf("usage: refresh-check row <file> <field> <value>")
			}
			var doc struct {
				Rows []map[string]any `json:"rows"`
			}
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &doc))
			var owned []map[string]any
			for _, row := range doc.Rows {
				if row["kind"] == "owned" {
					owned = append(owned, row)
				}
			}
			if len(owned) != 1 || owned[0][args[2]] != args[3] {
				ts.Fatalf("unexpected owned status row %v", owned)
			}
		default:
			ts.Fatalf("unknown refresh check %q", args[0])
		}
	})
}
