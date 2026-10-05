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

package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

type useSubject struct {
	storeDir string
	service  string
	tree     secret.Tree
	audit    secret.Target
}

func useBuildSubject(paths *config.Paths, env *claude.EnvView, live bool, store *config.AccountRecord, inherited string) (*useSubject, *useReport) {
	if !live {
		if store == nil {
			return nil, useRefused(claude.SwapRefusal{Kind: claude.SwapNotOwned}, "", "a namespaced swap needs the record that owns the store")
		}
		var sha8 string
		if store.Kind.Owned != nil {
			sha8 = store.Kind.Owned.ExportSHA8
		}
		service := claude.LiveService + "-" + sha8
		kind, valid := claude.Classify(service)
		storeDir := paths.NamespaceDir(store.AccountUUID, store.OrganizationUUID)
		if !valid || kind.Live || !paths.IsUnderNamespaceRoot(storeDir) {
			return nil, useRefused(claude.SwapRefusal{Kind: claude.SwapNotOwned}, "", "the store's recorded export spelling does not name a namespace agentctl owns")
		}
		spelled := claude.ExportSpelling(storeDir)
		if spelled != inherited {
			return nil, useRefused(claude.SwapRefusal{Kind: claude.SwapNotOwned}, service, fmt.Sprintf("this store is named `%s`, but `%s`'s namespace now spells `%s`; the store moved, so agentctl would take the Claude Code locks in a different directory than the session reads — see `agentctl claude doctor`", inherited, store.AccountUUID, spelled))
		}
		return &useSubject{storeDir: storeDir, service: service, tree: secret.TreeOwn, audit: secret.NamespaceTarget(sha8)}, nil
	}
	if value, present := claude.SecureStorageNamespace(env); present {
		return nil, useRefused(claude.SwapRefusal{Kind: claude.SwapLiveNamespaceEnv}, "", fmt.Sprintf("`%s` is set to `%s` in this shell, so this environment names a namespace rather than the live store; run without it, or target that namespace instead", claude.SecureStorageEnv, usePrintable(value)))
	}
	subject := &useSubject{storeDir: claude.LiveStoreDir(env), service: claude.ServiceName(env), tree: secret.TreeLive, audit: secret.TargetLive}
	// This resolution diagnoses an unreachable store only; the held lock must
	// independently resolve and validate its own directory before any write.
	if _, err := claude.Canonical(subject.storeDir); err != nil {
		return nil, useRefused(claude.SwapRefusal{Kind: claude.SwapLiveUnreachable}, subject.service, fmt.Sprintf("the live store `%s` could not be resolved: %v", subject.storeDir, err))
	}
	return subject, nil
}

type useIdentityFailure struct {
	kind     claude.SwapRefusalKind
	detail   string
	claimed  string
	resolved string
	ownWrite bool
}

func (f *useIdentityFailure) report(service string) *useReport {
	switch {
	case f.claimed != "":
		named := fmt.Sprintf("the server says it belongs to `%s`", f.resolved)
		if f.ownWrite {
			named = fmt.Sprintf("agentctl wrote those bytes into the item for `%s`", f.resolved)
		}
		return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionIdentityMismatch}, service, fmt.Sprintf("the outgoing credential cannot be adopted: the credential in the live item says it belongs to `%s`, but %s; agentctl will not guess which account it is", f.claimed, named))
	case f.kind == claude.SwapProfileUnavailable:
		return useRefused(claude.SwapRefusal{Kind: f.kind}, service, fmt.Sprintf("agentctl could not ask the server whose credential the live item holds (`%s`); nothing was written — check the connection and run this again", usePrintable(f.detail)))
	case f.kind == claude.SwapTokenExpired:
		return useRefused(claude.SwapRefusal{Kind: f.kind}, service, "the live credential has expired and agentctl did not write it, so nothing can say whose it is; send one message in Claude Code, which refreshes it, then run this again")
	default:
		return useRefused(claude.SwapRefusal{Kind: f.kind}, service, f.detail)
	}
}

func useIdentify(ctx context.Context, source claude.ProfileSource, item *claude.Credentials, item8 string, log func() (secret.AuditTail, error), now int64) (*claude.Identity, *claude.Profile, *useIdentityFailure) {
	var profile *claude.Profile
	var resolved *claude.Identity
	expired := item.AccessExpired(now, 0)
	if !expired {
		var err error
		profile, err = source.ProfileOf(ctx, item)
		if err != nil {
			status, ok := errors.AsType[*errs.HTTPError](err)
			if !ok || (status.Status != 401 && status.Status != 403) {
				return nil, nil, &useIdentityFailure{kind: claude.SwapProfileUnavailable, detail: err.Error()}
			}
			expired = true
		} else {
			resolved = &claude.Identity{AccountUUID: profile.AccountUUID, OrganizationUUID: new(profile.OrganizationUUID)}
		}
	}
	if expired {
		tail, err := log()
		if err != nil {
			note := fmt.Sprintf("the live credential has expired, and agentctl's audit log could not be read (%v) to tell whether agentctl wrote it; see `agentctl claude doctor`", err)
			if configErr, ok := errors.AsType[*errs.ConfigError](err); ok {
				note = "a swap of the live store will not proceed unrecorded: " + configErr.Error()
			}
			return nil, nil, &useIdentityFailure{kind: claude.SwapAuditRefused, detail: note}
		}
		resolved = useIdentityByOwnWrite(tail, item8)
		if resolved == nil {
			return nil, nil, &useIdentityFailure{kind: claude.SwapTokenExpired}
		}
	}
	if claimed := item.Identity(); claimed != nil && !useIdentitiesAgree(claimed, resolved) {
		return nil, nil, &useIdentityFailure{claimed: useDisplayIdentity(claimed), resolved: useDisplayIdentity(resolved), ownWrite: expired}
	}
	return resolved, profile, nil
}

func useIdentityByOwnWrite(tail secret.AuditTail, item8 string) *claude.Identity {
	if item8 == "" {
		return nil
	}
	for _, v := range slices.Backward(tail.Entries) {
		event, ok := v.Event.(*secret.WriteEvent)
		if !ok || event.Target != secret.TargetLive || event.ToDigest8 != item8 || (event.Outcome != secret.WriteApplied && event.Outcome != secret.WriteUnknown) {
			continue
		}
		// The newest matching write is authoritative, even when an older
		// matching entry carried identity and this one does not.
		if event.IncomingIdentity == nil {
			return nil
		}
		return &claude.Identity{AccountUUID: event.IncomingIdentity.AccountUUID, OrganizationUUID: event.IncomingIdentity.OrganizationUUID}
	}
	return nil
}

func useIdentitiesAgree(a, b *claude.Identity) bool {
	return a.AccountUUID == b.AccountUUID && (a.OrganizationUUID == nil || b.OrganizationUUID == nil || *a.OrganizationUUID == *b.OrganizationUUID)
}

func useDisplayIdentity(identity *claude.Identity) string {
	text := usePrintable(identity.AccountUUID)
	if identity.OrganizationUUID != nil {
		text += "/" + usePrintable(*identity.OrganizationUUID)
	}
	return text
}

func usePrintable(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch r {
		case '\x00':
			out.WriteString(`\0`)
		case '\t':
			out.WriteString(`\t`)
		case '\r':
			out.WriteString(`\r`)
		case '\n':
			out.WriteString(`\n`)
		case '\\', '\'', '"':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			if unicode.IsPrint(r) {
				out.WriteRune(r)
			} else {
				fmt.Fprintf(&out, `\u{%x}`, r)
			}
		}
	}
	return out.String()
}
