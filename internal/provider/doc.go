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

// Package provider names the vendors agentctl reads usage for and defines
// the seam between "which vendor's API is this" and everything downstream
// of a fetch — the cache, the table, the exit status.
//
// The seam is narrow on purpose: one call, one account, one snapshot. The
// interesting per-vendor work (which endpoint, which headers, how a refresh
// is done, what a 401 body means) stays behind each implementation, and the
// commands are written against [UsageProvider] plus [FetchError] alone.
//
// # The credential does not cross the seam
//
// A usage client needs one thing from an account's credential — the headers
// that authenticate the request — and [UsageAuth] is exactly that and
// nothing else. [AccountRef] therefore carries a [UsageAuth] interface value
// rather than a concrete credential type, which is what lets a second
// vendor's client be written without either vendor's credential type
// appearing in the other's signature, and what keeps each plaintext token
// behind its vendor's single exposure site. The header value a client
// receives does carry the token; what it never receives is the sealed
// secret or a field it could expose a second time.
//
// # Why the error type is not the vendor's
//
// A status pass branches on four things and no more: "the token is dead"
// (the refresh-once trigger), "slow down" (the stale-cache path), "try
// again later" (everything transient) and "this will not work" (everything
// else). A client that surfaced its HTTP library's error type directly
// would push HTTP handling into the pass, and a second vendor would then
// push a second library's error type in beside it. [FetchError] is that
// four-way classification.
package provider
