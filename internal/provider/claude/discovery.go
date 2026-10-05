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

// Turning a machine's keychain and this store's registry into a list of
// rows.
//
// Discovery is where the awkward facts about a real machine get handled,
// and all of them come from one place: a keychain item is named after a
// string, not after an account. So the same account can appear under
// several names, two names can point at one directory, and a name can
// outlive the credentials it was created for.
//
// The rules that fall out of that, each earning its place:
//
//   - Fold by digest, never by path. Two entries are the same account only
//     when their token digests match. A live item and the item named after
//     the live directory's resolved spelling can name one physical
//     directory and hold different credentials, so folding by path would
//     merge two accounts into one row and show the wrong numbers.
//   - Same directory, different credentials, is a stale sibling. Hidden by
//     default and counted in the footer, because it is real but not
//     actionable.
//   - Identity comes from the credential's own tokenAccount, or from
//     .claude.json for the live row only. A blob without one is
//     identity unknown and stays visible, because the fix — log in — is
//     something the user can act on.
//   - An unrecognised Claude Code item is unclaimed, and shown. It is a
//     true statement about the machine. `accounts forget` hides it.
//   - claude-switcher items are listed and never touched. They belong to a
//     third-party tool that rewrites the live item on every switch;
//     agentctl neither reads nor writes them.

package claude

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
)

// SwitcherServicePrefix marks the keychain items a third-party account
// switcher owns. They are listed so the user can see they exist, and never
// read or written.
const SwitcherServicePrefix = "claude-switcher:"

// keychainPrefixes is what discovery asks the keychain to list.
//
// "Claude Code" rather than the credentials prefix, so the legacy
// per-directory API-key items show up in the listing and get rejected by
// [Classify] — proving they are ignored, rather than never looking at them.
var keychainPrefixes = [2]string{"Claude Code", SwitcherServicePrefix}

// migrationProbeSkipped is appended to an owned row's note when the
// keychain could not be read, so the migration probe could not run.
const migrationProbeSkipped = "keychain locked — migration probe skipped"

// MaxClaudeJSONBytes is the largest .claude.json that will be read for the
// live row's identity.
//
// The file belongs to Claude Code, not to agentctl: it accumulates session
// history and project state, it is measured in hundreds of kilobytes on a
// machine in daily use, and nothing bounds it. Sixteen mebibytes is far
// above anything observed and still small enough that reading it cannot
// exhaust a terminal's memory; past it, the live row simply reports an
// unknown identity rather than a failure.
const MaxClaudeJSONBytes int64 = 16 << 20

// CredentialsFileName is the credential document's name inside a namespace
// directory, the same name the peer tooling reads.
const CredentialsFileName = ".credentials.json"

// The lock artefacts a Claude Code session leaves inside a store directory
// it is using. Their presence means something other than agentctl owns the
// namespace right now, and every read of them here is read-only.
const (
	// refreshLockName is the peer's primary refresh lock.
	refreshLockName = ".oauth_refresh.lock"
	// storageWriteLockName is the peer's mutex directory.
	storageWriteLockName = ".storage-write.lock"
)

// Discovery is everything one pass found.
type Discovery struct {
	// Rows is one row per account, in the order they should be rendered.
	Rows []AccountRow
	// Preflight is what the keychain preflight said.
	Preflight secret.KeychainStatus
	// Listing is the keychain items seen, attributes only.
	Listing []secret.ServiceEntry
}

// Discover builds the row list for one pass: the live row first, then one
// row per registry record, then every keychain item nothing claims, then
// the environment-token row when that variable is set. A cancelled ctx
// returns whatever was built so far rather than nothing: the rows already
// in hand are still true.
func Discover(ctx context.Context, registry *config.Registry, paths *config.Paths, reader secret.Reader, env *EnvView) Discovery {
	preflight := reader.Preflight(ctx)
	keychainReadable := preflight.State == secret.KeychainStateUnlocked

	var listing []secret.ServiceEntry
	if keychainReadable {
		for _, prefix := range keychainPrefixes {
			entries, err := reader.ListServices(ctx, prefix)
			if err != nil {
				slog.DebugContext(ctx, "could not list keychain services", slog.String("prefix", prefix), slog.Any("error", err))
				continue
			}
			listing = append(listing, entries...)
		}
		sortDedupeListing(&listing)
	}

	var rows []AccountRow
	liveService := ServiceName(env)
	live := liveRow(ctx, liveService, preflight, reader, env)
	liveDigests := digestsOf(live.Credentials)
	rows = append(rows, live)

	claimedServices := []string{liveService}
	for i := range registry.Accounts {
		if ctx.Err() != nil {
			return Discovery{Rows: rows, Preflight: preflight, Listing: listing}
		}
		record := &registry.Accounts[i]
		if record.Kind.ConfigDirReadOnly != nil {
			claimedServices = append(claimedServices, record.Kind.ConfigDirReadOnly.Service)
		}
		if row, ok := recordRow(ctx, record, registry, paths, reader, preflight, listing, liveDigests); ok {
			rows = append(rows, row)
		}
	}

	rows = append(rows, unclaimedRows(ctx, listing, claimedServices, registry.ForgottenServices, liveService, reader, env, liveDigests)...)

	if env.OAuthTokenSet {
		rows = append(rows, envTokenRow())
	}

	return Discovery{Rows: rows, Preflight: preflight, Listing: listing}
}

// LiveIdentity reports who the live credentials belong to, from
// .claude.json alone.
//
// For a caller that wants the live identity and has no business holding
// the live credential. The live row itself prefers the keychain blob's own
// identity and falls back to this; a caller that is not building a row
// deliberately does not get that first half, because reading the item
// means reading a token pair, and a notice is not worth a credential read.
func LiveIdentity(env *EnvView) *Identity {
	return claudeJSONIdentity(ClaudeJSONPath(env))
}

// resolved is one credential read classified for discovery's data flow.
type resolved struct {
	// credentials is set exactly when the read produced the credential.
	credentials *Credentials
	// outcome is the read classified through the one-store rule.
	outcome secret.Outcome
}

// resolveKeychain reads one keychain item, without ever falling through to
// a file: a keychain failure classifies as locked or transient, and the
// closed outcome vocabulary has no value that names another store.
func resolveKeychain(ctx context.Context, reader secret.Reader, service string) resolved {
	sealed, err := reader.Read(ctx, service)
	outcome := secret.KeychainItem(service).Classify(err)
	if outcome.Kind != secret.OutcomeCredential {
		return resolved{outcome: outcome}
	}

	var credentials *Credentials
	parseErr := sealed.WithPlaintext(func(blob []byte) error {
		parsed, err := ParseBlob(blob)
		if err != nil {
			return err
		}
		credentials = parsed
		return nil
	})
	if parseErr != nil {
		return resolved{outcome: secret.Outcome{Kind: secret.OutcomeTransient, Reason: parseErr.Error()}}
	}
	return resolved{credentials: credentials, outcome: outcome}
}

// resolveFile reads a namespace's credential file, read-only.
//
// The leaf is refused when it is not a plain file: a symbolic link planted
// at the credential path must not be followed to wherever it points.
func resolveFile(paths *config.Paths, nsDir string) resolved {
	dir, err := secret.OpenDirUnder(paths.NamespaceRoot(), nsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return resolved{outcome: secret.Outcome{Kind: secret.OutcomeAbsent}}
	}
	if err != nil {
		return resolved{outcome: secret.Outcome{Kind: secret.OutcomeTransient, Reason: err.Error()}}
	}
	defer func() { _ = unix.Close(dir) }()
	file := secret.NewSecretFile(paths.NamespaceRoot(), dir, CredentialsFileName, filepath.Join(nsDir, CredentialsFileName))
	read, err := file.ReadStrict(secret.MaxCredentialsBytes)
	if err != nil {
		return resolved{outcome: secret.Outcome{Kind: secret.OutcomeTransient, Reason: err.Error()}}
	}
	if !read.Present {
		return resolved{outcome: secret.Outcome{Kind: secret.OutcomeAbsent}}
	}
	credentials, err := ParseBlob(read.Bytes)
	memguard.WipeBytes(read.Bytes)
	if err != nil {
		return resolved{outcome: secret.Outcome{Kind: secret.OutcomeTransient, Reason: err.Error()}}
	}
	return resolved{credentials: credentials, outcome: secret.Outcome{Kind: secret.OutcomeCredential}}
}

// liveRow builds the row for whatever Claude Code is using right now.
func liveRow(ctx context.Context, service string, preflight secret.KeychainStatus, reader secret.Reader, env *EnvView) AccountRow {
	var credentials *Credentials
	var state AccountState
	source := SourceNone

	switch preflight.State {
	case secret.KeychainStateUnlocked:
		result := resolveKeychain(ctx, reader, service)
		switch result.outcome.Kind {
		case secret.OutcomeCredential:
			credentials, state, source = result.credentials, StateOfOK(), SourceKeychain
		case secret.OutcomeAbsent:
			state = StateOfNeedsLogin()
		case secret.OutcomeLocked:
			state = StateOfKeychainLocked("")
		default:
			state = StateOfError(result.outcome.Reason)
		}
	// Not even attempted: a preflight that says the keychain is not
	// readable makes the read a certain prompt or a certain failure, and
	// the one-store rule forbids answering it from the file.
	case secret.KeychainStateLocked:
		state = StateOfKeychainLocked("")
	case secret.KeychainStateTimeout:
		state = StateOfKeychainTimeout()
	case secret.KeychainStateUnavailable:
		state = StateOfError("keychain unavailable: " + preflight.Reason)
	default:
		state = StateOfError(string(secret.KeychainClassUnsupported))
	}

	// The live row is the one place .claude.json is evidence: it records
	// the last login through this configuration directory, which is
	// exactly what the live credentials are.
	var identity *Identity
	if credentials != nil {
		identity = credentials.Identity()
	}
	if identity == nil {
		identity = claudeJSONIdentity(ClaudeJSONPath(env))
	}
	if credentials != nil && identity == nil {
		state = StateOfIdentityUnknown()
	}

	record := recordFromIdentity(identity, config.AccountKindLive())
	id := "live"
	if identity != nil {
		id = identity.AccountUUID
	}
	return AccountRow{
		ID:               id,
		Record:           record,
		State:            state,
		Source:           source,
		Credentials:      credentials,
		VisibleByDefault: true,
		Note:             fmt.Sprintf("keychain service `%s`", service),
	}
}

// recordRow builds the row for one registry record; ok is false when the
// record folds into another row.
func recordRow(ctx context.Context, record *config.AccountRecord, registry *config.Registry, paths *config.Paths, reader secret.Reader, preflight secret.KeychainStatus, listing []secret.ServiceEntry, liveDigests *Digests) (AccountRow, bool) {
	id := record.DisplayID(registry.Accounts)

	if record.Forgotten {
		return AccountRow{ID: id, Record: *record, State: StateOfForgotten(), Source: SourceNone, VisibleByDefault: false}, true
	}

	switch kind := record.Kind; {
	// Synthesized separately, from the environment rather than from the
	// registry, because which item is live depends on the environment the
	// command was run in.
	case kind.Live:
		return AccountRow{}, false
	case kind.Foreign != nil:
		return AccountRow{
			ID:               id,
			Record:           *record,
			State:            StateOfForeign(kind.Foreign.Source),
			Source:           SourceNone,
			VisibleByDefault: true,
			Note:             "belongs to " + kind.Foreign.Source,
		}, true
	case kind.ConfigDirReadOnly != nil:
		return configDirRow(ctx, id, record, reader, preflight, liveDigests)
	case kind.Owned != nil:
		return ownedRow(ctx, id, record, paths, reader, preflight, listing), true
	default:
		return AccountRow{}, false
	}
}

// configDirRow builds the row for a read-only keychain item another Claude
// configuration directory owns; ok is false when it folds into the live
// row.
func configDirRow(ctx context.Context, id string, record *config.AccountRecord, reader secret.Reader, preflight secret.KeychainStatus, liveDigests *Digests) (AccountRow, bool) {
	kind := record.Kind.ConfigDirReadOnly
	var result resolved
	if preflight.State == secret.KeychainStateUnlocked {
		result = resolveKeychain(ctx, reader, kind.Service)
	}
	credentials := result.credentials
	if foldsIntoLive(liveDigests, credentials) {
		return AccountRow{}, false
	}

	var state AccountState
	if kind.SharesLiveDir {
		state = StateOfStaleSiblingOfLive()
	} else {
		state = readOnlyState(credentials, preflight)
	}
	source := SourceNone
	if credentials != nil {
		source = SourceKeychain
	}
	return AccountRow{
		ID:               id,
		Record:           *record,
		State:            state,
		Source:           source,
		Credentials:      credentials,
		VisibleByDefault: !kind.SharesLiveDir,
		Note:             fmt.Sprintf("keychain service `%s`", kind.Service),
	}, true
}

// ownedRow builds the row for a namespace this store owns: the credential
// file is the source, unless the migration probe says a Claude Code
// session has taken the namespace over.
func ownedRow(ctx context.Context, id string, record *config.AccountRecord, paths *config.Paths, reader secret.Reader, preflight secret.KeychainStatus, listing []secret.ServiceEntry) AccountRow {
	kind := record.Kind.Owned
	nsDir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
	canonical := canonicalSHA8(nsDir)
	if canonical == kind.ExportSHA8 {
		canonical = ""
	}

	// With an unreadable keychain the migration probe cannot run. The row
	// still uses the file, and says so: reporting needs login here would
	// be wrong and alarming.
	var probeListing []secret.ServiceEntry
	if preflight.State == secret.KeychainStateUnlocked {
		probeListing = listing
	}
	activity := detectForeign(ctx, nsDir, kind.ExportSHA8, canonical, probeListing, reader)

	var note string
	if preflight.State != secret.KeychainStateUnlocked {
		if preflight.State == secret.KeychainStateUnsupported {
			note = string(secret.KeychainClassUnsupported)
		} else {
			note = migrationProbeSkipped
		}
	}

	var credentials *Credentials
	var state AccountState
	source := SourceNone
	switch {
	case activity.migratedService != "":
		if result := resolveKeychain(ctx, reader, activity.migratedService); result.credentials != nil {
			credentials = result.credentials
			source = SourceKeychain
		}
		state = StateOfMigratedToKeychain(activity.migratedService)
	case activity.lockName != "":
		if result := resolveFile(paths, nsDir); result.credentials != nil {
			credentials = result.credentials
			source = SourceFile
		}
		state = StateOfClaudeSessionDetected(activity.lockName, activity.lockAgeMillis)
	default:
		result := resolveFile(paths, nsDir)
		switch result.outcome.Kind {
		case secret.OutcomeCredential:
			credentials, state, source = result.credentials, StateOfOK(), SourceFile
		case secret.OutcomeAbsent:
			state = StateOfNeedsLogin()
		case secret.OutcomeLocked:
			state = StateOfKeychainLocked("")
		default:
			state = StateOfError(result.outcome.Reason)
		}
	}

	if note != "" && credentials == nil {
		if preflight.State == secret.KeychainStateUnsupported {
			note = string(secret.KeychainClassUnsupported)
		} else {
			note = migrationProbeSkipped + " (migration unknown)"
		}
	}

	return AccountRow{
		ID:               id,
		Record:           *record,
		State:            state,
		Source:           source,
		Credentials:      credentials,
		VisibleByDefault: true,
		Note:             note,
	}
}

// unclaimedRows builds rows for keychain items no registry record claims.
func unclaimedRows(ctx context.Context, listing []secret.ServiceEntry, claimed, forgotten []string, liveService string, reader secret.Reader, env *EnvView, liveDigests *Digests) []AccountRow {
	liveDir := LiveStoreDir(env)
	liveSpellingSHA8 := SHA8(ExportSpelling(liveDir))
	liveCanonicalSHA8 := canonicalSHA8(liveDir)

	var rows []AccountRow
	for _, entry := range listing {
		if slices.Contains(claimed, entry.Service) {
			continue
		}
		if rest, isSwitcher := strings.CutPrefix(entry.Service, SwitcherServicePrefix); isSwitcher {
			rows = append(rows, foreignRow(entry.Service, rest))
			continue
		}

		// Legacy per-directory API-key items classify as nothing at all
		// and are dropped here, never read.
		serviceKind, ok := Classify(entry.Service)
		if !ok || serviceKind.Live {
			continue
		}
		if entry.Service == liveService {
			continue
		}

		// Hidden at the user's request. Checked before the read, so a
		// forgotten item costs no find-generic-password and the keychain
		// is not touched on its account at all.
		if slices.Contains(forgotten, entry.Service) {
			rows = append(rows, forgottenServiceRow(entry.Service))
			continue
		}

		result := resolveKeychain(ctx, reader, entry.Service)
		credentials := result.credentials
		if foldsIntoLive(liveDigests, credentials) {
			continue
		}

		sharesLiveDir := serviceKind.Suffix == liveSpellingSHA8 || (liveCanonicalSHA8 != "" && serviceKind.Suffix == liveCanonicalSHA8)
		var identity *Identity
		if credentials != nil {
			identity = credentials.Identity()
		}
		var state AccountState
		switch {
		case sharesLiveDir:
			state = StateOfStaleSiblingOfLive()
		case credentials != nil && identity == nil:
			state = StateOfIdentityUnknown()
		default:
			state = StateOfUnclaimed()
		}

		record := recordFromIdentity(identity, config.AccountKindConfigDirReadOnly("", entry.Service, sharesLiveDir))
		id := entry.Service
		if identity != nil {
			id = identity.AccountUUID
		}
		source := SourceNone
		if credentials != nil {
			source = SourceKeychain
		}
		rows = append(rows, AccountRow{
			ID:               id,
			Record:           record,
			State:            state,
			Source:           source,
			Credentials:      credentials,
			VisibleByDefault: !sharesLiveDir,
			Note:             fmt.Sprintf("keychain service `%s`", entry.Service),
		})
	}
	return rows
}

// forgottenServiceRow is a keychain item `accounts forget` has hidden:
// named, never read.
//
// Still a row rather than nothing at all, so the show-everything flag and
// `accounts list --all` can show what was hidden and `accounts unforget`
// names something the user can see.
func forgottenServiceRow(service string) AccountRow {
	return AccountRow{
		ID:               service,
		Record:           recordFromIdentity(nil, config.AccountKindConfigDirReadOnly("", service, false)),
		State:            StateOfForgotten(),
		Source:           SourceNone,
		VisibleByDefault: false,
		Note:             fmt.Sprintf("keychain service `%s`; hidden by `accounts forget`", service),
	}
}

// foreignRow is a claude-switcher item: listed, hidden, and never read.
func foreignRow(service, email string) AccountRow {
	record := recordFromIdentity(nil, config.AccountKindForeign("claude-switcher"))
	if email != "" {
		record.Email = &email
	}
	return AccountRow{
		ID:               service,
		Record:           record,
		State:            StateOfForeign("claude-switcher"),
		Source:           SourceNone,
		VisibleByDefault: false,
		Note:             "foreign: managed by claude-account-switcher, never read or written",
	}
}

// envTokenRow is the row for the environment token variable, which
// short-circuits every credential store.
func envTokenRow() AccountRow {
	return AccountRow{
		ID:               "env",
		Record:           recordFromIdentity(nil, config.AccountKindForeign(OAuthTokenEnv)),
		State:            StateOfEnvToken(),
		Source:           SourceEnv,
		VisibleByDefault: true,
		Note:             OAuthTokenEnv + " is set and short-circuits every credential store",
	}
}

// readOnlyState is the state of a row agentctl may read but never refresh.
func readOnlyState(credentials *Credentials, preflight secret.KeychainStatus) AccountState {
	if credentials != nil {
		if credentials.Identity() == nil {
			return StateOfIdentityUnknown()
		}
		return StateOfOK()
	}
	switch preflight.State {
	case secret.KeychainStateUnsupported:
		return StateOfError(string(secret.KeychainClassUnsupported))
	case secret.KeychainStateLocked:
		return StateOfKeychainLocked("")
	case secret.KeychainStateTimeout:
		return StateOfKeychainTimeout()
	case secret.KeychainStateUnavailable:
		return StateOfError("keychain unavailable: " + preflight.Reason)
	default:
		return StateOfNeedsLogin()
	}
}

// foldsIntoLive reports whether a candidate credential is the live one,
// by digest alone: two entries are one account exactly when their token
// digests match, and a digest that cannot be computed matches nothing.
func foldsIntoLive(live *Digests, candidate *Credentials) bool {
	if live == nil || candidate == nil {
		return false
	}
	digests, err := candidate.Digests()
	if err != nil {
		return false
	}
	return *live == digests
}

// digestsOf fingerprints one credential, or returns nil when there is none
// or its secrets can no longer be opened.
func digestsOf(credentials *Credentials) *Digests {
	if credentials == nil {
		return nil
	}
	digests, err := credentials.Digests()
	if err != nil {
		return nil
	}
	return &digests
}

// canonicalSHA8 is the sha8 of a directory's canonical spelling, or empty
// when it does not resolve.
func canonicalSHA8(dir string) string {
	canonical, err := Canonical(dir)
	if err != nil {
		return ""
	}
	return SHA8(ExportSpelling(canonical))
}

// recordFromIdentity builds a display-only record from an identity that
// may not exist.
func recordFromIdentity(identity *Identity, kind config.AccountKind) config.AccountRecord {
	record := config.AccountRecord{OrganizationUUID: config.UnknownOrg, Kind: kind}
	if identity == nil {
		return record
	}
	record.AccountUUID = identity.AccountUUID
	if identity.OrganizationUUID != nil {
		record.OrganizationUUID = *identity.OrganizationUUID
	}
	record.Email = identity.Email
	record.OrgName = identity.OrgName
	return record
}

// foreignActivity is what somebody else is doing with a namespace: at most
// one of the lock artefact and the migrated service is set, and neither
// means agentctl may proceed to write.
type foreignActivity struct {
	// lockName is a Claude Code lock artefact's file name, when one is
	// present.
	lockName string
	// lockAgeMillis is how long ago that artefact was last touched.
	lockAgeMillis uint64
	// migratedService is the keychain service a session migrated this
	// namespace's credentials into, when one exists.
	migratedService string
}

// detectForeign looks for signs that something other than agentctl owns a
// namespace. Everything here is read-only, and the keychain is consulted
// only when the listing actually names one of this namespace's two
// possible services — which keeps a namespace with no migration from
// issuing any item read at all.
func detectForeign(ctx context.Context, nsDir, exportSHA8, canonicalSHA8 string, listing []secret.ServiceEntry, reader secret.Reader) foreignActivity {
	artefacts := []string{filepath.Join(nsDir, refreshLockName), filepath.Join(nsDir, storageWriteLockName)}
	// The legacy lock sits beside the directory, named after its resolved
	// path with .lock appended.
	if canonical, err := Canonical(nsDir); err == nil {
		artefacts = append(artefacts, canonical+".lock")
	}
	for _, artefact := range artefacts {
		if ageMillis, ok := artefactAgeMillis(artefact); ok {
			return foreignActivity{lockName: filepath.Base(artefact), lockAgeMillis: ageMillis}
		}
	}

	for _, sha8 := range []string{exportSHA8, canonicalSHA8} {
		if sha8 == "" {
			continue
		}
		service := LiveService + "-" + sha8
		if !listingHas(listing, service) {
			continue
		}
		_, err := reader.Read(ctx, service)
		// Listed but gone by the time it was read: nothing owns it now.
		// Any other failure — locked, timed out, refused — leaves the
		// listing as evidence enough: an item under this namespace's name
		// exists, so agentctl stops writing. Failing closed here costs a
		// refresh; failing open costs the user's session.
		if errors.Is(err, secret.ErrItemNotFound) {
			continue
		}
		return foreignActivity{migratedService: service}
	}
	return foreignActivity{}
}

// artefactAgeMillis reports how long ago a lock artefact was modified, or
// false when it is not there. An artefact younger than the clock —
// modified "in the future" — reports age zero rather than wrapping.
func artefactAgeMillis(path string) (uint64, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	age := time.Since(info.ModTime())
	if age < 0 {
		return 0, true
	}
	return uint64(age / time.Millisecond), true
}

// listingHas reports whether the attribute listing names service.
func listingHas(listing []secret.ServiceEntry, service string) bool {
	for i := range listing {
		if listing[i].Service == service {
			return true
		}
	}
	return false
}

// sortDedupeListing sorts the listing by service name and drops duplicate
// services, keeping the first of each run.
func sortDedupeListing(listing *[]secret.ServiceEntry) {
	slices.SortStableFunc(*listing, func(a, b secret.ServiceEntry) int { return strings.Compare(a.Service, b.Service) })
	*listing = slices.CompactFunc(*listing, func(a, b secret.ServiceEntry) bool { return a.Service == b.Service })
}

// claudeJSON is just the oauthAccount object of a .claude.json, and
// nothing else.
//
// Deserializing into this rather than into a generic tree matters because
// of what the rest of the file is: session history, project lists, MCP
// configuration and tips state, all of which the decoder skips once it
// knows no field wants them. The live file on a machine in daily use is
// hundreds of kilobytes with no documented ceiling.
type claudeJSON struct {
	OAuthAccount *oauthAccount `json:"oauthAccount"`
}

// oauthAccount is the identity half of .claude.json's oauthAccount member.
type oauthAccount struct {
	AccountUUID      *string `json:"accountUuid"`
	EmailAddress     *string `json:"emailAddress"`
	OrganizationUUID *string `json:"organizationUuid"`
	OrganizationName *string `json:"organizationName"`
}

// claudeJSONFingerprint identifies one state of the file: a re-read is
// skipped exactly when every one of these matches.
type claudeJSONFingerprint struct {
	dev   uint64
	ino   uint64
	size  int64
	mtime int64
}

// claudeJSONMemo is the last .claude.json that was parsed, keyed by its
// identity.
//
// The watch loop re-runs discovery every few seconds, and .claude.json is
// rewritten continuously by running Claude Code sessions but changes its
// oauthAccount only at a login. Re-reading and re-parsing a file of this
// size on every frame to learn that nothing changed is the kind of work a
// terminal UI cannot afford, so the fingerprint decides whether the parse
// runs at all. The path is part of the key so that two stores — or two
// tests — cannot read each other's answers.
var claudeJSONMemo struct {
	sync.Mutex
	path        string
	fingerprint claudeJSONFingerprint
	identity    *Identity
	valid       bool
}

// claudeJSONIdentity reads oauthAccount out of a .claude.json.
//
// Every failure is nil: a missing file, one past [MaxClaudeJSONBytes], and
// a partial write caught mid-parse all mean the same thing to the caller —
// identity unavailable for this pass, with the live row still rendered.
// Symbolic links are followed: this is Claude Code's file, agentctl only
// reads it, and on real machines the path is routinely a link into the
// real configuration directory.
func claudeJSONIdentity(path string) *Identity {
	// The stat is what makes the memo worth having: one syscall against a
	// read of hundreds of kilobytes and a parse of the same.
	if fingerprint, ok := fingerprintOf(path); ok {
		claudeJSONMemo.Lock()
		if claudeJSONMemo.valid && claudeJSONMemo.path == path && claudeJSONMemo.fingerprint == fingerprint {
			identity := cloneIdentity(claudeJSONMemo.identity)
			claudeJSONMemo.Unlock()
			return identity
		}
		claudeJSONMemo.Unlock()
	}

	bytes, fingerprint, ok := readClaudeJSON(path)
	if !ok {
		return nil
	}

	var document claudeJSON
	var identity *Identity
	if err := json.Unmarshal(bytes, &document); err == nil && document.OAuthAccount != nil && document.OAuthAccount.AccountUUID != nil {
		identity = &Identity{
			AccountUUID:      *document.OAuthAccount.AccountUUID,
			OrganizationUUID: document.OAuthAccount.OrganizationUUID,
			Email:            document.OAuthAccount.EmailAddress,
			OrgName:          document.OAuthAccount.OrganizationName,
		}
	}

	// Stored against the state the read itself observed, not the one the
	// stat above saw: those differ exactly when the file changed in
	// between, and the bytes just parsed belong to the later of the two.
	claudeJSONMemo.Lock()
	claudeJSONMemo.path = path
	claudeJSONMemo.fingerprint = fingerprint
	claudeJSONMemo.identity = cloneIdentity(identity)
	claudeJSONMemo.valid = true
	claudeJSONMemo.Unlock()
	return identity
}

// readClaudeJSON reads the file whole, bounded, following links, together
// with the fingerprint of the bytes it actually read.
func readClaudeJSON(path string) ([]byte, claudeJSONFingerprint, bool) {
	read, err := secret.ReadFileFollowing(path, MaxClaudeJSONBytes)
	if err != nil || !read.Present {
		return nil, claudeJSONFingerprint{}, false
	}
	fingerprint := claudeJSONFingerprint{dev: read.Snap.Dev, ino: read.Snap.Ino, size: read.Snap.Size, mtime: read.Snap.MtimeNS}
	return read.Bytes, fingerprint, true
}

// fingerprintOf stats path, following links, and fingerprints the result.
func fingerprintOf(path string) (claudeJSONFingerprint, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return claudeJSONFingerprint{}, false
	}
	return fingerprintFromInfo(info), true
}

// fingerprintFromInfo derives the (device, inode, size, mtime) key from
// one stat result.
func fingerprintFromInfo(info fs.FileInfo) claudeJSONFingerprint {
	fingerprint := claudeJSONFingerprint{size: info.Size(), mtime: info.ModTime().UnixNano()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		fingerprint.dev = uint64(stat.Dev)
		fingerprint.ino = stat.Ino
	}
	return fingerprint
}

// cloneIdentity deep-copies an identity so the memo and its callers never
// share pointers.
func cloneIdentity(identity *Identity) *Identity {
	if identity == nil {
		return nil
	}
	cloned := Identity{AccountUUID: identity.AccountUUID}
	if identity.OrganizationUUID != nil {
		cloned.OrganizationUUID = new(*identity.OrganizationUUID)
	}
	if identity.Email != nil {
		cloned.Email = new(*identity.Email)
	}
	if identity.OrgName != nil {
		cloned.OrgName = new(*identity.OrgName)
	}
	return &cloned
}
