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
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	provider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/runtime/proc"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/usage"
)

var doctorReportedEnv = [...]string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID"}

// Doctor reports local Codex evidence without creating or repairing any file.
type Doctor struct {
	Paths   *config.Paths
	Env     provider.Env
	Present []string
	Reader  secret.Reader
	Out     io.Writer
}

// Run renders a read-only report, including unusable homes and state files.
// An unreadable registry or an unlistable Codex tree prevents the report.
func (d *Doctor) Run(ctx context.Context, opts cli.CodexDoctorOptions) error {
	registry, err := config.LoadRegistry(ctx, d.Paths)
	if err != nil {
		return err
	}
	report, err := d.build(ctx, registry.CodexAccounts)
	if err != nil {
		return err
	}
	if opts.JSON {
		data, err := json.Marshal(report, jsontext.WithIndent("  "))
		if err != nil {
			return errs.NewConfig("the report could not be serialized")
		}
		_, err = fmt.Fprintln(d.Out, string(data))
		return err
	}
	_, err = io.WriteString(d.Out, renderDoctor(report))
	return err
}

type doctorListings struct {
	codexAuth      []secret.ServiceEntry
	codexAvailable bool
	switcher       int
}

func (d *Doctor) listings(ctx context.Context) doctorListings {
	if d.Reader == nil {
		return doctorListings{}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	entries, err := d.Reader.ListServices(ctx, provider.KeyringService)
	switcher, _ := d.Reader.ListServices(ctx, "codex-switcher:")
	return doctorListings{codexAuth: entries, codexAvailable: err == nil, switcher: len(switcher)}
}

func (l doctorListings) probe(home string) provider.KeyringProbe {
	if !l.codexAvailable {
		return provider.KeyringUnknown
	}
	account := provider.KeyringAccount(home)
	for _, entry := range l.codexAuth {
		if entry.Service == provider.KeyringService && (entry.Account == "" || entry.Account == account) {
			return provider.KeyringPresent
		}
	}
	return provider.KeyringAbsent
}

func (d *Doctor) build(ctx context.Context, accounts []config.CodexAccountRecord) (doctorReport, error) {
	listings := d.listings(ctx)
	resolved, homeErr := provider.ResolveHome(d.Env)
	report := doctorReport{Version: 1, Home: doctorHome{SymlinkChain: []string{}}, Environment: []doctorEnv{}, Namespaces: []doctorNamespace{}, Orphans: []doctorOrphan{}, Audit: []string{}, Notes: []string{}}
	if homeErr == nil {
		report.Home.Path = &resolved.Dir
		report.Home.SymlinkChain = doctorSymlinks(resolved.Dir)
	} else {
		reason := homeErr.Error()
		report.Home.Error = &reason
	}
	report.Store = doctorStoreSection(ctx, report.Home.Path, listings)
	report.Live = d.live(ctx, report.Home.Path, report.Store.Read, accounts)
	for _, name := range doctorReportedEnv {
		report.Environment = append(report.Environment, doctorEnv{Name: name, Present: slices.Contains(d.Present, name)})
	}
	caused, _ := provider.GainedCodexKeychainAccounts(ctx, d.Paths)
	report.Foreign = doctorForeignSection(report.Home.Path, listings, caused)
	for i := range accounts {
		record := &accounts[i]
		owned := provider.Owned(record)
		if owned == nil {
			continue
		}
		dir, err := d.Paths.CodexNamespaceDir(owned.User(), owned.Account())
		if err != nil {
			continue
		}
		artifacts, present := doctorArtifacts(dir)
		ns := doctorNamespace{User: owned.User(), Acct: owned.Account(), Path: dir, CredentialsPresent: present, RefreshPolicy: string(record.Kind.Owned.Refresh), Artefacts: artifacts, Marker: doctorMarkerSection(ctx, d.Paths, owned.User(), owned.Account()), Notes: []string{}}
		ns.Lock = doctorLockSection(ctx, d.Paths, owned.User(), owned.Account(), &ns.Notes)
		for _, line := range artifacts {
			if strings.HasPrefix(line, "stray tmp") {
				ns.Notes = append(ns.Notes, "`accounts refresh --resend` is refused while a stray temporary file is there")
				break
			}
		}
		report.Namespaces = append(report.Namespaces, ns)
	}
	orphans, err := provider.Orphans(ctx, d.Paths, accounts, time.Now())
	if err != nil {
		return doctorReport{}, errs.NewConfig(fmt.Sprintf("the Codex tree could not be listed: %v", err))
	}
	for _, orphan := range orphans {
		entry := doctorOrphan{}
		valid := false
		switch orphan.Kind {
		case provider.OrphanNamespace, provider.OrphanRecord:
			valid = config.ValidateCodexSegment(orphan.User) == nil && config.ValidateCodexSegment(orphan.Account) == nil
			entry.Subject = orphan.User + "+" + orphan.Account
			if orphan.Kind == provider.OrphanNamespace {
				entry.Kind = "namespace without record"
			} else {
				entry.Kind = "record without " + provider.ShownName()
			}
		case provider.OrphanScratch:
			valid = doctorScratchName(orphan.Name)
			entry.Kind, entry.Subject = "stale scratch", orphan.Name
			age := usage.RenderCountdown(time.Now().Add(-orphan.Age), time.Now())
			entry.Age = &age
		}
		if valid {
			report.Orphans = append(report.Orphans, entry)
		} else {
			report.UnnameableOrphans++
		}
	}
	report.Audit = doctorAudit(ctx, d.Paths)
	return report, nil
}

func doctorSymlinks(dir string) []string {
	chain := []string{}
	prefix := ""
	if filepath.IsAbs(dir) {
		prefix = string(filepath.Separator)
	}
	for component := range strings.SplitSeq(strings.TrimPrefix(dir, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		prefix = filepath.Join(prefix, component)
		if info, err := os.Lstat(prefix); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(prefix); err == nil {
				chain = append(chain, prefix+" -> "+target)
			}
		}
	}
	return chain
}

func doctorStoreSection(ctx context.Context, home *string, listings doctorListings) doctorStore {
	store := doctorStore{Mode: "file", Read: "not read"}
	if home == nil {
		return store
	}
	cfg := provider.LoadConfig(ctx, *home)
	store.Mode, store.BaseURL = cfg.Store.Label(), cfg.BaseURL
	read, note := provider.FileInEffect(cfg.Store, func() provider.KeyringProbe { return listings.probe(*home) })
	if read {
		store.Read = "file"
		if note != "" {
			store.Read = note
		}
	}
	if cfg.Store == provider.StoreAuto {
		for _, entry := range listings.codexAuth {
			if entry.Service == provider.KeyringService && entry.Account == "" {
				store.CoarseMatch = true
				break
			}
		}
	}
	if cfg.Note != nil {
		note := "unparseable config.toml"
		if cfg.Note.Kind == "unreadable" {
			note = "config.toml could not be read"
		} else if cfg.Note.Line != nil {
			note = fmt.Sprintf("unparseable config.toml (line %d)", *cfg.Note.Line)
		}
		store.ConfigNote = &note
	}
	return store
}

func (d *Doctor) live(ctx context.Context, home *string, read string, accounts []config.CodexAccountRecord) doctorLive {
	live := doctorLive{State: "no home", Daemon: "none", MissingKnownMembers: []string{}}
	if home == nil {
		return live
	}
	switch provider.DaemonEvidence(ctx, *home).Kind {
	case provider.DaemonAlive:
		live.Daemon = "a daemon is running"
	case provider.DaemonRecycled:
		live.Daemon = "a recycled process id, not the daemon"
	case provider.DaemonArtefact:
		live.Daemon = "artefacts only"
	case provider.DaemonUnreadable:
		live.Daemon = "a daemon record cannot be read"
	}
	if read == "not read" {
		live.State = "not read"
		return live
	}
	path := filepath.Join(*home, provider.ShownName())
	if info, err := os.Lstat(path); err == nil {
		mode := uint32(info.Mode().Perm())
		if info.Mode()&os.ModeSetuid != 0 {
			mode |= 0o4000
		}
		if info.Mode()&os.ModeSetgid != 0 {
			mode |= 0o2000
		}
		if info.Mode()&os.ModeSticky != 0 {
			mode |= 0o1000
		}
		bits, size := fmt.Sprintf("%04o", mode), uint64(info.Size())
		live.ModeBits, live.Size = &bits, &size
		if mode != 0o600 {
			warning := fmt.Sprintf("warning: `%s` is mode %04o; a credential file should be 0600", path, mode)
			live.ModeWarning = &warning
		}
	}
	found := provider.ReadAuth(ctx, *home)
	switch found.Kind {
	case provider.ResolvedAbsent:
		live.State = "absent"
	case provider.ResolvedTorn:
		live.State = "torn"
	case provider.ResolvedTransient:
		live.State = "unusable"
	case provider.ResolvedCredentials:
		live.State = "credentials"
		credential := found.Credentials
		mode := credential.AuthMode().Label()
		live.AuthMode = &mode
		if at := credential.AccessExpiresAt(); at != nil {
			when := time.Unix(*at, 0)
			if when.Year() >= -9999 && when.Year() <= 9999 {
				relative := doctorRelative(when, time.Now())
				live.AccessExpiry = &relative
			}
		}
		if at := credential.LastRefresh(); at != nil {
			relative := doctorRelative(*at, time.Now())
			live.LastRefresh = &relative
		}
		live.MissingKnownMembers = credential.MissingKnownMembers()
		live.UnknownMemberCount = credential.UnknownMemberCount()
		if digest, ok := credential.RefreshDigest8(); ok {
			for i := range accounts {
				owned := provider.Owned(&accounts[i])
				if owned == nil {
					continue
				}
				dir, err := d.Paths.CodexNamespaceDir(owned.User(), owned.Account())
				if err != nil {
					continue
				}
				other := provider.ReadAuth(ctx, dir)
				if other.Kind == provider.ResolvedCredentials {
					if otherDigest, ok := other.Credentials.RefreshDigest8(); ok && otherDigest == digest {
						pair := owned.User() + "+" + owned.Account()
						live.MatchesNamespace = &pair
						break
					}
				}
			}
		}
	}
	return live
}

func doctorRelative(at, now time.Time) string {
	if !at.After(now) {
		return "expired " + usage.RenderCountdown(at, now) + " ago"
	}
	return "in " + usage.RenderCountdown(now, at)
}

func doctorForeignSection(home *string, listings doctorListings, caused []string) doctorForeign {
	foreign := doctorForeign{SwitcherItems: listings.switcher, CodexAuthItems: len(listings.codexAuth), UnexplainedRemovals: []string{}}
	expected := ""
	if home != nil {
		expected = provider.KeyringAccount(*home)
		if info, err := os.Lstat(filepath.Join(*home, "multi-auth")); err == nil {
			foreign.MultiAuthPresent = info.IsDir()
		}
	}
	for _, entry := range listings.codexAuth {
		if entry.Service != provider.KeyringService || entry.Account == "" || entry.Account == expected {
			continue
		}
		if !provider.IsHomeAccount(entry.Account) {
			foreign.UnnameableItems++
		} else if slices.Contains(caused, entry.Account) {
			foreign.UnexplainedRemovals = append(foreign.UnexplainedRemovals, fmt.Sprintf("security delete-generic-password -s \"Codex Auth\" -a \"%s\"", entry.Account))
		} else {
			foreign.UnexplainedItems++
		}
	}
	return foreign
}

func doctorArtifacts(dir string) ([]string, bool) {
	lines := []string{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return lines, false
	}
	if err != nil {
		reason := "other error"
		if errors.Is(err, os.ErrPermission) {
			reason = "permission denied"
		}
		return append(lines, "the namespace directory could not be listed: "+reason), false
	}
	present, stray, session := false, 0, 0
	name := provider.ShownName()
	for _, entry := range entries {
		switch {
		case entry.Name() == name:
			present = true
		case entry.Name() == name+".pending":
			lines = append(lines, "a parked pending write is there")
		case entry.Name() == "auth.pending.meta":
		case strings.HasPrefix(entry.Name(), name+".tmp."):
			stray++
		default:
			session++
		}
	}
	if stray > 0 {
		lines = append(lines, fmt.Sprintf("stray tmp (rotated grant?): %d file(s)", stray))
	}
	if session > 0 {
		lines = append(lines, fmt.Sprintf("codex session artefacts present: %d entry(s)", session))
	}
	slices.Sort(lines)
	return lines, present
}

func doctorLockSection(ctx context.Context, paths *config.Paths, user, account string, notes *[]string) *doctorLock {
	path, err := paths.CodexLockPath(user, account)
	if err != nil {
		return nil
	}
	body, ok := secret.ReadBody(path)
	if !ok {
		return nil
	}
	holder := "held"
	process, err := proc.Lookup(ctx, int(body.PID))
	switch {
	case errors.Is(err, proc.ErrProcessGone) || err == nil && process.Holder == proc.HolderDead:
		holder = "dead (holder gone)"
	case err != nil || body.PIDStartTime == nil || process.StartIdentity() == "":
		*notes = append(*notes, "namespace lock: unknown holder identity")
		return nil
	case process.StartIdentity() != *body.PIDStartTime:
		holder = "dead (pid recycled)"
	}
	return &doctorLock{PID: body.PID, AcquiredAt: body.AcquiredAt, Holder: holder}
}

func doctorMarkerSection(ctx context.Context, paths *config.Paths, user, account string) doctorMarker {
	marker := doctorMarker{State: "absent"}
	store, err := provider.NewRefreshStateStore(paths, user, account)
	if err != nil {
		reason := err.Error()
		marker.State, marker.Unavailable = "unavailable", &reason
		return marker
	}
	read := store.Load(ctx)
	switch read.Kind {
	case provider.RefreshStateUnavailable:
		marker.State, marker.Unavailable = "unavailable", &read.Reason
	case provider.RefreshStatePresent:
		state := read.State
		marker.State = "present"
		marker.FloorMin, marker.DidNotHelp, marker.Resent = &state.FloorMin, &state.DidNotHelp, &state.Resent
		now := time.Now()
		if state.Inflight != nil {
			marker.InflightDigest8 = &state.Inflight.SentDigest8
			age := usage.RenderCountdown(minTime(state.Inflight.SentAt, now), now)
			marker.InflightAge = &age
		}
		if state.Class != nil {
			class := state.Class.Label()
			marker.Class = &class
		}
		if state.AmbiguousSince != nil {
			age := usage.RenderCountdown(minTime(*state.AmbiguousSince, now), now)
			marker.AmbiguousSince = &age
			if state.Class != nil {
				eligible := !state.Resent && !provider.ResendEligibleAt(*state.AmbiguousSince, *state.Class, state.RetryAfter).After(now)
				marker.ResendEligible = &eligible
			}
		}
	}
	return marker
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func doctorScratchName(name string) bool {
	suffix, ok := strings.CutPrefix(name, provider.ScratchPrefix)
	if !ok || len(suffix) != 8 {
		return false
	}
	for _, b := range []byte(suffix) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func doctorAudit(ctx context.Context, paths *config.Paths) []string {
	text, present, err := provider.ReadCodexAudit(ctx, paths)
	if err != nil {
		return []string{fmt.Sprintf("the Codex write log could not be read: %v", err)}
	}
	lines := []string{}
	if !present {
		return lines
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	lines = lines[max(len(lines)-10, 0):]
	for i, line := range lines {
		shown, ok := provider.ShownCodexAuditLine(line)
		if !ok {
			shown = "a line agentctl did not write, or would not write; not shown"
		}
		lines[i] = shown
	}
	return lines
}
