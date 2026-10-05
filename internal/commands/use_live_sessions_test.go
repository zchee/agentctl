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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
)

func TestUseSessionHintsBoundedAndNamesStayHumanOnly(t *testing.T) {
	tests := map[string]struct {
		body       string
		name       string
		symlink    bool
		missing    bool
		unreadable bool
		count      int
		wantName   string
	}{
		"success: missing registry is silent":                {missing: true},
		"error: non-directory registry is unreadable":        {unreadable: true},
		"success: active bridged process has sanitized name": {name: "a.json", body: fmt.Sprintf(`{"pid":%d,"bridgeSessionId":"private-id","name":"  hello`+"`"+`\nworld  "}`, os.Getpid()), count: 1, wantName: "hello'world"},
		"success: absent pid uses numeric filename":          {name: fmt.Sprintf("%d.json", os.Getpid()), body: `{"bridgeSessionId":"private-id"}`, count: 1},
		"success: zero pid is not a session":                 {name: "a.json", body: `{"pid":0,"bridgeSessionId":"private-id"}`},
		"success: no bridge is not a remote session":         {name: "a.json", body: fmt.Sprintf(`{"pid":%d,"bridgeSessionId":""}`, os.Getpid())},
		"success: invalid JSON is skipped":                   {name: "a.json", body: `invalid`},
		"success: oversized entry is skipped":                {name: "a.json", body: strings.Repeat("x", 64*1024)},
		"success: symlink entries are skipped":               {name: "a.json", body: fmt.Sprintf(`{"pid":%d,"bridgeSessionId":"private-id"}`, os.Getpid()), symlink: true},
		"success: long names are bounded by runes":           {name: "a.json", body: fmt.Sprintf(`{"pid":%d,"bridgeSessionId":"private-id","name":%q}`, os.Getpid(), strings.Repeat("a", 50)), count: 1, wantName: strings.Repeat("a", 47) + "…"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "registry")
			if test.unreadable {
				if err := os.WriteFile(dir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if !test.missing {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, test.name)
				if test.symlink {
					target := filepath.Join(t.TempDir(), "target")
					if err := os.WriteFile(target, []byte(test.body), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			hints := useScanSessions(t.Context(), dir)
			if len(hints.names) != test.count || hints.unreadable != test.unreadable {
				t.Fatalf("hints=%+v want count=%d unreadable=%v", hints, test.count, test.unreadable)
			}
			if test.count == 1 {
				if diff := gocmp.Diff(test.wantName, hints.names[0]); diff != "" {
					t.Fatal(diff)
				}
				if !strings.Contains(hints.consent(), "120-second") || hints.completion(false) == "" {
					t.Fatal("missing advisory")
				}
				for _, text := range []string{hints.consent(), hints.completion(true), hints.completion(false)} {
					if strings.Contains(text, "private-id") || strings.Contains(text, fmt.Sprint(os.Getpid())) {
						t.Fatalf("registry identity exposed: %s", text)
					}
				}
				if test.wantName != "" && strings.Contains(hints.completion(false), test.wantName) {
					t.Fatal("JSON warning contains human name")
				}
			} else if hints.consent() != "" || hints.completion(false) != "" {
				t.Fatal("empty scan produced an advisory")
			}
		})
	}
}

func TestUseLiveMissingIDDoesNotCreateConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	err := (SessionProcess{}).RunLive(t.Context(), cli.Globals{ConfigDir: path}, cli.ClaudeUseOptions{Live: true})
	if errs.ExitCode(err) != 1 {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("usage error created configuration: %v", err)
	}
}

func TestUseLiveScopeGateDoesNotReadCredentials(t *testing.T) {
	paths := config.NewPaths(t.TempDir())
	env := claude.EnvWithHome(t.TempDir())
	report := (useLiveSwap{paths: paths, env: &env, live: true}).swapIn(t.Context(), useIncoming{}, nil, cli.ClaudeUseOptions{})
	if report.outcome.Refusal.Kind != 0 && report.outcome.ExitCode() == 0 {
		t.Fatal("invalid refusal status")
	}
	if report.outcome.Refusal.Reason() != "live_unreachable" {
		t.Fatalf("report=%+v", report)
	}
}
