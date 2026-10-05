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

package codex

// PostPermit is the command thread's permission to enter the refresh driver.
// Its zero value carries no client; read-only workers and watch hold no permit.
type PostPermit struct{ transport *RefreshClient }

// NewPostPermit constructs the sole production refresh permission.
func NewPostPermit() *PostPermit { return &PostPermit{transport: NewRefreshClientFromEnv()} }

func (p *PostPermit) client() *RefreshClient {
	if p == nil {
		return nil
	}
	return p.transport
}
