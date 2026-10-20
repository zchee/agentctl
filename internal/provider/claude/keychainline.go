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

// ToKeychainUpdate builds the keychain update for this credential: the
// account object followed by the members carried beside it, as
// [Credentials.KeychainBlobJSON] writes them.
//
// The update itself is assembled by [secret.NewKeychainUpdate], which owns
// the name validation, the hex encoding and the size ceiling; this method
// owns the plaintext document and wipes it once the update is sealed, so the
// document never outlives the call.
//
// account is the item's acct attribute and service is the target item's
// service name. Both are recorded in the returned value so the write
// transport can refuse an update built for a different item than the one it
// was asked to write.
//
// A document over [secret.KeychainBlobLimit] returns
// [secret.BlobTooLargeError], whose exit classification is the
// credential_too_large refusal.
func (c *Credentials) ToKeychainUpdate(account, service string) (*secret.KeychainUpdate, error) {
	blob, err := c.KeychainBlobJSON()
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(blob)
	return secret.NewKeychainUpdate(account, service, blob)
}
