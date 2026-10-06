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
	"os/exec"
	"testing"
)

type codexFixture struct{ inner *Fixture }

func newCodexFixture(tb testing.TB) *codexFixture { return &codexFixture{inner: New(tb).WithCodex()} }

func (f *codexFixture) set(key, value string) {
	if key == "CODEX_HOME" {
		panic("a test must not set CODEX_HOME in a spawned environment; create a home below the fixture instead")
	}
	f.inner.Set(key, value)
}

func (f *codexFixture) apply(cmd *exec.Cmd) *exec.Cmd {
	if _, present := f.inner.Lookup("CODEX_HOME"); present {
		panic("a test must not set CODEX_HOME in a spawned environment")
	}
	if home, _ := f.inner.Lookup("HOME"); home != f.inner.Home() {
		panic("the Codex fixture's HOME must stay inside its sandbox")
	}
	cmd.Env = append(f.inner.Environ(), "AGENTCTL_CONFIG_DIR="+f.inner.ConfigDir())
	return cmd
}
