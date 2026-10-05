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

	"github.com/rogpeppe/go-internal/testscript"
)

func init() {
	registerScriptCmd("swap-immutable", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-immutable <file>")
		}
		path := ts.MkAbs(args[0])
		ts.Defer(func() { ts.Check(exec.Command("/usr/bin/chflags", "nouchg", path).Run()) })
		ts.Check(exec.Command("/usr/bin/chflags", "uchg", path).Run())
	})
}
