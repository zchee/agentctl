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

package codex

import (
	"os"
	"strings"
)

func loginTestBinary() (string, bool) { return os.LookupEnv("AGENTCTL_CODEX_BIN") }
func loginTestEnv(name string) bool {
	return strings.HasPrefix(name, "AGENTCTL_FAKE_CODEX_") && len(name) > len("AGENTCTL_FAKE_CODEX_")
}

func loginFixtureEnv(entry string) string {
	name, value, ok := strings.Cut(entry, "=")
	if !ok || !loginTestEnv(name) {
		return entry
	}
	return strings.Replace(name, "AGENTCTL_FAKE_CODEX_", "AGCTL_FAKE_CODEX_", 1) + "=" + value
}
