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

// Package commands implements the Claude subcommands over the provider,
// store and render packages.
//
// Each command is a struct holding its collaborators — the keychain
// reader, the usage client, the pass runner, the clock — so a test can
// drive a whole command against a temporary store and a local HTTP
// server without touching the process environment. The command tree in
// the cli package parses flags into typed options and hands them here;
// nothing in this package reads a flag or prints usage.
package commands
