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

package claude

import (
	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/secret"
)

// ToKeychainStdinLine builds the keychain update line for this credential,
// ready for `security -i`'s standard input.
//
// The line itself is assembled by [secret.NewKeychainStdinLine], which
// owns the wire shape, the quoting rule and the length bound; this method
// owns the secret half — the serialized blob — and wipes it once the line
// is sealed, so the plaintext document never outlives the call.
//
// account is the item's acct attribute and service is the target item's
// service name. Both are recorded in the returned value so the write
// transport can refuse a line built for a different item than the one it
// was asked to write.
//
// A finished line — trailing newline included — over
// [secret.KeychainLineLimit] returns [secret.LineTooLongError], whose exit
// classification is the lettered refusal for an over-long credential line.
func (c *Credentials) ToKeychainStdinLine(account, service string) (*secret.KeychainStdinLine, error) {
	blob, err := c.BlobJSON()
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(blob)
	return secret.NewKeychainStdinLine(account, service, blob)
}
