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

package main

import (
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/zchee/agentctl/internal/testutil"
)

// TestMain registers the binary under test and the harness helpers as
// script commands, so the end-to-end scripts and the tests share one
// build of one binary - there is no separately built executable to go
// stale against the sources.
func TestMain(m *testing.M) {
	commands := testutil.ScriptCommands()
	commands["agentctl"] = main
	testscript.Main(m, commands)
}

// TestTaggedBuild is the guard the whole end-to-end target rests on:
// this file only compiles under the testing tag, and the constant only
// reports true under it, so a run of this test is evidence the suite ran
// with its seams rather than being silently skipped.
func TestTaggedBuild(t *testing.T) {
	if !testutil.TaggedBuild {
		t.Fatalf("the end-to-end suite is running without the testing build tag")
	}
}

// TestScripts runs every end-to-end script against the isolated world
// ScriptSetup builds.
func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/script",
		Setup:               testutil.ScriptSetup,
		RequireExplicitExec: true,
	})
}
