//go:build agentctl_testing

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
	"net/http"
	"strings"
	"testing"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestWatchRechecksTheKeychainEveryPass(t *testing.T) {
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.fixture.WithKeychain().KeychainItem("Claude Code-credentials", world.fixture.Blob("access-fresh", "refresh-fresh", testutil.FreshAt())).Dump("Claude Code-credentials")
	for _, pair := range world.fixture.Environ() {
		key, value, _ := strings.Cut(pair, "=")
		t.Setenv(key, value)
	}
	t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "")
	watch := Watch{NewStatus: func() *Status { copy := *world.status; copy.Reader = secret.NewReader(); return &copy }}
	paths := config.NewPaths(world.fixture.ConfigDir())
	first, err := watch.collect(t.Context(), paths, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || first[0].state.Kind == claude.StateKeychainLocked {
		t.Fatalf("first pass: %v", first)
	}
	t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT", "36")
	t.Setenv("AGCTL_FAKE_SECURITY_PREFLIGHT_STDERR", "The keychain is locked.")
	second, err := watch.collect(t.Context(), paths, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) == 0 || second[0].state.Kind != claude.StateKeychainLocked {
		t.Fatalf("second pass did not notice keychain locking: %v", second)
	}
	preflights := 0
	for _, line := range world.fixture.SecurityLog() {
		if strings.Contains(line, "show-keychain-info") {
			preflights++
		}
	}
	if preflights != 2 {
		t.Fatalf("preflights=%d want 2", preflights)
	}
	world.fixture.AssertKeychainReadOnly()
}
