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
	"bufio"
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/sys/unix"
)

func init() { registerScriptCmd("loginfixture", loginFixture) }

func loginFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: loginfixture <case>")
	}
	ctx := ts.Value(scriptContextKey{}).(context.Context)
	kind := args[0]
	root := ts.MkAbs(filepath.Join("login-cases", kind))
	home, store := filepath.Join(root, "home"), filepath.Join(root, "config")
	ts.Check(os.MkdirAll(home, 0o700))
	var organization atomic.Value
	organization.Store(Org)
	if kind == "unknown-org" {
		organization.Store("")
	}
	var exchanges, profiles atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			exchanges.Add(1)
			if r.Method != http.MethodPost {
				w.WriteHeader(405)
				return
			}
			var request map[string]string
			if err := json.UnmarshalRead(r.Body, &request); err != nil || request["grant_type"] != "authorization_code" || request["code"] != "minted-code" || request["code_verifier"] == "" || request["state"] == "" {
				w.WriteHeader(400)
				return
			}
			body := map[string]any{"token_type": "Bearer", "access_token": "sk-ant-oat01-minted", "refresh_token": "sk-ant-ort01-minted", "expires_in": 28800, "refresh_token_expires_in": 2377445, "scope": "user:file_upload user:inference user:mcp_servers user:profile user:sessions:claude_code", "account": map[string]string{"uuid": Acct, "email_address": "user@example.com"}}
			if org := organization.Load().(string); org != "" {
				body["organization"] = map[string]string{"uuid": org, "name": "Example Org"}
			}
			_ = json.MarshalWrite(w, body)
		case "/profile":
			profiles.Add(1)
			if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-minted" {
				w.WriteHeader(401)
				return
			}
			if kind != "plan" {
				w.WriteHeader(500)
				return
			}
			_ = json.MarshalWrite(w, map[string]any{"account": map[string]string{"uuid": Acct, "email": "profile@example.com"}, "organization": map[string]string{"uuid": Org, "organization_type": "claude_max", "rate_limit_tier": "default_claude_max_20x"}})
		default:
			w.WriteHeader(404)
		}
	}))
	ts.Defer(server.Close)
	for key, value := range map[string]string{"HOME": home, "AGENTCTL_CONFIG_DIR": store, "AGENTCTL_CLAUDE_TOKEN_URL": server.URL + "/token", "AGENTCTL_CLAUDE_AUTHORIZE_URL": server.URL + "/authorize", "AGENTCTL_CLAUDE_PROFILE_URL": server.URL + "/profile", "AGENTCTL_NO_BROWSER": "1", "AGENTCTL_FAULT": "", "AGCTL_FAKE_SECURITY_LOG": filepath.Join(root, "security.log"), "LOGIN_STORE": store, "LOGIN_HOME": home, "LOGIN_ACCT": Acct, "LOGIN_ORG": Org, "AGENTCTL_LOG": "warn"} {
		ts.Setenv(key, value)
	}
	ts.Check(os.WriteFile(filepath.Join(root, "security.log"), nil, 0o600))
	var liveBefore []byte
	if kind == "live" || kind == "other-live" || kind == "no-duplicate" || kind == "other-no-duplicate" || kind == "plan" {
		acct := Acct
		if strings.HasPrefix(kind, "other-") {
			acct = "99999999-9999-4999-8999-999999999999"
		}
		liveBefore, _ = json.Marshal(map[string]any{"oauthAccount": map[string]string{"accountUuid": acct, "organizationUuid": Org, "emailAddress": "user@example.com", "organizationName": "Example Org"}})
		livePath := filepath.Join(home, ".claude.json")
		if kind == "plan" {
			realStore := filepath.Join(home, ".claude-real")
			ts.Check(os.MkdirAll(realStore, 0o700))
			ts.Check(os.Symlink(realStore, filepath.Join(home, ".claude")))
			target := filepath.Join(realStore, ".claude.json")
			ts.Check(os.Symlink(target, livePath))
			livePath = target
		}
		ts.Check(os.WriteFile(livePath, liveBefore, 0o600))
	}
	var prior []byte
	var priorInfo os.FileInfo
	ts.SetCmd("login-org", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: login-org <organization>")
		}
		organization.Store(args[0])
	})
	ts.SetCmd("login-run", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) < 2 {
			ts.Fatalf("usage: login-run <manual|loopback|signal> <exit> [flags]")
		}
		exit, err := strconv.Atoi(args[1])
		ts.Check(err)
		path := filepath.Join(store, "claude", Acct, Org, ".credentials.json")
		prior, _ = os.ReadFile(path)
		priorInfo, _ = os.Stat(path)
		loginProcess(ctx, ts, args[0], exit, args[2:], kind == "wrong-state")
	})
	ts.SetCmd("login-check", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: login-check <stored|untouched|unchanged|signal|siblings|plan|no-plan> <calls>")
		}
		calls, err := strconv.Atoi(args[1])
		ts.Check(err)
		if int(exchanges.Load()) != calls || int(profiles.Load()) != calls {
			ts.Fatalf("request counts exchange=%d profile=%d; want %d each", exchanges.Load(), profiles.Load(), calls)
		}
		if log := ts.ReadFile(filepath.Join(root, "security.log")); log != "" {
			ts.Fatalf("login touched the keychain: %s", log)
		}
		if liveBefore != nil && !bytes.Equal(liveBefore, []byte(ts.ReadFile(filepath.Join(home, ".claude.json")))) {
			ts.Fatalf("login changed the live identity document")
		}
		switch args[0] {
		case "untouched":
			if _, err := os.Stat(store); !os.IsNotExist(err) {
				ts.Fatalf("refusal changed the store: %v", err)
			}
			return
		case "unchanged":
			path := filepath.Join(store, "claude", Acct, Org, ".credentials.json")
			info, err := os.Stat(path)
			ts.Check(err)
			if priorInfo == nil || !os.SameFile(priorInfo, info) || !bytes.Equal(prior, []byte(ts.ReadFile(path))) {
				ts.Fatalf("refusal replaced the previous credential")
			}
			return
		case "signal":
			nsDir := filepath.Join(store, "claude", Acct, Org)
			entries, err := os.ReadDir(nsDir)
			ts.Check(err)
			if len(entries) != 0 {
				ts.Fatalf("signal left namespace files behind: %v", entries)
			}
			lock, err := os.OpenFile(filepath.Join(store, "claude", ".locks", Acct+"."+Org+".lock"), os.O_RDWR, 0)
			ts.Check(err)
			defer func() { _ = lock.Close() }()
			ts.Check(unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
			return
		}
		var registry struct {
			Accounts []struct {
				Account      string `json:"account_uuid"`
				Organization string `json:"organization_uuid"`
				Kind         struct {
					Kind     string `json:"kind"`
					Spelling string `json:"export_spelling"`
					SHA8     string `json:"export_sha8"`
				} `json:"kind"`
			} `json:"accounts"`
		}
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(filepath.Join(store, "config.json"))), &registry))
		wantRecords := 1
		if args[0] == "siblings" {
			wantRecords = 2
		}
		if len(registry.Accounts) != wantRecords {
			ts.Fatalf("registry has %d accounts, want %d", len(registry.Accounts), wantRecords)
		}
		for _, record := range registry.Accounts {
			nsDir := filepath.Join(store, "claude", record.Account, record.Organization)
			spelling := ExportSpelling(nsDir)
			if record.Account != Acct || record.Kind.Kind != "owned" || record.Kind.Spelling != spelling || record.Kind.SHA8 != Sha8(spelling) {
				ts.Fatalf("owned registry identity or namespace spelling differs")
			}
			var stored struct {
				OAuth struct {
					Access       string   `json:"accessToken"`
					Refresh      string   `json:"refreshToken"`
					Expires      int64    `json:"expiresAt"`
					Scopes       []string `json:"scopes"`
					Subscription *string  `json:"subscriptionType"`
					Tier         *string  `json:"rateLimitTier"`
					Account      struct {
						Email string `json:"emailAddress"`
					} `json:"tokenAccount"`
				} `json:"claudeAiOauth"`
			}
			path := filepath.Join(nsDir, ".credentials.json")
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(path)), &stored))
			if stored.OAuth.Access != "sk-ant-oat01-minted" || stored.OAuth.Refresh != "sk-ant-ort01-minted" {
				ts.Fatalf("stored credential differs from the minted pair")
			}
			if diff := gocmp.Diff(strings.Fields("user:file_upload user:inference user:mcp_servers user:profile user:sessions:claude_code"), stored.OAuth.Scopes); diff != "" {
				ts.Fatalf("scopes: %s", diff)
			}
			if delta := stored.OAuth.Expires - time.Now().UnixMilli(); delta < 28740000 || delta > 28860000 {
				ts.Fatalf("expiry calculation differs: %d", delta)
			}
			if args[0] == "plan" && (stored.OAuth.Subscription == nil || *stored.OAuth.Subscription != "max" || stored.OAuth.Tier == nil || *stored.OAuth.Tier != "default_claude_max_20x" || stored.OAuth.Account.Email != "user@example.com") {
				ts.Fatalf("profile plan or exchange identity was not preserved")
			}
			if args[0] == "no-plan" && (stored.OAuth.Subscription != nil || stored.OAuth.Tier != nil) {
				ts.Fatalf("unavailable profile supplied a plan")
			}
			for path, mode := range map[string]os.FileMode{nsDir: 0o700, path: 0o600, filepath.Join(store, "claude", ".locks", record.Account+"."+record.Organization+".lock"): 0o600} {
				info, err := os.Stat(path)
				ts.Check(err)
				if info.Mode().Perm() != mode {
					ts.Fatalf("mode of %s: %o, want %o", path, info.Mode().Perm(), mode)
				}
			}
		}
	})
}

func loginProcess(ctx context.Context, ts *testscript.TestScript, mode string, wantExit int, flags []string, wrongState bool) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	binary, err := os.Executable()
	ts.Check(err)
	args := []string{"claude", "login"}
	if mode != "loopback" {
		args = append(args, "--manual")
	}
	args = append(args, flags...)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Args[0] = "agentctl"
	cmd.Dir = ts.MkAbs(".")
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "AGENTCTL_CONFIG_DIR", "AGENTCTL_LOG", "AGENTCTL_NO_BROWSER", "AGENTCTL_CLAUDE_TOKEN_URL", "AGENTCTL_CLAUDE_AUTHORIZE_URL", "AGENTCTL_CLAUDE_PROFILE_URL", "AGENTCTL_SECURITY_BIN", "AGCTL_FAKE_SECURITY_LOG", "AGCTL_FAKE_SECURITY_ITEMS", "AGCTL_FAKE_SECURITY_DUMP"} {
		cmd.Env = append(cmd.Env, key+"="+ts.Getenv(key))
	}
	if mode == "signal" {
		cmd.Env = append(cmd.Env, "AGENTCTL_FAULT=pause_before_rename")
	}
	input, err := cmd.StdinPipe()
	ts.Check(err)
	defer func() { _ = input.Close() }()
	output, err := cmd.StdoutPipe()
	ts.Check(err)
	var stdout, stderr bytes.Buffer
	cmd.Stderr = &stderr
	ts.Check(cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	urls := make(chan string, 1)
	drained := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			line := scanner.Text()
			stdout.WriteString(line)
			stdout.WriteByte('\n')
			if strings.HasPrefix(line, "  http") {
				urls <- strings.TrimSpace(line)
			}
		}
		drained <- scanner.Err()
	}()
	var authorize string
	select {
	case authorize = <-urls:
	case <-ctx.Done():
		ts.Fatalf("login did not print an authorize URL")
	}
	parsed, err := url.Parse(authorize)
	ts.Check(err)
	if parsed.Query().Get("code_challenge") == "" || parsed.Query().Get("code_challenge_method") != "S256" {
		ts.Fatalf("authorize URL omitted PKCE")
	}
	state := parsed.Query().Get("state")
	if wrongState {
		state = "not-the-state-you-minted"
	}
	if mode == "loopback" {
		callback := parsed.Query().Get("redirect_uri") + "?code=minted-code&state=" + url.QueryEscape(state)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
		ts.Check(err)
		response, err := http.DefaultClient.Do(request)
		ts.Check(err)
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		ts.Check(err)
		if response.StatusCode != 200 || string(body) != "<!doctype html><title>agentctl</title><p>Login complete. You can close this tab." {
			ts.Fatalf("callback page differs")
		}
	} else {
		_, err = fmt.Fprintf(input, "minted-code#%s\n", state)
		ts.Check(err)
	}
	if mode == "signal" {
		nsDir := filepath.Join(ts.Getenv("AGENTCTL_CONFIG_DIR"), "claude", Acct, Org)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		staged := false
		for !staged {
			entries, _ := os.ReadDir(nsDir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".credentials.json.tmp.") {
					staged = true
					break
				}
			}
			if staged {
				break
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				ts.Fatalf("login did not stage a temporary credential")
			}
		}
		lock, err := os.OpenFile(filepath.Join(ts.Getenv("AGENTCTL_CONFIG_DIR"), "claude", ".locks", Acct+"."+Org+".lock"), os.O_RDWR, 0)
		ts.Check(err)
		lockErr := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		_ = lock.Close()
		if !errors.Is(lockErr, unix.EWOULDBLOCK) {
			ts.Fatalf("staged write did not hold namespace lock: %v", lockErr)
		}
		ts.Check(cmd.Process.Signal(unix.SIGTERM))
	}
	ts.Check(<-drained)
	err = cmd.Wait()
	gotExit := 0
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			gotExit = exit.ExitCode()
		} else {
			ts.Check(err)
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), "sk-ant") {
		ts.Fatalf("login output contains token material")
	}
	if gotExit != wantExit {
		ts.Fatalf("login exited %d, want %d; stderr: %s", gotExit, wantExit, stderr.String())
	}
	_, _ = io.WriteString(ts.Stdout(), stdout.String())
	_, _ = io.WriteString(ts.Stderr(), stderr.String())
}
