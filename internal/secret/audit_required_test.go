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

package secret

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestAuditLockBreakRequiresEveryNonOptionalMember(t *testing.T) {
	tests := map[string]struct {
		member string
	}{
		"error: missing path":                {member: "path"},
		"error: missing store directory":     {member: "store_dir"},
		"error: missing tree":                {member: "tree"},
		"error: missing service":             {member: "service"},
		"error: missing target":              {member: "target"},
		"error: missing first sample":        {member: "sample_a"},
		"error: missing wall interval":       {member: "interval_wall_ms"},
		"error: missing monotonic interval":  {member: "interval_monotonic_ms"},
		"error: missing holder evidence":     {member: "holder_evidence"},
		"error: missing outcome":             {member: "outcome"},
		"error: missing first sample time":   {member: "sample_a.at"},
		"error: missing first sample mtime":  {member: "sample_a.mtime_ns"},
		"error: missing first sample age":    {member: "sample_a.age_ms"},
		"error: missing second sample time":  {member: "sample_b.at"},
		"error: missing second sample mtime": {member: "sample_b.mtime_ns"},
		"error: missing second sample age":   {member: "sample_b.age_ms"},
		"error: missing third sample time":   {member: "sample_c.at"},
		"error: missing third sample mtime":  {member: "sample_c.mtime_ns"},
		"error: missing third sample age":    {member: "sample_c.age_ms"},
	}
	line, err := auditEntryLine(NewAuditEntry(breakRecord(t)))
	if err != nil {
		t.Fatalf("serialize complete record: %v", err)
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			variants := map[string]struct {
				null bool
			}{
				"error: omitted": {},
				"error: null":    {null: true},
			}
			for name, variant := range variants {
				t.Run(name, func(t *testing.T) {
					var members map[string]jsontext.Value
					if err := json.Unmarshal([]byte(line), &members); err != nil {
						t.Fatalf("decode complete record: %v", err)
					}
					container := members
					parent, member, nested := strings.Cut(tt.member, ".")
					if nested {
						container = nil
						if err := json.Unmarshal(members[parent], &container); err != nil {
							t.Fatalf("decode %s: %v", parent, err)
						}
					} else {
						member = parent
					}
					if variant.null {
						container[member] = jsontext.Value("null")
					} else {
						delete(container, member)
					}
					if nested {
						body, err := json.Marshal(container)
						if err != nil {
							t.Fatalf("serialize %s: %v", parent, err)
						}
						members[parent] = body
					}
					body, err := json.Marshal(members)
					if err != nil {
						t.Fatalf("serialize incomplete record: %v", err)
					}
					if _, err := decodeAuditLine(body); err == nil {
						t.Fatalf("accepted absent required member %s (null=%t)", tt.member, variant.null)
					}

					paths := newAuditStoreWithRoot(t)
					if err := os.WriteFile(AuditLogPath(paths), []byte(line+string(body)+"\n"), 0o600); err != nil {
						t.Fatalf("write audit lines: %v", err)
					}
					tail, err := TailAuditLog(paths, 2)
					if err != nil {
						t.Fatalf("one malformed record must not stop the tail: %v", err)
					}
					if len(tail.Entries) != 1 || len(tail.Unreadable) != 1 || tail.Unreadable[0].Line != 2 {
						t.Fatalf("want one valid entry and unreadable line 2, got %+v", tail)
					}
				})
			}
		})
	}
}

func TestAuditLockBreakAcceptsZeroValuesAndOptionalMembers(t *testing.T) {
	tests := map[string]struct {
		omitOptional bool
	}{
		"success: optional members omitted": {omitOptional: true},
		"success: optional members null":    {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			record := breakRecord(t)
			record.Path = ""
			record.StoreDir = ""
			record.Service = ""
			record.IntervalWallMS = 0
			record.IntervalMonotonicMS = 0
			record.SampleA.MtimeNS = 0
			record.SampleA.AgeMS = 0
			record.SampleB = nil
			record.SampleC = nil
			record.Reason = ""
			entry := NewAuditEntry(record)
			line, err := auditEntryLine(entry)
			if err != nil {
				t.Fatalf("serialize zero-valued record: %v", err)
			}
			var members map[string]jsontext.Value
			if err := json.Unmarshal([]byte(line), &members); err != nil {
				t.Fatalf("decode zero-valued record: %v", err)
			}
			for _, member := range []string{"sample_b", "sample_c", "reason"} {
				if tt.omitOptional {
					delete(members, member)
				} else {
					members[member] = jsontext.Value("null")
				}
			}
			body, err := json.Marshal(members)
			if err != nil {
				t.Fatalf("serialize optional members: %v", err)
			}
			got, err := decodeAuditLine(body)
			if err != nil {
				t.Fatalf("required members are present, even when zero-valued: %v", err)
			}
			if diff := gocmp.Diff(entry, got); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
