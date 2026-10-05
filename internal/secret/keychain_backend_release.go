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

//go:build !agentctl_testing

package secret

import "runtime"

// securityBin is the production security(1) binary. An absolute path, never
// resolved through PATH: this process must not be talked into running some
// other program by an inherited environment.
const securityBin = "/usr/bin/security"

// newReader is the release factory: the real security(1) on macOS, a refusal
// everywhere else. No environment variable can redirect it.
func newReader() Reader {
	if runtime.GOOS != "darwin" {
		return unsupportedReader{}
	}
	return newSecurityCLI(securityBin, CurrentAccount(), minimalChildEnv())
}
