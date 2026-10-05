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
	"os"
	"path/filepath"
)

// WithCodex installs the fake codex executable and wires it into every
// spawn, so a login child runs the stand-in instead of a real binary.
func (f *Fixture) WithCodex() *Fixture {
	f.tb.Helper()
	f.installScript("fake-codex.sh", f.CodexBin())
	f.Set("AGENTCTL_CODEX_BIN", f.CodexBin())
	return f
}

// CodexBin returns where the fake codex is installed.
func (f *Fixture) CodexBin() string {
	return filepath.Join(f.BinDir(), "fake-codex.sh")
}

// CodexLogPath returns where the fake codex records its argv, environment
// and working directory once a test wires the script's log knob to it.
func (f *Fixture) CodexLogPath() string {
	return filepath.Join(f.root, "fake-codex.log")
}

// CodexHome returns a named home directory inside the fixture, created on
// demand. The only such home a test may name: a test that wants the
// fallback path puts its file under Home instead, which is inside the
// fixture too.
func (f *Fixture) CodexHome(name string) string {
	f.tb.Helper()
	dir := filepath.Join(f.root, "codex-homes", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.tb.Fatalf("create fixture codex home: %v", err)
	}
	return dir
}
