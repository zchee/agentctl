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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// pipeLoginTerminal leaves browser navigation to the real pipe-driving test.
type pipeLoginTerminal struct{ LoginTerminal }

func (p pipeLoginTerminal) OpenBrowser(context.Context, string) {}

func TestLoginPersistsOnlyAfterAuthorization(t *testing.T) {
	tests := map[string]struct {
		manual, wrongState, noDuplicate, sameLive, missingIdentity, noOrganization, profileDown bool
		wantExit, wantExchanges                                                                 int
	}{
		"success: manual":                                {manual: true, wantExchanges: 1},
		"success: loopback":                              {wantExchanges: 1},
		"success: live notice":                           {manual: true, sameLive: true, wantExchanges: 1},
		"success: no duplicate allows different account": {manual: true, noDuplicate: true, wantExchanges: 1},
		"success: profile supplies identity":             {manual: true, missingIdentity: true, wantExchanges: 1},
		"success: profile cannot add organization":       {manual: true, noOrganization: true, wantExchanges: 1},
		"success: profile unavailable":                   {manual: true, profileDown: true, wantExchanges: 1},
		"error: wrong state":                             {manual: true, wrongState: true, wantExit: 1},
		"error: no duplicate":                            {manual: true, noDuplicate: true, sameLive: true, wantExit: 1, wantExchanges: 1},
		"error: no identity anywhere":                    {manual: true, missingIdentity: true, profileDown: true, wantExit: 1, wantExchanges: 1},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var exchanges, profiles atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					exchanges.Add(1)
					account, organization := `,"account":{"uuid":"acct","email_address":"minted@example.com"}`, `,"organization":{"uuid":"org","name":"Example"}`
					if test.missingIdentity {
						account = ""
					}
					if test.noOrganization {
						organization = ""
					}
					_, _ = fmt.Fprintf(w, `{"access_token":"sk-ant-oat01-minted","refresh_token":"sk-ant-ort01-minted","expires_in":28800%s%s}`, account, organization)
					return
				}
				profiles.Add(1)
				if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-minted" {
					t.Error("profile did not use exchanged bearer")
				}
				if test.profileDown {
					w.WriteHeader(500)
					return
				}
				_, _ = io.WriteString(w, `{"account":{"uuid":"acct","email":"profile@example.com"},"organization":{"uuid":"org","name":"Profile Org","organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`)
			}))
			defer server.Close()
			client, err := claude.NewLoginClient(server.URL+"/authorize", server.URL+"/token", server.URL+"/profile", "test")
			if err != nil {
				t.Fatal(err)
			}
			paths := config.NewPaths(filepath.Join(t.TempDir(), "store"))
			login := Login{Paths: paths, Client: client, LiveIdentitySource: "/private-home/.claude.json"}
			if test.sameLive {
				login.LiveIdentity = &claude.Identity{AccountUUID: "acct", OrganizationUUID: new("org")}
			}
			out, stderr, runErr := driveLogin(t, &login, cli.ClaudeLoginOptions{Manual: test.manual, NoDuplicate: test.noDuplicate, Label: "Primary"}, test.wrongState)
			if got := errs.ExitCode(runErr); got != test.wantExit {
				t.Fatalf("exit %d, want %d: %v", got, test.wantExit, runErr)
			}
			if int(exchanges.Load()) != test.wantExchanges || profiles.Load() != exchanges.Load() {
				t.Fatalf("requests exchange=%d profile=%d", exchanges.Load(), profiles.Load())
			}
			if strings.Contains(out+stderr, "sk-ant") {
				t.Fatal("login output leaked a token")
			}
			if test.wantExit != 0 {
				if _, err := os.Stat(paths.ConfigDir()); !os.IsNotExist(err) {
					t.Fatalf("refused login changed store: %v", err)
				}
				return
			}
			organization := "org"
			if test.noOrganization {
				organization = config.UnknownOrg
			}
			nsDir := paths.NamespaceDir("acct", organization)
			stored, err := secret.ReadCredentials(nsDir)
			if err != nil || !stored.Present {
				t.Fatalf("stored credential: %v", err)
			}
			credentials, err := claude.ParseBlob(stored.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			if (credentials.SubscriptionType != nil) == test.profileDown {
				t.Fatalf("profile plan = %v", credentials.SubscriptionType)
			}
			if got := credentials.ExpiresAtMillis - time.Now().UnixMilli(); got < 28700000 || got > 28800000 {
				t.Fatalf("expiry delta = %d", got)
			}
			registry, err := config.LoadRegistry(t.Context(), paths)
			if err != nil {
				t.Fatal(err)
			}
			record := registry.Get("acct", organization)
			if record == nil || record.Kind.Owned == nil || record.Label == nil || *record.Label != "Primary" {
				t.Fatalf("wrong registry: %+v", registry)
			}
			spelling := claude.ExportSpelling(nsDir)
			if diff := gocmp.Diff(config.OwnedKind{ExportSpelling: spelling, ExportSHA8: claude.SHA8(spelling)}, *record.Kind.Owned); diff != "" {
				t.Fatal(diff)
			}
			for path, mode := range map[string]os.FileMode{nsDir: 0o700, filepath.Join(nsDir, secret.CredentialsFile): 0o600, paths.LockPath("acct", organization): 0o600} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Fatalf("%s mode = %o", path, info.Mode().Perm())
				}
			}
			if test.sameLive && !strings.Contains(stderr, "both stay valid") {
				t.Fatalf("missing notice: %s", stderr)
			}
			if !strings.Contains(out, "Logged in as") {
				t.Fatalf("missing success: %s", out)
			}
		})
	}
}

func driveLogin(t *testing.T, login *Login, opts cli.ClaudeLoginOptions, wrongState bool) (string, string, error) {
	t.Helper()
	in, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = input.Close() }()
	output, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close(); _ = out.Close() }()
	var stdout, stderr bytes.Buffer
	login.IO = pipeLoginTerminal{LoginTerminal{In: in, Out: out, Err: &stderr}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			line := scanner.Text()
			stdout.WriteString(line)
			stdout.WriteByte('\n')
			if !strings.HasPrefix(line, "  http") {
				continue
			}
			parsed, err := url.Parse(strings.TrimSpace(line))
			if err != nil {
				done <- err
				return
			}
			state := parsed.Query().Get("state")
			if wrongState {
				state = "wrong"
			}
			if opts.Manual {
				_, err = fmt.Fprintf(input, "minted-code#%s\n", state)
			} else {
				callback := parsed.Query().Get("redirect_uri") + "?code=minted-code&state=" + url.QueryEscape(state)
				request, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
				if reqErr != nil {
					done <- reqErr
					return
				}
				response, requestErr := http.DefaultClient.Do(request)
				err = requestErr
				if response != nil {
					_ = response.Body.Close()
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- scanner.Err()
	}()
	err = login.Run(ctx, opts)
	_ = out.Close()
	if readErr := <-done; readErr != nil {
		t.Fatal(readErr)
	}
	return stdout.String(), stderr.String(), err
}

func TestLoginRefusesNonterminalOverwrite(t *testing.T) {
	in, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = input.Close() }()
	var out bytes.Buffer
	terminal := LoginTerminal{In: in, Out: &out}
	confirmed, err := terminal.Confirm(t.Context(), "replace?")
	if confirmed || errs.ExitCode(err) != 1 || !strings.Contains(err.Error(), "not a terminal") || strings.Contains(err.Error(), "--yes") {
		t.Fatalf("confirm = %v, %v", confirmed, err)
	}
}

func TestLoginClearStaleFiles(t *testing.T) {
	paths := config.NewPaths(filepath.Join(t.TempDir(), "store"))
	nsDir := paths.NamespaceDir("acct", "org")
	if err := ClearStaleFiles(t.Context(), paths, nsDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.ConfigDir()); !os.IsNotExist(err) {
		t.Fatal("cleanup created missing store")
	}
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{secret.PendingFile, secret.PendingMetaFile, ".credentials.json.tmp.12ABCDEF", ".credentials.json.tmp.not-hex!", secret.CredentialsFile} {
		if err := os.WriteFile(filepath.Join(nsDir, name), []byte("keep private"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ClearStaleFiles(t.Context(), paths, nsDir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if diff := gocmp.Diff([]string{secret.CredentialsFile, ".credentials.json.tmp.not-hex!"}, names); diff != "" {
		t.Fatal(diff)
	}
	if err := ClearStaleFiles(t.Context(), paths, t.TempDir()); err == nil {
		t.Fatal("outside store was allowed")
	}
}
