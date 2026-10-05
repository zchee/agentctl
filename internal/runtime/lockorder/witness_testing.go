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

package lockorder

// report is the tagged witness: a violation panics, so the test that
// reached the forbidden order fails loudly at the exact call site rather
// than threading a refusal up through code that was never meant to see
// one. The message is one literal with nothing interpolated into it, so a
// strings scan finds it whole in a tagged binary and the release gate can
// prove it absent from a shipped one.
func report(err *OrderError) error {
	panic("agentctl lock order violated: the configuration lock was requested while this operation holds a Codex namespace guard")
}
