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

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	provider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/secret"
)

func TestKeyringListingProbe(t *testing.T) {
	t.Parallel()
	const home = "/home/test/.codex"
	matched := secret.ServiceEntry{Service: provider.KeyringService, Account: provider.KeyringAccount(home)}
	tests := map[string]struct {
		listing keyringListing
		want    provider.KeyringProbe
	}{
		"success: unavailable listing is unknown": {listing: keyringListing{entries: []secret.ServiceEntry{matched}}, want: provider.KeyringUnknown},
		"success: absent service":                 {listing: keyringListing{available: true}, want: provider.KeyringAbsent},
		"success: exact account":                  {listing: keyringListing{entries: []secret.ServiceEntry{matched}, available: true}, want: provider.KeyringPresent},
		"success: accountless service":            {listing: keyringListing{entries: []secret.ServiceEntry{{Service: provider.KeyringService}}, available: true}, want: provider.KeyringPresent},
		"success: another account is absent":      {listing: keyringListing{entries: []secret.ServiceEntry{{Service: provider.KeyringService, Account: "another"}}, available: true}, want: provider.KeyringAbsent},
		"success: another service is absent":      {listing: keyringListing{entries: []secret.ServiceEntry{{Service: "another", Account: matched.Account}}, available: true}, want: provider.KeyringAbsent},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.want, tt.listing.probe(home)); diff != "" {
				t.Errorf("status probe (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.want, (doctorListings{keyringListing: tt.listing}).probe(home)); diff != "" {
				t.Errorf("doctor probe (-want +got):\n%s", diff)
			}
		})
	}
}
