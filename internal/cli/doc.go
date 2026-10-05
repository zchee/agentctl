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

// Package cli defines the agentctl command-line surface: the command tree
// with every subcommand, flag, default and help string, the duration grammar
// shared by the interval and timeout flags, and the exit codes that the
// credential-swap outcomes of `claude use` report.
//
// The package parses the command line into typed option structs and
// dispatches through a Handlers value injected by the caller, so the surface
// stays reviewable in one place while the command implementations arrive
// independently.
package cli
