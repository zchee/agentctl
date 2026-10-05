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

package secret

import (
	"os"
	"strings"
)

// The seam variables a tagged build selects its keychain backend through.
// Neither name exists in a release build.
const (
	// keychainBackendEnv switched to "none" disables the keychain outright.
	keychainBackendEnv = "AGENTCTL_KEYCHAIN_BACKEND"
	// securityBinEnv points at the stand-in security(1) script.
	securityBinEnv = "AGENTCTL_SECURITY_BIN"
)

// fakeKnobPrefix marks the stand-in script's own environment variables,
// which a tagged build passes through to the child it spawns.
const fakeKnobPrefix = "AGCTL_FAKE_"

// newReader is the tagged factory. AGENTCTL_KEYCHAIN_BACKEND=none selects
// [DisabledReader]; AGENTCTL_SECURITY_BIN points the transport at the
// stand-in. A tagged build with neither set also gets [DisabledReader], and
// that is the point: the real security(1) is not a fallback here. A test
// that has not wired a stand-in has not decided to talk to the developer's
// own keychain, and a build that could reach it by omission would eventually
// reach it by accident.
func newReader() Reader {
	if os.Getenv(keychainBackendEnv) == "none" {
		return DisabledReader{}
	}
	bin := os.Getenv(securityBinEnv)
	if bin == "" {
		return DisabledReader{}
	}
	return newSecurityCLI(bin, currentAccount(), testingChildEnv())
}

// testingChildEnv is the release child environment plus every stand-in knob
// from the process environment, so the fake script can find its item files,
// its argv log and its scripted failures. The passthrough exists only under
// the tag; a release child never sees these names.
func testingChildEnv() []string {
	env := minimalChildEnv()
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, fakeKnobPrefix) {
			env = append(env, entry)
		}
	}
	return env
}
