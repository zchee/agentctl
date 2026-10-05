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
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/proc"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/usage"
)

// Doctor examines the complete store without refreshing credentials.
type Doctor struct {
	Paths  *config.Paths
	Env    *claude.EnvView
	Reader secret.Reader
	Out    io.Writer
	// SampleInterval is the heartbeat observation window. Zero uses the production interval.
	SampleInterval time.Duration
}

// DoctorStaleMinAge is the youngest lock eligible for explicit removal.
const DoctorStaleMinAge = 60 * time.Second

// DoctorSampleInterval spans at least two missed holder heartbeats.
const DoctorSampleInterval = 12 * time.Second

func (d *Doctor) interval() time.Duration {
	if d.SampleInterval > 0 {
		return d.SampleInterval
	}
	return DoctorSampleInterval
}

// Report prints every diagnostic section. Only an unreadable registry or output failure aborts the report.
func (d *Doctor) Report(ctx context.Context) error {
	registry, err := config.LoadRegistry(ctx, d.Paths)
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, secret.NamespaceLockWait)
	found := claude.Discover(readCtx, registry, d.Paths, d.Reader, d.Env)
	cancel()
	out := []string{
		"store",
		fmt.Sprintf("  config dir       %s", d.Paths.ConfigDir()),
		fmt.Sprintf("  namespace root   %s", d.Paths.NamespaceRoot()),
		"  registry         " + doctorStateOf(d.Paths.ConfigFile()),
		fmt.Sprintf("  audit log        %s (%s)", secret.AuditLogPath(d.Paths), secret.AuditLogStateOf(d.Paths).Note()),
		d.configRow(),
		fmt.Sprintf("  accounts         %d recorded", len(registry.Accounts)),
		"", "keychain", "  preflight        " + doctorPreflight(found.Preflight),
		"  live service     " + claude.ServiceName(d.Env),
		fmt.Sprintf("  services listed  %d", len(found.Listing)), "", "accounts",
	}
	forgotten := 0
	for i := range found.Rows {
		row := &found.Rows[i]
		if row.State.Kind == claude.StateForgotten {
			forgotten++
			continue
		}
		email := "(no email)"
		if row.Record.Email != nil {
			email = *row.Record.Email
		}
		out = append(out, fmt.Sprintf("  %s  %s  kind=%s source=%s state=%s", row.ID, email, row.Record.Kind.Name(), row.Source.Name(), row.State.Label()))
		if c := row.Credentials; c != nil {
			refresh := "not recorded"
			now := time.Now()
			if c.RefreshTokenExpiresAtMillis != nil {
				refresh = doctorRelative(*c.RefreshTokenExpiresAtMillis, now)
			}
			out = append(out, fmt.Sprintf("      access %s, refresh %s", doctorRelative(c.ExpiresAtMillis, now), refresh))
		}
	}
	if forgotten > 0 {
		out = append(out, fmt.Sprintf("  %d forgotten service(s) hidden; `accounts list --all` shows them", forgotten))
	}
	out = append(out, "")
	out = append(out, d.foreignSection(&found)...)
	out = append(out, "")
	out = append(out, d.lockSection(ctx)...)
	out = append(out, "")
	out = append(out, d.heldLocksSection(ctx)...)
	out = append(out, "")
	namespaces, err := d.namespaceSection(ctx, registry)
	if err != nil {
		return err
	}
	out = append(out, namespaces...)
	out = append(out, "")
	out = append(out, d.attentionSection(registry, &found)...)
	out = append(out, "")
	isolation := d.collectIsolation(registry, found.Listing)
	out = append(out, doctorIsolationSection(&isolation, d.Paths.SessionRoot())...)
	return tell(d.Out, strings.Join(out, "\n"))
}

func doctorStateOf(path string) string {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return path + " (absent)"
	case info.Mode()&os.ModeSymlink != 0:
		return path + " (a symbolic link — refused)"
	case info.Mode().IsRegular():
		return path + " (present)"
	default:
		return path + " (present, but not a regular file)"
	}
}

func doctorPreflight(status secret.KeychainStatus) string {
	switch status.State {
	case secret.KeychainStateUnlocked:
		return "unlocked"
	case secret.KeychainStateLocked:
		return "locked — unlock the login keychain and run this again"
	case secret.KeychainStateTimeout:
		return "timed out — `security(1)` did not answer inside its budget"
	case secret.KeychainStateUnavailable:
		return "unavailable (" + status.Reason + ")"
	default:
		return "unsupported on this platform"
	}
}

func doctorRelative(millis int64, now time.Time) string {
	at := time.UnixMilli(millis)
	if at.Year() < -9999 || at.Year() > 9999 {
		return fmt.Sprintf("%d (not a usable timestamp)", millis)
	}
	if !at.After(now) {
		return "expired " + usage.RenderCountdown(at, now) + " ago"
	}
	return "in " + usage.RenderCountdown(now, at)
}

func (d *Doctor) foreignSection(found *claude.Discovery) []string {
	out := []string{"foreign items (never read)"}
	for _, entry := range found.Listing {
		if strings.HasPrefix(entry.Service, claude.SwitcherServicePrefix) {
			out = append(out, "  "+entry.Service+"  belongs to claude-switcher; agentctl never reads or writes it")
		}
	}
	if d.Env.OAuthTokenSet {
		out = append(out, "  "+claude.OAuthTokenEnv+"  is set in the environment and short-circuits every credential store; agentctl reports it and never reads its value")
	}
	if len(out) == 1 {
		out = append(out, "  none")
	}
	return out
}

func (d *Doctor) configRow() string {
	path := claude.GlobalConfigPath(d.Env)
	lock := path
	if name, ok := claude.ConfigLockName(path); ok {
		lock = filepath.Join(filepath.Dir(path), name)
	}
	tail, err := secret.TailAuditLog(d.Paths, math.MaxInt)
	verdict := ""
	if err != nil {
		verdict = fmt.Sprintf("config writes unknown (%s)", doctorEscape(err.Error()))
	} else {
		verdict = doctorConfigVerdict(&tail, path)
	}
	return fmt.Sprintf("  claude config    %s (lock %s); %s", path, lock, verdict)
}

func doctorEscape(value string) string {
	return strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%q", value), "\""), "\"")
}

func doctorConfigVerdict(tail *secret.AuditTail, path string) string {
	for i := len(tail.Entries) - 1; i >= 0; i-- {
		entry := &tail.Entries[i]
		switch event := entry.Event.(type) {
		case *secret.WriteEvent:
			if event.Target == secret.TargetLive && (event.Outcome == secret.WriteApplied || event.Outcome == secret.WriteUnknown) {
				return fmt.Sprintf("the newest live write (%s) has no config write after it", entry.ID())
			}
		case *secret.ConfigWriteRecord:
			reason, after := "", ""
			if event.Reason != nil {
				reason = " (" + string(*event.Reason) + ")"
			}
			if event.After != nil {
				after = " after " + doctorEscape(*event.After)
			}
			return fmt.Sprintf("last config write %s%s at %s%s; %s", event.Outcome, reason, entry.ID(), after, doctorConfigAgreement(path, event.Account))
		}
	}
	return "no config write recorded"
}

func doctorConfigAgreement(path string, recorded *secret.IncomingIdentity) string {
	if recorded == nil {
		return "it recorded no account"
	}
	file, err := secret.ReadFileFollowing(path, claude.MaxClaudeJSONBytes)
	if err != nil {
		return "the file cannot be read"
	}
	defer clear(file.Bytes)
	if !file.Present {
		return "the file is absent"
	}
	value, err := secret.NewSecret(file.Bytes)
	if err != nil {
		return "the file cannot be read"
	}
	verdict := "the file cannot be read"
	_ = value.WithPlaintext(func(data []byte) error {
		var identity struct {
			Account struct {
				UUID             *string `json:"accountUuid"`
				OrganizationUUID *string `json:"organizationUuid"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(data, &identity) != nil {
			return nil
		}
		verdict = "the file names no account"
		if identity.Account.UUID == nil {
			return nil
		}
		verdict = "its account differs from the file"
		if *identity.Account.UUID == recorded.AccountUUID && (recorded.OrganizationUUID == nil || (identity.Account.OrganizationUUID != nil && *recorded.OrganizationUUID == *identity.Account.OrganizationUUID)) {
			verdict = "its account agrees with the file"
		}
		return nil
	})
	return verdict
}

func doctorRecordedHolder(ctx context.Context, pid uint32, started *string) string {
	if started != nil && proc.WriterGone(ctx, int(pid), *started) {
		return proc.HolderDead.Label()
	}
	return holderLabel(ctx, pid)
}

func (d *Doctor) lockSection(ctx context.Context) []string {
	out := []string{"namespace locks"}
	entries, err := os.ReadDir(d.Paths.LocksDir())
	if err != nil {
		return append(out, "  "+d.Paths.LocksDir()+" is not readable")
	}
	if len(entries) == 0 {
		return append(out, "  none")
	}
	for _, entry := range entries {
		body, ok := secret.ReadBody(filepath.Join(d.Paths.LocksDir(), entry.Name()))
		if !ok {
			out = append(out, "  "+entry.Name()+"  no readable body (never held, or held by an older build)")
			continue
		}
		out = append(out, fmt.Sprintf("  %s  pid %d (%s), taken %s", entry.Name(), body.PID, doctorRecordedHolder(ctx, body.PID, body.PIDStartTime), body.AcquiredAt))
	}
	return out
}

func (d *Doctor) heldLocksSection(ctx context.Context) []string {
	out := []string{"held locks"}
	records := secret.ReadAllHeldLockRecords(d.Paths)
	if len(records) == 0 {
		return append(out, "  none")
	}
	for _, held := range records {
		record := &held.Record
		state := doctorRecordedHolder(ctx, record.WriterPID, record.WriterStartTime)
		if DoctorCanRemoveStale {
			state = holderLabel(ctx, record.WriterPID)
		}
		out = append(out, fmt.Sprintf("  %s  pid %d (%s), %s, taken %s", held.File, record.WriterPID, state, record.Tree.Label(), record.TakenAt))
		present := 0
		gone := record.WriterIsGone(ctx)
		for _, path := range record.Paths {
			if _, err := os.Lstat(path); err != nil {
				continue
			}
			present++
			if filepath.Base(path) == secret.LegacyStorageWriteArtefact {
				out = append(out, "    "+doctorLegacyNotice(path))
			} else if !DoctorCanRemoveStale {
				writer := "writer not proved gone"
				if gone {
					writer = "writer gone"
				}
				out = append(out, fmt.Sprintf("    %s  %s; %s", path, writer, DoctorStaleRemovalUnsupported))
			} else if gone {
				out = append(out, fmt.Sprintf("    %s  leaked — `doctor --remove-stale %s --yes` removes it", path, path))
			} else {
				out = append(out, fmt.Sprintf("    %s  held by pid %d", path, record.WriterPID))
			}
		}
		if present == 0 {
			out = append(out, "    the directories it names are gone: a stale record, holding nothing")
		}
	}
	return out
}

func (d *Doctor) attentionSection(registry *config.Registry, found *claude.Discovery) []string {
	out := []string{"worth knowing"}
	byDigest := make(map[string][]string)
	for i := range found.Rows {
		row := &found.Rows[i]
		if row.State.Kind == claude.StateStaleSiblingOfLive {
			out = append(out, "  "+row.ID+"  names the same directory as the live credential but holds different tokens; hidden by default, never merged")
		}
		if row.Credentials != nil {
			if digests, err := row.Credentials.Digests(); err == nil {
				byDigest[digests.AccessSHA256] = append(byDigest[digests.AccessSHA256], row.ID)
			}
		}
	}
	hasService := func(service string) bool {
		return slices.ContainsFunc(found.Listing, func(entry secret.ServiceEntry) bool { return entry.Service == service })
	}
	for _, service := range registry.ForgottenServices {
		if !hasService(service) {
			out = append(out, "  "+service+"  is on the forgotten list but is not in the keychain any more; `accounts unforget` clears the entry")
		}
	}
	digests := make([]string, 0, len(byDigest))
	for digest := range byDigest {
		digests = append(digests, digest)
	}
	slices.Sort(digests)
	for _, digest := range digests {
		if ids := byDigest[digest]; len(ids) > 1 {
			out = append(out, fmt.Sprintf("  %s  hold the same access token (digest %s…): one refresh rotates it out from under the others", strings.Join(ids, ", "), digest[:8]))
		}
	}
	for i := range registry.Accounts {
		record := &registry.Accounts[i]
		owned := record.Kind.Owned
		if owned == nil {
			continue
		}
		if record.OrganizationUUID == config.UnknownOrg {
			out = append(out, fmt.Sprintf("  %s/%s  the login could not name an organization; `accounts relocate %s` moves it once one is known", record.AccountUUID, config.UnknownOrg, record.AccountUUID))
		}
		current := claude.ExportSpelling(d.Paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID))
		if owned.ExportSpelling != current {
			out = append(out, fmt.Sprintf("  %s/%s  was created as `%s` but this store spells it `%s`: the store moved, or another agentctl store holds the same account", record.AccountUUID, record.OrganizationUUID, owned.ExportSpelling, current))
		}
		migrated := claude.LiveService + "-" + owned.ExportSHA8
		if hasService(migrated) {
			out = append(out, "  "+migrated+"  a keychain item exists for this namespace: a Claude Code session has migrated it, and agentctl will not write the file again")
		}
	}
	if len(out) == 1 {
		out = append(out, "  nothing")
	}
	return out
}
