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
	"fmt"
	"maps"

	"github.com/rogpeppe/go-internal/testscript"
)

// ScriptCmd is an in-process testscript command whose resources live for
// one script run.
type ScriptCmd = func(ts *testscript.TestScript, neg bool, args []string)

var scriptCmds = map[string]ScriptCmd{}

// registerScriptCmd adds one in-process command. Each command family
// registers from its own file, so adding a family never edits a line
// another family also edits. A duplicate name is a programming error
// and panics at package initialisation, where it cannot be missed.
func registerScriptCmd(name string, cmd ScriptCmd) {
	if _, dup := scriptCmds[name]; dup {
		panic(fmt.Sprintf("testutil: script command %q registered twice", name))
	}
	scriptCmds[name] = cmd
}

// ScriptCmds returns the in-process commands registered by every family,
// as a fresh map the caller may hand to testscript.Params.Cmds.
func ScriptCmds() map[string]ScriptCmd {
	return maps.Clone(scriptCmds)
}
