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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestTaggedSeamVariableNamesAreTheDocumentedOnes(t *testing.T) {
	if keychainBackendEnv != "AGENTCTL_KEYCHAIN_BACKEND" {
		t.Errorf("keychainBackendEnv = %q, want AGENTCTL_KEYCHAIN_BACKEND", keychainBackendEnv)
	}
	if securityBinEnv != "AGENTCTL_SECURITY_BIN" {
		t.Errorf("securityBinEnv = %q, want AGENTCTL_SECURITY_BIN", securityBinEnv)
	}
	if fakeKnobPrefix != "AGCTL_FAKE_" {
		t.Errorf("fakeKnobPrefix = %q, want AGCTL_FAKE_", fakeKnobPrefix)
	}
}

func TestTaggedFactoryFailsClosed(t *testing.T) {
	// A tagged build with no stand-in wired must not have a keychain at all:
	// the real security(1) is not a fallback, because a build that could
	// reach the developer's keychain by omission eventually reaches it by
	// accident. Constructing and calling the reader is therefore safe here,
	// and is the assertion.
	tests := map[string]struct {
		backend string
		bin     string
	}{
		"success: nothing wired falls closed to disabled":    {backend: "", bin: ""},
		"success: backend none disables outright":            {backend: "none", bin: ""},
		"success: backend none wins over a wired stand-in":   {backend: "none", bin: "/stand-in/security"},
		"error: an unknown backend value still finds no bin": {backend: "real", bin: ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// An empty value reads the same as an unset variable through
			// os.Getenv, so t.Setenv covers both the unset and the wired
			// shapes.
			t.Setenv(keychainBackendEnv, tt.backend)
			t.Setenv(securityBinEnv, tt.bin)

			reader := NewReader()
			if _, ok := reader.(DisabledReader); !ok {
				t.Fatalf("NewReader() = %T, want DisabledReader", reader)
			}
			status := reader.Preflight(t.Context())
			want := KeychainStatus{State: KeychainStateUnavailable, Reason: "disabled"}
			if diff := gocmp.Diff(want, status); diff != "" {
				t.Errorf("Preflight() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTaggedFactoryWiresTheStandIn(t *testing.T) {
	t.Setenv(keychainBackendEnv, "")
	t.Setenv(securityBinEnv, "/stand-in/security")
	t.Setenv("AGCTL_FAKE_SECURITY_LOG", "/stand-in/argv.log")

	reader := NewReader()
	cli, ok := reader.(*securityCLI)
	if !ok {
		t.Fatalf("NewReader() = %T, want *securityCLI", reader)
	}
	if cli.bin != "/stand-in/security" {
		t.Errorf("bin = %q, want the wired stand-in", cli.bin)
	}
	if cli.readBudget != ReadTimeout || cli.dumpBudget != DumpTimeout {
		t.Errorf("budgets = %v/%v, want %v/%v", cli.readBudget, cli.dumpBudget, ReadTimeout, DumpTimeout)
	}

	var sawKnob bool
	for _, entry := range cli.env {
		if entry == "AGCTL_FAKE_SECURITY_LOG=/stand-in/argv.log" {
			sawKnob = true
		}
	}
	if !sawKnob {
		t.Errorf("the stand-in's knobs did not reach the child environment: %q", cli.env)
	}
}
