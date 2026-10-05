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

package config

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// ImportIdentity is the identity learned from a credential, never from its path.
type ImportIdentity struct {
	AccountUUID      string
	OrganizationUUID *string
	Email            *string
	OrgName          *string
}

// ImportItem is a classified credential service and its directory metadata.
// Dir is nil for a listing-wide import and preserves the supplied spelling otherwise.
type ImportItem struct {
	Service       string
	Dir           *string
	Listed        bool
	Unsuffixed    bool
	SharesLiveDir bool
}

// ImportDecision describes one record, known account, skipped item, or warning.
// Record, KnownKind, Reason, and Warning distinguish its four variants.
type ImportDecision struct {
	Record    *AccountRecord
	Detail    string
	KnownID   string
	KnownKind string
	What      string
	Reason    string
	Warning   string
}

// ImportPlan contains the decisions in source order without changing the registry.
type ImportPlan struct {
	Decisions []ImportDecision
}

// Records returns the records this plan would add, in order.
func (p ImportPlan) Records() []AccountRecord {
	var records []AccountRecord
	for _, decision := range p.Decisions {
		if decision.Record != nil {
			records = append(records, *decision.Record)
		}
	}
	return records
}

// Lines returns one line per decision followed by the summary.
func (p ImportPlan) Lines() []string {
	lines := make([]string, 0, len(p.Decisions)+1)
	for _, d := range p.Decisions {
		switch {
		case d.Record != nil:
			lines = append(lines, fmt.Sprintf("import %s: %s/%s (%s)", importKindLabel(d.Record.Kind), d.Record.AccountUUID, d.Record.OrganizationUUID, d.Detail))
		case d.KnownKind != "":
			lines = append(lines, fmt.Sprintf("already known as %s: %s", d.KnownKind, d.KnownID))
		case d.Reason != "":
			lines = append(lines, fmt.Sprintf("skipped (%s): %s", d.Reason, d.What))
		default:
			lines = append(lines, "warning: "+d.Warning)
		}
	}
	return append(lines, p.Summary())
}

// Summary counts imported, skipped by sorted reason, and already-known entries.
func (p ImportPlan) Summary() string {
	var imported, known, total int
	skipped := make(map[string]int)
	for _, d := range p.Decisions {
		switch {
		case d.Record != nil:
			imported++
		case d.KnownKind != "":
			known++
		case d.Reason != "":
			skipped[d.Reason]++
			total++
		}
	}
	var parts []string
	for _, reason := range slices.Sorted(maps.Keys(skipped)) {
		if len(skipped) == 1 {
			parts = append(parts, reason)
		} else {
			parts = append(parts, fmt.Sprintf("%d %s", skipped[reason], reason))
		}
	}
	detail := ""
	if len(parts) > 0 {
		detail = " (" + strings.Join(parts, ", ") + ")"
	}
	return fmt.Sprintf("imported %d, skipped %d%s, already known %d", imported, total, detail, known)
}

// PlanKeychainImport records unclaimed directory credentials without downgrading
// existing records. identify is called once per eligible item; nil means unknown
// identity, including an item that disappeared between its listing and read.
// Callers supply classified items so this package need not depend on a provider.
func PlanKeychainImport(ctx context.Context, items []ImportItem, liveService string, existing *Registry, identify func(context.Context, string) *ImportIdentity) ImportPlan {
	claimed := map[string]bool{liveService: true}
	for _, record := range existing.Accounts {
		if record.Kind.ConfigDirReadOnly != nil {
			claimed[record.Kind.ConfigDirReadOnly.Service] = true
		}
	}
	var plan ImportPlan
	planned := make(map[[2]string]bool)
	for _, item := range items {
		detail, dir := item.Service, ""
		if item.Dir != nil {
			detail, dir = *item.Dir, *item.Dir
			if item.Unsuffixed {
				plan.Decisions = append(plan.Decisions, ImportDecision{What: detail, Reason: "names the live keychain item"})
				continue
			}
		}
		if claimed[item.Service] {
			if item.Dir != nil {
				id, kind := item.Service, "config-dir-read-only"
				if item.Service == liveService {
					id, kind = "live", "live"
				}
				plan.Decisions = append(plan.Decisions, ImportDecision{KnownID: id, KnownKind: kind})
			}
			continue
		}
		if !item.Listed {
			plan.Decisions = append(plan.Decisions, ImportDecision{What: fmt.Sprintf("no keychain item for %s (service `%s`)", detail, item.Service), Reason: "no keychain item"})
			continue
		}
		if item.Dir != nil && item.SharesLiveDir {
			plan.Decisions = append(plan.Decisions, ImportDecision{Warning: detail + " is an alias of the live config dir; recorded as a stale sibling"})
		}
		identity := identify(ctx, item.Service)
		account, org := item.Service, UnknownOrg
		if identity != nil {
			account = identity.AccountUUID
			if identity.OrganizationUUID != nil {
				org = *identity.OrganizationUUID
			}
		}
		if record := existing.Get(account, org); record != nil {
			plan.Decisions = append(plan.Decisions, ImportDecision{KnownID: record.DisplayID(existing.Accounts), KnownKind: importKindLabel(record.Kind)})
			continue
		}
		key := [2]string{account, org}
		if planned[key] {
			plan.Decisions = append(plan.Decisions, ImportDecision{What: account + "/" + org, Reason: "duplicate"})
			continue
		}
		kind := AccountKindConfigDirReadOnly(dir, item.Service, item.SharesLiveDir)
		record := AccountRecord{AccountUUID: account, OrganizationUUID: org, Kind: kind, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if identity != nil {
			var err error
			record, err = NewRecord(account, org, kind)
			if err != nil {
				plan.Decisions = append(plan.Decisions, ImportDecision{What: fmt.Sprintf("%s: %v", detail, err), Reason: "unusable ids"})
				continue
			}
			record.Email, record.OrgName = identity.Email, identity.OrgName
		} else {
			detail += ", identity unknown"
		}
		planned[key] = true
		plan.Decisions = append(plan.Decisions, ImportDecision{Record: &record, Detail: detail})
	}
	return plan
}

func importKindLabel(kind AccountKind) string {
	if kind.ConfigDirReadOnly != nil {
		return "config-dir-read-only"
	}
	return kind.Name()
}
