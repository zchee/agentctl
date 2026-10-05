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

// Package fixtures embeds the shared test data tree: three fake executables
// written as POSIX shell scripts, and the JSON, TOML and terminal-capture
// documents that tests use as parser inputs and subprocess payloads.
//
// Embedding does not preserve file modes, so any helper that materialises
// one of the scripts on disk must write it with mode 0o755 itself.
package fixtures

import "embed"

// FS holds every file under claude/, codex/ and rc-screens/, plus the three
// fake executable scripts at the package root.
//
//go:embed all:claude all:codex all:rc-screens fake-codex.sh fake-security.sh fake-tmux.sh
var FS embed.FS
