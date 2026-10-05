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
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/lockfile"
)

// RegistryVersion is the schema version of a registry with Claude accounts
// and nothing else.
//
// The version is derived at every write from what the document holds, not
// remembered from what was read: a registry whose last Codex row was
// removed goes back to this version, and an older build can read it again.
// The two constants are therefore a property of the content, not a
// setting.
const RegistryVersion = 1

// RegistryVersionCodex is the schema version of a registry that holds at
// least one Codex account.
//
// A build without Codex support refuses this file by version rather than
// ignoring the member it does not know: such a build would drop every
// Codex row on its next write, and a silently emptied registry is worse
// than one that says it is too new.
const RegistryVersionCodex = 2

// RegistryLockWait is how long a registry update waits for the
// configuration lock before giving up. Short on purpose: the only thing
// held under this lock is a rewrite of a small file, so a wait this long
// already means something is wrong.
const RegistryLockWait = 5 * time.Second

// Registry is the account registry document, one JSON file holding one
// record per account this store has been told about.
//
// The registry is small and rewritten whole. It is still written under a
// lock and through a temporary file, because two processes racing to add
// an account must not leave a truncated document behind.
type Registry struct {
	// Version is the schema version; derived from the content at every
	// write.
	Version int `json:"version"`
	// Accounts is every Claude account this store knows about, in
	// insertion order.
	Accounts []AccountRecord `json:"accounts"`
	// ForgottenServices is the keychain service names the user has asked
	// status and doctor to stop reporting. Separate from
	// [AccountRecord.Forgotten] because the rows this hides have no record
	// to carry a flag: such an item is discovered from the keychain
	// listing alone. The name is remembered, nothing else; the keychain
	// item itself is never touched.
	ForgottenServices []string `json:"forgotten_services"`
	// CodexAccounts is every Codex account this store knows about, in
	// insertion order: a list of its own rather than a fifth account kind,
	// because the two providers share a file and nothing else. It is
	// dropped from the document when empty, so a registry with no Codex
	// account serializes to exactly the bytes a Codex-unaware build wrote.
	CodexAccounts []CodexAccountRecord `json:"codex_accounts,omitzero"`
}

// AccountRecord is one Claude account, keyed by (account UUID,
// organization UUID).
type AccountRecord struct {
	// AccountUUID is the Anthropic account UUID.
	AccountUUID string `json:"account_uuid"`
	// OrganizationUUID is the organization UUID, or [UnknownOrg] when the
	// login could not name one.
	OrganizationUUID string `json:"organization_uuid"`
	// Email is the account's email address, when it is known.
	Email *string `json:"email"`
	// OrgName is the organization's display name, when it is known.
	OrgName *string `json:"org_name"`
	// Label is a user-chosen label.
	Label *string `json:"label"`
	// Kind says where the credentials live and whether agentctl may write
	// them.
	Kind AccountKind `json:"kind"`
	// Forgotten reports whether the user has asked for this row to be
	// hidden.
	Forgotten bool `json:"forgotten"`
	// CreatedAt is when the record was created, RFC 3339 in UTC.
	CreatedAt string `json:"created_at"`
}

// Key returns the (account, organization) pair that keys this record.
func (r *AccountRecord) Key() (string, string) {
	return r.AccountUUID, r.OrganizationUUID
}

// DisplayID returns the identifier shown in tables and accepted by
// --account: the bare account UUID when that is unambiguous within all,
// and <acct>/<org> when it is not.
func (r *AccountRecord) DisplayID(all []AccountRecord) string {
	duplicated := 0
	for i := range all {
		if all[i].AccountUUID == r.AccountUUID {
			duplicated++
		}
	}
	if duplicated > 1 {
		return r.AccountUUID + "/" + r.OrganizationUUID
	}
	return r.AccountUUID
}

// OwnedKind is the payload of an owned account: created by
// `agentctl claude login`, with credentials in this store that agentctl
// refreshes.
type OwnedKind struct {
	// ExportSpelling is the namespace directory as it was spelled at login
	// time, NFC normalized, recorded so a moved store can be reported
	// rather than silently producing a different keychain service name.
	ExportSpelling string `json:"export_spelling"`
	// ExportSHA8 is the eight-hex-digit digest of ExportSpelling — the
	// keychain service name a Claude Code session pointed at this
	// namespace would migrate to.
	ExportSHA8 string `json:"export_sha8"`
}

// ConfigDirReadOnlyKind is the payload of a keychain item belonging to
// another Claude configuration directory. Read-only, always.
type ConfigDirReadOnlyKind struct {
	// Dir is the configuration directory the item is named after.
	Dir string `json:"dir"`
	// Service is the keychain service name.
	Service string `json:"service"`
	// SharesLiveDir reports whether Dir resolves to the same physical
	// directory as the live store, which makes this a stale sibling rather
	// than a separate account.
	SharesLiveDir bool `json:"shares_live_dir"`
}

// ForeignKind is the payload of a credential that exists on the machine
// but belongs to something else; there is nothing in it agentctl may read.
type ForeignKind struct {
	// Source is what owns it: a third-party tool, or an environment
	// variable.
	Source string `json:"source"`
}

// AccountKind says where an account's credentials live and whether
// agentctl may write them. Exactly one variant is set; [NewAccountKind]
// constructors keep that true.
type AccountKind struct {
	// Owned is set for an account created by `agentctl claude login`.
	Owned *OwnedKind
	// Live is set for the credentials a running Claude Code session is
	// using. Read-only, always: a refresh here would rotate the token out
	// from under the session.
	Live bool
	// ConfigDirReadOnly is set for a keychain item belonging to another
	// Claude configuration directory.
	ConfigDirReadOnly *ConfigDirReadOnlyKind
	// Foreign is set for a credential that belongs to something that is
	// neither agentctl nor Claude Code.
	Foreign *ForeignKind
}

// AccountKindOwned returns the kind of an account this store owns.
func AccountKindOwned(exportSpelling, exportSHA8 string) AccountKind {
	return AccountKind{Owned: &OwnedKind{ExportSpelling: exportSpelling, ExportSHA8: exportSHA8}}
}

// AccountKindLive returns the kind of the live session's credentials.
func AccountKindLive() AccountKind {
	return AccountKind{Live: true}
}

// AccountKindConfigDirReadOnly returns the kind of a read-only item named
// after another configuration directory.
func AccountKindConfigDirReadOnly(dir, service string, sharesLiveDir bool) AccountKind {
	return AccountKind{ConfigDirReadOnly: &ConfigDirReadOnlyKind{Dir: dir, Service: service, SharesLiveDir: sharesLiveDir}}
}

// AccountKindForeign returns the kind of a credential owned by something
// else.
func AccountKindForeign(source string) AccountKind {
	return AccountKind{Foreign: &ForeignKind{Source: source}}
}

// Name returns the kind as one stable, machine-readable token, for JSON
// reports, the Kind column, and log fields.
func (k AccountKind) Name() string {
	switch {
	case k.Owned != nil:
		return "owned"
	case k.Live:
		return "live"
	case k.ConfigDirReadOnly != nil:
		return "config_dir"
	case k.Foreign != nil:
		return "foreign"
	default:
		return ""
	}
}

// MarshalJSONTo writes the kind as a tagged object, the tag first.
func (k AccountKind) MarshalJSONTo(enc *jsontext.Encoder) error {
	switch {
	case k.Owned != nil:
		return json.MarshalEncode(enc, struct {
			Kind string `json:"kind"`
			OwnedKind
		}{Kind: "owned", OwnedKind: *k.Owned})
	case k.Live:
		return json.MarshalEncode(enc, struct {
			Kind string `json:"kind"`
		}{Kind: "live"})
	case k.ConfigDirReadOnly != nil:
		return json.MarshalEncode(enc, struct {
			Kind string `json:"kind"`
			ConfigDirReadOnlyKind
		}{Kind: "config_dir_read_only", ConfigDirReadOnlyKind: *k.ConfigDirReadOnly})
	case k.Foreign != nil:
		return json.MarshalEncode(enc, struct {
			Kind string `json:"kind"`
			ForeignKind
		}{Kind: "foreign", ForeignKind: *k.Foreign})
	default:
		return errors.New("an account kind must name a variant")
	}
}

// UnmarshalJSONFrom reads a tagged kind object, refusing a tag this build
// does not know.
func (k *AccountKind) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var raw jsontext.Value
	if err := json.UnmarshalDecode(dec, &raw); err != nil {
		return err
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	*k = AccountKind{}
	switch probe.Kind {
	case "owned":
		var v OwnedKind
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		k.Owned = &v
	case "live":
		k.Live = true
	case "config_dir_read_only":
		var v ConfigDirReadOnlyKind
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		k.ConfigDirReadOnly = &v
	case "foreign":
		var v ForeignKind
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		k.Foreign = &v
	default:
		return fmt.Errorf("`%s` is not a known account kind", probe.Kind)
	}
	return nil
}

// RefreshPolicy says whether agentctl refreshes an owned Codex grant on
// its own.
type RefreshPolicy string

const (
	// RefreshAuto refreshes when the access token is expired or rejected —
	// the default, and the only mode in which agentctl ever sends a
	// refresh token.
	RefreshAuto RefreshPolicy = "auto"
	// RefreshNever never sends a refresh; the row reports that a fresh
	// login is needed instead.
	RefreshNever RefreshPolicy = "never"
)

// CodexOwnedKind is the payload of a Codex account created by
// `agentctl codex login`.
type CodexOwnedKind struct {
	// ExportSpelling is the namespace directory as it was spelled at login
	// time, recorded so a moved store can be reported. It is never a path
	// agentctl writes through — every write derives its path from the
	// validated ids under the Codex root.
	ExportSpelling string `json:"export_spelling"`
	// Refresh says whether agentctl may send a refresh POST for this
	// account. Always written, never omitted: a policy absent from the
	// file would read as "whatever this build defaults to", and the whole
	// point of `--refresh never` is that the answer does not depend on the
	// build.
	Refresh RefreshPolicy `json:"refresh"`
}

// CodexHomeReadOnlyKind is the payload of another Codex home, recorded by
// import. Read-only.
type CodexHomeReadOnlyKind struct {
	// Dir is the home directory the credentials were read from.
	Dir string `json:"dir"`
}

// CodexKind says where a Codex account's credentials live and whether
// agentctl may write them. Exactly one variant is set.
type CodexKind struct {
	// Owned is set for an account created by `agentctl codex login`.
	Owned *CodexOwnedKind
	// Live is set for the credentials the user's own codex is using.
	// Read-only, always: agentctl never writes a file under a Codex home
	// it did not create.
	Live bool
	// HomeReadOnly is set for another Codex home recorded by import.
	HomeReadOnly *CodexHomeReadOnlyKind
}

// CodexKindOwned returns the kind of a Codex account this store owns.
func CodexKindOwned(exportSpelling string, refresh RefreshPolicy) CodexKind {
	return CodexKind{Owned: &CodexOwnedKind{ExportSpelling: exportSpelling, Refresh: refresh}}
}

// CodexKindLive returns the kind of the user's own Codex credentials.
func CodexKindLive() CodexKind {
	return CodexKind{Live: true}
}

// CodexKindHomeReadOnly returns the kind of an imported, read-only Codex
// home.
func CodexKindHomeReadOnly(dir string) CodexKind {
	return CodexKind{HomeReadOnly: &CodexHomeReadOnlyKind{Dir: dir}}
}

// MarshalJSONTo writes the kind as a tagged object, the tag first.
func (k CodexKind) MarshalJSONTo(enc *jsontext.Encoder) error {
	switch {
	case k.Owned != nil:
		return json.MarshalEncode(enc, struct {
			Kind string `json:"kind"`
			CodexOwnedKind
		}{Kind: "owned", CodexOwnedKind: *k.Owned})
	case k.Live:
		return json.MarshalEncode(enc, struct {
			Kind string `json:"kind"`
		}{Kind: "live"})
	case k.HomeReadOnly != nil:
		return json.MarshalEncode(enc, struct {
			Kind string `json:"kind"`
			CodexHomeReadOnlyKind
		}{Kind: "home_read_only", CodexHomeReadOnlyKind: *k.HomeReadOnly})
	default:
		return errors.New("a codex account kind must name a variant")
	}
}

// UnmarshalJSONFrom reads a tagged kind object, refusing a tag this build
// does not know and defaulting an absent refresh policy to [RefreshAuto].
func (k *CodexKind) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var raw jsontext.Value
	if err := json.UnmarshalDecode(dec, &raw); err != nil {
		return err
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	*k = CodexKind{}
	switch probe.Kind {
	case "owned":
		var v CodexOwnedKind
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		switch v.Refresh {
		case "":
			v.Refresh = RefreshAuto
		case RefreshAuto, RefreshNever:
		default:
			return fmt.Errorf("`%s` is not a known refresh policy", v.Refresh)
		}
		k.Owned = &v
	case "live":
		k.Live = true
	case "home_read_only":
		var v CodexHomeReadOnlyKind
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		k.HomeReadOnly = &v
	default:
		return fmt.Errorf("`%s` is not a known codex account kind", probe.Kind)
	}
	return nil
}

// CodexAccountRecord is one Codex account, keyed by (ChatGPT user id,
// ChatGPT account id). The record never holds a token: it holds what the
// identity is, what the user called it, and which kind it is.
type CodexAccountRecord struct {
	// ChatGPTUserID is the ChatGPT user id from the id token's claims.
	ChatGPTUserID string `json:"chatgpt_user_id"`
	// ChatGPTAccountID is the ChatGPT account (workspace) id from the same
	// claims.
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	// Email is the account's email address, when the claims carried one.
	Email *string `json:"email"`
	// PlanType is the subscription tier, as the wire spells it.
	PlanType *string `json:"plan_type"`
	// Label is a user-chosen label.
	Label *string `json:"label"`
	// Kind says where the credentials live and whether agentctl may write
	// them.
	Kind CodexKind `json:"kind"`
	// Forgotten reports whether the user has asked for this row to be
	// hidden.
	Forgotten bool `json:"forgotten"`
	// CreatedAt is when the record was created, RFC 3339 in UTC.
	CreatedAt string `json:"created_at"`
}

// NewRecord builds a Claude account record with CreatedAt set to now,
// refusing an identifier that is unusable as a directory name.
func NewRecord(accountUUID, organizationUUID string, kind AccountKind) (AccountRecord, error) {
	if err := ValidateSegment(accountUUID); err != nil {
		return AccountRecord{}, err
	}
	if err := ValidateSegment(organizationUUID); err != nil {
		return AccountRecord{}, err
	}
	return AccountRecord{AccountUUID: accountUUID, OrganizationUUID: organizationUUID, Kind: kind, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

// LoadRegistry reads the registry, treating an absent file as an empty
// one.
//
// It returns a [errs.IOError] when the file exists but cannot be read,
// and a [errs.ConfigError] when it is not a registry this build
// understands.
func LoadRegistry(ctx context.Context, p *Paths) (*Registry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := p.ConfigFile()
	bytes, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &Registry{Version: RegistryVersion}, nil
	case err != nil:
		return nil, errs.NewIO(fmt.Sprintf("could not read `%s`", path), err)
	}

	var registry Registry
	if err := json.Unmarshal(bytes, &registry); err != nil {
		return nil, errs.NewConfig(fmt.Sprintf("`%s` is not a valid agentctl config: %v", path, err))
	}
	if registry.Version < RegistryVersion || registry.Version > RegistryVersionCodex {
		return nil, errs.NewConfig(fmt.Sprintf("`%s` is version %d, but this build understands version %d and version %d", path, registry.Version, RegistryVersion, RegistryVersionCodex))
	}
	// A version-1 document with Codex rows in it was written by something
	// that did not derive the version — a hand edit, or a merge of two
	// files. Refused rather than accepted, because the two halves disagree
	// about what the file is: a Codex-unaware build would read the same
	// bytes, believe the version, and erase the rows on its next write.
	if registry.Version == RegistryVersion && len(registry.CodexAccounts) > 0 {
		return nil, errs.NewConfig(fmt.Sprintf("`%s` says version %d but holds %d Codex account(s); a registry with Codex accounts is version %d", path, RegistryVersion, len(registry.CodexAccounts), RegistryVersionCodex))
	}
	return &registry, nil
}

// UpdateRegistry reads the registry, applies f to it, and writes it back
// — all under one hold of the configuration lock.
//
// This is the only way to change the registry, and the reason is the
// lock: flock is per open file description, so a caller that took the
// lock itself and then called a self-locking save would deadlock against
// its own descriptor, and a caller that did not take the lock would read,
// think, and write across a window in which another process could have
// added an account, which the write would then erase. Re-reading the file
// inside the lock closes both. f must not itself touch the registry on
// disk: it runs while the lock is held.
//
// It returns a [errs.RefusedError] when the lock is held past
// [RegistryLockWait], a [errs.ConfigError] when the file on disk is not a
// registry this build understands, and a [errs.IOError] for any
// filesystem failure along the way.
func UpdateRegistry(ctx context.Context, p *Paths, f func(*Registry)) error {
	if err := p.EnsureDirs(ctx); err != nil {
		return err
	}

	lockPath := p.ConfigLock()
	guard, err := lockfile.Lock(ctx, lockPath, time.Now().Add(RegistryLockWait))
	if err != nil {
		return errs.NewRefused(0, fmt.Sprintf("could not lock `%s`: %v", lockPath, err))
	}
	defer func() { _ = guard.Release() }()

	registry, err := LoadRegistry(ctx, p)
	if err != nil {
		return err
	}
	f(registry)
	return registry.writeLocked(p)
}

// derivedVersion is the version this document is, from what it holds:
// derived rather than stored, so the stamp cannot drift from the content.
// An emptied Codex list returns the file to version 1, and no code path
// can add a Codex row and forget to raise the version.
func (r *Registry) derivedVersion() int {
	if len(r.CodexAccounts) == 0 {
		return RegistryVersion
	}
	return RegistryVersionCodex
}

// document returns the exact bytes a save renames into place. Split out
// so the derivation is testable without a store, a lock or a filesystem:
// what a document serializes to is the whole of the compatibility
// promise.
func (r *Registry) document() ([]byte, error) {
	stamped := *r
	stamped.Version = r.derivedVersion()
	if len(stamped.CodexAccounts) == 0 {
		stamped.CodexAccounts = nil
	}
	if stamped.Accounts == nil {
		stamped.Accounts = []AccountRecord{}
	}
	if stamped.ForgottenServices == nil {
		stamped.ForgottenServices = []string{}
	}
	out, err := json.Marshal(&stamped, jsontext.WithIndent("  "))
	if err != nil {
		return nil, errs.NewConfig(fmt.Sprintf("could not serialize the config: %v", err))
	}
	return out, nil
}

// writeLocked writes the registry atomically: an exclusive 0600 temporary
// beside the target, a flush to stable storage, then a rename. The caller
// holds the configuration lock.
func (r *Registry) writeLocked(p *Paths) error {
	document, err := r.document()
	if err != nil {
		return err
	}

	target := p.ConfigFile()
	tmp := target + ".tmp." + hex8()
	if err := writeThenRename(tmp, target, document); err != nil {
		_ = os.Remove(tmp)
		return errs.NewIO(fmt.Sprintf("could not write `%s`", target), err)
	}
	return nil
}

// Upsert inserts or replaces the record for its (account, organization)
// key.
func (r *Registry) Upsert(rec AccountRecord) {
	for i := range r.Accounts {
		if r.Accounts[i].AccountUUID == rec.AccountUUID && r.Accounts[i].OrganizationUUID == rec.OrganizationUUID {
			r.Accounts[i] = rec
			return
		}
	}
	r.Accounts = append(r.Accounts, rec)
}

// Get looks up one record by its exact key, or returns nil.
func (r *Registry) Get(acct, org string) *AccountRecord {
	for i := range r.Accounts {
		if r.Accounts[i].AccountUUID == acct && r.Accounts[i].OrganizationUUID == org {
			return &r.Accounts[i]
		}
	}
	return nil
}

// ResolveID resolves a user-supplied identifier to exactly one record.
//
// <account>/<organization> is tried first, because it is the spelling the
// ambiguity message tells the user to fall back to and so must never
// itself be ambiguous. Anything else is matched against three fields at
// once — the account UUID, the email address, and the label — and has to
// name exactly one record. It returns a [errs.ConfigError] when nothing
// matches, or when more than one record does, in which case the message
// lists the unambiguous spellings so the user can pick one.
func (r *Registry) ResolveID(id string) (*AccountRecord, error) {
	if acct, org, found := strings.Cut(id, "/"); found {
		if rec := r.Get(acct, org); rec != nil {
			return rec, nil
		}
	}

	var matches []*AccountRecord
	for i := range r.Accounts {
		rec := &r.Accounts[i]
		if rec.AccountUUID == id || (rec.Email != nil && *rec.Email == id) || (rec.Label != nil && *rec.Label == id) {
			matches = append(matches, rec)
		}
	}

	switch len(matches) {
	case 0:
		return nil, errs.NewConfig(fmt.Sprintf("no account matches `%s`", id))
	case 1:
		return matches[0], nil
	default:
		candidates := make([]string, 0, len(matches))
		for _, rec := range matches {
			candidates = append(candidates, rec.AccountUUID+"/"+rec.OrganizationUUID)
		}
		return nil, errs.NewConfig(fmt.Sprintf("`%s` matches %d accounts; use one of: %s", id, len(matches), strings.Join(candidates, ", ")))
	}
}

// writeThenRename writes bytes to tmp at 0600, flushes it to stable
// storage, renames it over target, and pins the target's mode.
func writeThenRename(tmp, target string, bytes []byte) error {
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(bytes); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	return os.Chmod(target, FileMode)
}

// hex8 returns eight random hexadecimal digits for a temporary file name.
func hex8() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// rand.Read never fails on supported platforms; a failure here
		// means the process cannot continue safely anyway.
		panic(err)
	}
	return hex.EncodeToString(buf[:])
}
