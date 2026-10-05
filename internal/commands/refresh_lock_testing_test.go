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

//go:build agentctl_testing

package commands

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestRefreshUnavailableFlockRefusesWithoutRequests(t *testing.T) {
	t.Setenv("AGENTCTL_FAULT", "flock_enotsup")
	world := newStatusWorld(t, serveBody(http.StatusOK, usageBody(t)))
	world.seedOwned(t, world.fixture.Blob("expired-access", "old-refresh", testutil.ExpiredAt()))
	world.status.Refresher = refreshOAuth(t, func(http.ResponseWriter, *http.Request) {
		t.Error("a refused lock must not spend a refresh grant")
	})
	paths, _ := refreshRecord(t, world.fixture)
	guard, err := secret.Acquire(t.Context(), paths.LocksDir(), "probe.lock", time.Now().Add(time.Second))
	if guard != nil || !errors.Is(err, unix.ENOTSUP) {
		t.Fatalf("lock = %v, %v; want unavailable ENOTSUP", guard, err)
	}
	stdout, err := world.run(t, cli.ClaudeStatusOptions{Refresh: true, Accounts: ownedOnly})
	assertPartial(t, err, 1)
	if !strings.Contains(stdout, "lock unavailable") || world.calls.Load() != 0 {
		t.Fatalf("lock refusal must be visible without requests: calls=%d\n%s", world.calls.Load(), stdout)
	}
	if stored := rereadCredential(paths.NamespaceDir(testutil.Acct, testutil.Org)); stored == nil || stored.AuthorizationHeader() != "Bearer expired-access" {
		t.Fatal("the refused refresh modified the credential")
	}
}
