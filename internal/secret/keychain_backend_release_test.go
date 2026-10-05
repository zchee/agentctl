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

import (
	"runtime"
	"strings"
	"testing"
)

func TestReleaseFactoryIgnoresTheTestingSeam(t *testing.T) {
	// The seam names must be dead in a release build: pointing them at a
	// stand-in changes nothing, and the transport stays the absolute
	// production path.
	t.Setenv("AGENTCTL_KEYCHAIN_BACKEND", "none")
	t.Setenv("AGENTCTL_SECURITY_BIN", "/stand-in/security")

	reader := NewReader()
	if runtime.GOOS != "darwin" {
		if _, ok := reader.(unsupportedReader); !ok {
			t.Fatalf("NewReader() = %T, want unsupportedReader off darwin", reader)
		}
		return
	}
	cli, ok := reader.(*securityCLI)
	if !ok {
		t.Fatalf("NewReader() = %T, want *securityCLI", reader)
	}
	if cli.bin != securityBin {
		t.Errorf("bin = %q, want %q: the release transport is never redirected", cli.bin, securityBin)
	}
	for _, entry := range cli.env {
		if strings.HasPrefix(entry, "AGCTL_FAKE_") || strings.HasPrefix(entry, "AGENTCTL_") {
			t.Errorf("a release child environment carries %q", entry)
		}
	}
}

func TestReleaseSecurityBinIsAbsolute(t *testing.T) {
	t.Parallel()
	if securityBin != "/usr/bin/security" {
		t.Errorf("securityBin = %q, want /usr/bin/security: never resolved through PATH", securityBin)
	}
}
