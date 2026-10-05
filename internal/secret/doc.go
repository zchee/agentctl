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

// Package secret holds the on-disk primitives the credential store is
// built from. The ground rule across the package is that every path that
// decides whether a write may happen is reached exactly once: locks are
// taken on inodes that are never unlinked, directories that hold them are
// opened without following symbolic links, and any error that is not plain
// contention fails closed, because the alternative is two processes
// rotating one refresh chain.
package secret
