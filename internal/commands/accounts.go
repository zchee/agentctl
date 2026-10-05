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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/runtime/proc"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/usage"
)

// AccountsHeadings is the column headings of `claude accounts list`, in
// order.
var AccountsHeadings = [7]string{"Id", "Account", "Org", "Kind", "Source", "State", "Location"}

// Accounts is the store one `claude accounts` invocation works on. The
// read-only subcommands run the same discovery pass `status` does, so a
// row here is the same row there, with the registry's own fields and the
// namespace's on-disk state spelled out rather than compressed into a
// table cell.
type Accounts struct {
	// Paths is where the store lives.
	Paths *config.Paths
	// Env is the environment discovery reads the live entry's identity
	// from.
	Env *claude.EnvView
	// Reader is the keychain transport.
	Reader secret.Reader
	// Out is where reports are written.
	Out io.Writer
}

// List runs `accounts list [--all]`.
//
// The same visibility rule and hidden-row footer as `status`, because
// they are the same rows: a stale sibling, a switcher item and a
// forgotten service are hidden here exactly as they are there, and all
// reveals them here exactly as it does there.
//
// It returns the registry's own error when the registry cannot be read.
func (a *Accounts) List(ctx context.Context, all bool) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	discovery := claude.Discover(ctx, registry, a.Paths, a.Reader, a.Env)
	return tell(a.Out, listTable(discovery.Rows, claude.ServiceName(a.Env), all))
}

// Show runs `accounts show <id>`.
//
// Resolved against the discovered rows rather than against the registry
// alone, so every row `status` prints can be inspected — including the
// live entry and an unclaimed keychain item, neither of which has a
// registry record.
//
// It returns a [errs.ConfigError] when id names no row or more than one.
func (a *Accounts) Show(ctx context.Context, id string) error {
	registry, err := config.LoadRegistry(ctx, a.Paths)
	if err != nil {
		return err
	}
	discovery := claude.Discover(ctx, registry, a.Paths, a.Reader, a.Env)
	row, err := resolveAccountRow(discovery.Rows, id)
	if err != nil {
		return err
	}
	return tell(a.Out, showReport(ctx, a.Paths, row, claude.ServiceName(a.Env)))
}

// showLabelWidth is the field the left-hand label of every `accounts
// show` line is laid out in, so the values align into one column.
const showLabelWidth = 19

// listTable renders the `accounts list` table over the discovered rows:
// the same visibility rule and hidden-row footer as the status table,
// because they are the same rows. A hidden row is counted, never shown,
// unless all reveals it.
func listTable(rows []claude.AccountRow, liveService string, all bool) string {
	var records [][]string
	hidden := 0
	for i := range rows {
		row := &rows[i]
		if !all && !row.VisibleByDefault {
			hidden++
			continue
		}
		records = append(records, listRecord(row, liveService))
	}
	out := render.Table(AccountsHeadings[:], records)
	if hidden > 0 {
		out += "\n" + render.Footer(hidden)
	}
	return out
}

// listRecord builds one row of the `accounts list` table.
func listRecord(row *claude.AccountRow, liveService string) []string {
	account := render.EmptyCell
	if row.Record.Email != nil {
		account = *row.Record.Email
	}
	org := row.Record.OrganizationUUID
	if row.Record.OrgName != nil {
		org = *row.Record.OrgName
	}
	return []string{
		row.ID,
		account,
		org,
		row.Record.Kind.Name(),
		row.Source.Name(),
		row.State.Label(),
		location(&row.Record, liveService),
	}
}

// location is where a row's credentials are named, in the terms of its
// kind.
//
// For an owned row this is the export spelling — the namespace directory
// as it was spelled at login, which is the string a Claude Code session
// pointed at this namespace would hash into a keychain service name. It
// is worth a column of its own precisely because a moved store keeps the
// old spelling, and that mismatch is what doctor reports.
func location(record *config.AccountRecord, liveService string) string {
	switch {
	case record.Kind.Owned != nil:
		return record.Kind.Owned.ExportSpelling
	case record.Kind.ConfigDirReadOnly != nil:
		return record.Kind.ConfigDirReadOnly.Service
	case record.Kind.Live:
		return liveService
	case record.Kind.Foreign != nil:
		return record.Kind.Foreign.Source
	default:
		return ""
	}
}

// resolveAccountRow resolves a user-supplied identifier against the
// discovered rows.
//
// The <account>/<organization> spelling is tried first and exactly,
// because it is what the ambiguity message tells the user to fall back to
// and so must never itself be ambiguous — which is also how a live row
// and an owned record for the same account are told apart (they differ in
// organization, or else they are one row).
//
// It returns a [errs.ConfigError] naming the unambiguous spellings when
// more than one row matches, and a plain "no account matches" when none
// does.
func resolveAccountRow(rows []claude.AccountRow, id string) (*claude.AccountRow, error) {
	for i := range rows {
		if keySpelling(&rows[i].Record) == id {
			return &rows[i], nil
		}
	}

	var matches []*claude.AccountRow
	for i := range rows {
		row := &rows[i]
		if row.ID == id || row.Record.AccountUUID == id || (row.Record.Email != nil && *row.Record.Email == id) || (row.Record.Label != nil && *row.Record.Label == id) {
			matches = append(matches, row)
		}
	}

	switch len(matches) {
	case 0:
		return nil, errs.NewConfig(fmt.Sprintf("no account matches `%s`; `agentctl claude accounts list --all` shows every row", id))
	case 1:
		return matches[0], nil
	default:
		candidates := make([]string, 0, len(matches))
		for _, row := range matches {
			candidates = append(candidates, keySpelling(&row.Record))
		}
		return nil, errs.NewConfig(fmt.Sprintf("`%s` matches %d rows; use one of: %s", id, len(matches), strings.Join(candidates, ", ")))
	}
}

// keySpelling is the <account>/<organization> spelling of one record.
func keySpelling(record *config.AccountRecord) string {
	return record.AccountUUID + "/" + record.OrganizationUUID
}

// showReport renders the `accounts show` report for one resolved row.
// When the row is one agentctl owns, the namespace's on-disk state is
// reported too: the directory, the lock file and its holder, and whether
// a pending or stray temporary file is sitting there.
//
// No token material is printed. The credentials value carries only the
// expiry timestamps, the scopes and the plan here (invariant: the token
// never reaches a display path).
func showReport(ctx context.Context, paths *config.Paths, row *claude.AccountRow, liveService string) string {
	record := &row.Record

	var service string
	if record.Kind.ConfigDirReadOnly != nil {
		service = record.Kind.ConfigDirReadOnly.Service
	}

	out := []string{
		showLine("id", row.ID),
		accountLine(record, service),
		showLine("organization", orDash(record.OrganizationUUID)),
		showLine("email", opt(record.Email)),
		showLine("org name", opt(record.OrgName)),
		showLine("label", opt(record.Label)),
		showLine("kind", record.Kind.Name()),
	}
	if service != "" {
		out = append(out, showLine("service", service))
	}
	out = append(out,
		showLine("source", row.Source.Name()),
		showLine("state", row.State.Label()),
		showLine("note", optString(row.Note)),
		showLine("forgotten", strconv.FormatBool(record.Forgotten)),
		showLine("created", orDash(record.CreatedAt)),
		showLine("location", location(record, liveService)),
	)

	if credentials := row.Credentials; credentials != nil {
		out = append(out,
			showLine("plan", opt(credentials.SubscriptionType)),
			showLine("scopes", strings.Join(credentials.Scopes, " ")),
			showLine("access expires", expiry(&credentials.ExpiresAtMillis)),
			showLine("refresh expires", expiry(credentials.RefreshTokenExpiresAtMillis)),
		)
	}

	if record.Kind.Owned != nil {
		out = append(out, namespaceReport(ctx, paths, record)...)
	}
	return strings.Join(out, "\n")
}

// showLine lays one label/value pair out with the values aligned into
// one column.
func showLine(label, value string) string {
	return fmt.Sprintf("%-*s%s", showLabelWidth, label, value)
}

// accountLine is the account line, which must never present a service
// name as a UUID.
//
// A read-only keychain item whose credential blob names nobody has no
// account UUID to be keyed by, so the import keys its record by the
// keychain service name instead. Printed under a bare account label,
// that string reads as an Anthropic account identifier — a user would
// copy it into --account expecting an account and get a keychain item —
// so this says what it actually is.
func accountLine(record *config.AccountRecord, service string) string {
	if service != "" && service == record.AccountUUID {
		return showLine("account", render.EmptyCell+" (the keychain item names no account, so this record is keyed by its service name)")
	}
	return showLine("account", orDash(record.AccountUUID))
}

// namespaceReport is the on-disk state of one owned namespace.
func namespaceReport(ctx context.Context, paths *config.Paths, record *config.AccountRecord) []string {
	nsDir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
	lockPath := paths.LockPath(record.AccountUUID, record.OrganizationUUID)

	out := []string{
		showLine("namespace", nsDir),
		showLine("credentials file", present(filepath.Join(nsDir, secret.CredentialsFile))),
		showLine("pending write", present(filepath.Join(nsDir, secret.PendingFile))),
		showLine("pending metadata", present(filepath.Join(nsDir, secret.PendingMetaFile))),
		showLine("lock file", lockPath),
	}

	if body, ok := secret.ReadBody(lockPath); ok {
		out = append(out, showLine("lock holder", fmt.Sprintf("pid %d (%s), taken %s", body.PID, holderLabel(ctx, body.PID), body.AcquiredAt)))
	} else {
		// A lock file with no readable body is the normal state once its
		// holder has gone: the body is left behind, but an unparseable
		// one means only that it was written by another build or caught
		// mid-write.
		out = append(out, showLine("lock holder", render.EmptyCell+" (no readable body)"))
	}

	stray, err := secret.ListStrayTmp(nsDir)
	switch {
	case err != nil:
		out = append(out, showLine("stray temporaries", "could not be listed: "+err.Error()))
	case len(stray) == 0:
		out = append(out, showLine("stray temporaries", "none"))
	default:
		out = append(out, showLine("stray temporaries", strings.Join(stray, ", ")))
	}
	return out
}

// holderLabel is what the recorded holder of a lock is doing now. A
// process that cannot be found is reported dead: for this display the
// difference between "gone" and "unobservable" changes nothing the
// reader can act on.
func holderLabel(ctx context.Context, pid uint32) string {
	process, err := proc.Lookup(ctx, int(pid))
	if err != nil {
		return proc.HolderDead.Label()
	}
	return process.Holder.Label()
}

// orDash returns value, or an em dash when it is empty.
func orDash(value string) string {
	if value == "" {
		return render.EmptyCell
	}
	return value
}

// opt returns the pointed-to string, or an em dash.
func opt(value *string) string {
	if value == nil {
		return render.EmptyCell
	}
	return *value
}

// optString returns value, or an em dash when it is empty: the display
// of an optional field whose absence is spelled "" rather than nil.
func optString(value string) string {
	if value == "" {
		return render.EmptyCell
	}
	return value
}

// present reports whether a path exists, without following a link to
// decide.
func present(path string) string {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return path + " (absent)"
	case info.Mode()&fs.ModeSymlink != 0:
		return path + " (a symbolic link — agentctl refuses to read it)"
	default:
		return path + " (present)"
	}
}

// expiry renders an expiry timestamp, with no token anywhere near it.
func expiry(millis *int64) string {
	if millis == nil {
		return render.EmptyCell + " (not recorded)"
	}
	at := time.UnixMilli(*millis).UTC()
	// A timestamp outside the years a calendar display can carry was
	// written by something broken; the raw number says more than a
	// confidently wrong date would.
	if at.Year() < 0 || at.Year() > 9999 {
		return fmt.Sprintf("%d (not a usable timestamp)", *millis)
	}
	rendered := at.Format("2006-01-02T15:04:05.999999999Z07:00")
	now := time.Now()
	if !at.After(now) {
		return rendered + " (expired)"
	}
	return rendered + " (in " + usage.RenderCountdown(now, at) + ")"
}

// tell writes one report and the newline that finishes it. Display
// writes share one error path: a closed stdout is reported, never
// panicked over.
func tell(w io.Writer, report string) error {
	if err := render.Print(w, report); err != nil {
		return errs.NewIO("could not write the report", err)
	}
	return nil
}
