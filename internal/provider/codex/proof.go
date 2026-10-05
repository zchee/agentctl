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

import "github.com/zchee/agentctl/internal/config"

// OwnedRecord proves a registry row owns a namespace with validated path segments.
// Its export spelling is display-only, never a write target.
type OwnedRecord struct {
	user    string
	account string
	export  *string
	refresh config.RefreshPolicy
}

// Owned validates an owned registry row rather than trusting editable ids.
func Owned(record *config.CodexAccountRecord) *OwnedRecord {
	if record == nil || record.Kind.Owned == nil || config.ValidateCodexSegment(record.ChatGPTUserID) != nil || config.ValidateCodexSegment(record.ChatGPTAccountID) != nil {
		return nil
	}
	return &OwnedRecord{user: record.ChatGPTUserID, account: record.ChatGPTAccountID, export: new(record.Kind.Owned.ExportSpelling), refresh: record.Kind.Owned.Refresh}
}

// User returns the validated user segment.
func (o *OwnedRecord) User() string { return o.user }

// Account returns the validated account segment.
func (o *OwnedRecord) Account() string { return o.account }

// ExportSpelling returns a display-only path, if the record supplied one.
func (o *OwnedRecord) ExportSpelling() *string { return o.export }

// Refresh returns the registry's refresh policy.
func (o *OwnedRecord) Refresh() config.RefreshPolicy { return o.refresh }
