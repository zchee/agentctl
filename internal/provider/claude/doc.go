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

// Package claude edits Claude Code's own configuration documents without
// disturbing a single byte it does not own.
//
// The central contract is byte preservation: Claude Code treats
// ~/.claude.json as its private state, so a peer that rewrites one member
// must carry every other byte over exactly as it was. The package enforces
// that structurally rather than by promise. A document is accepted only when
// re-rendering it with the same pretty-printer Claude Code uses (two-space
// indent, members in the order found, JavaScript's number and string
// spellings) reproduces its bytes exactly; a document that cannot be
// reproduced is refused untouched, because any rewrite of it would silently
// reshape bytes outside the members being edited. Once a document passes
// that gate, the rewrite is a splice: the bytes outside the replaced and
// deleted members are copied from the input verbatim, and only the new
// member value is freshly encoded.
//
// Nothing in this package opens, locks or renames files. Callers read the
// document, plan the rewrite in memory, and may re-verify that the document
// is still the bytes the plan was built from before writing the result.
//
// One credential-parsing divergence is deliberate: an empty accessToken
// string cannot be sealed into locked memory, so it is treated as a missing
// field and refused at parse time, rather than being carried along and
// failing later as an unauthorized request.
package claude
