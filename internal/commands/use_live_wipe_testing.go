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

package commands

import (
	"sync/atomic"

	"github.com/zchee/agentctl/internal/provider/claude"
)

// useParsedObserver, when a test installs one, sees every credential the
// swap and undo read helpers parse, before it is returned to its owner, so
// the test can check that the owner wiped it.
var useParsedObserver atomic.Pointer[func(*claude.Credentials)]

func useObserveParsed(credentials *claude.Credentials) {
	if observe := useParsedObserver.Load(); observe != nil && credentials != nil {
		(*observe)(credentials)
	}
}
