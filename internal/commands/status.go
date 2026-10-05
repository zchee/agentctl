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

// The status pass: the command that produces the table.
//
// Its five stages are load and preflight, discover, fan out, normalize,
// render and exit. Three rules shape everything here:
//
//   - Refusing is the default. Rows the pass does not own — the live
//     credential, a foreign configuration directory — are never
//     refreshed and, when expired, are not even fetched. Their owner
//     refreshes them; this pass reports.
//   - A failed fetch shows the last good numbers. A rate limit, a
//     dropped connection or a keychain that went away renders the cached
//     value with the row marked rate-limited or stale, rather than a
//     blank cell. The exit status still reports the degradation, so a
//     script can tell.
//   - The exit status is about shown rows. The partial exit means "you
//     asked for numbers and at least one row you can see did not have
//     them". A row hidden by default cannot change the exit status,
//     because the user did not ask about it.

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/usage"
)

// Status is the status command with its collaborators injected, so a
// test drives whole passes against a temporary store and a local HTTP
// server.
type Status struct {
	// Reader is the keychain reader the pass discovers and reads items
	// through.
	Reader secret.Reader
	// Client is the usage endpoint client, already carrying the
	// per-request timeout.
	Client provider.UsageProvider[*usage.UsageSnapshot]
	// Refresher exchanges stored refresh grants.
	Refresher TokenRefresher
	// Profiles supplies missing plan metadata after a refresh, best effort.
	Profiles claude.ProfileSource
	// Writer persists refreshed keychain items after peer-lock admission.
	Writer KeychainWriter
	// Runner bounds the fan-out; nil means the bounded default.
	Runner PassRunner
	// Env is the environment view every naming decision of the pass
	// derives from, captured once so two halves of one pass cannot read
	// the environment differently.
	Env *claude.EnvView
	// Stdout is where the table or the JSON document goes; nil means
	// the process stdout.
	Stdout io.Writer
	// Now is the clock; nil means the system clock.
	Now func() time.Time
	// Zone is where the reset columns are printed; nil means the system
	// zone.
	Zone *time.Location
}

// statusOptions is the cache pair, both halves of which bypass it.
type statusOptions struct {
	// refresh asks for expired credentials to be refreshed even when a
	// cached value would do.
	refresh bool
	// noCache ignores the on-disk usage cache for this pass.
	noCache bool
	// listing is the pass-wide keychain listing used for refresh rechecks.
	listing []secret.ServiceEntry
}

// mayServeCache reports whether a cached value may be served instead of
// fetching.
func (o statusOptions) mayServeCache() bool {
	return !o.refresh && !o.noCache
}

// rowOutcome is one finished row.
type rowOutcome struct {
	// index is the row's position in discovery order, which is the order
	// the table puts it back into.
	index int
	// id is the identifier the account flag accepts, and the key the raw
	// member files this row's body under.
	id string
	// record is what the registry knows about the row.
	record config.AccountRecord
	// source is where the credentials behind this row came from.
	source claude.Source
	// account is the first column: the email when known, else the id.
	account string
	// org is the organization's display name, or its UUID when unnamed.
	org string
	// plan is the subscription tier, as the credential recorded it.
	plan string
	// state is what the pass concluded about this row.
	state claude.AccountState
	// lockState is what the namespace lock did on this pass; only the
	// JSON report shows it.
	lockState string
	// note is a short explanation appended to the state, when there is
	// one.
	note string
	// usage holds the numbers, when this row has any.
	usage *usage.UsageSnapshot
	// visibleByDefault reports whether the row appears without the
	// show-everything flag.
	visibleByDefault bool
	// sameIdentityAsLive reports whether an owned row names the same
	// account and organization pair as the live credential.
	sameIdentityAsLive bool
}

// Run runs the whole status command: resolve the store, discover,
// fan out, render, and map the shown rows onto the exit status.
//
// A partial error means a shown row is degraded — the table is still on
// stdout, and the error only sets the exit status. Anything else is
// fatal and means nothing was rendered.
func (s *Status) Run(ctx context.Context, globals cli.Globals, opts cli.ClaudeStatusOptions) error {
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return err
	}
	// The first run works at all because of this: the cache write later
	// would otherwise fail on a store that does not exist.
	if err := paths.EnsureDirs(ctx); err != nil {
		return err
	}
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = cli.HTTPTimeoutDefault
	}
	passCtx, cancel := context.WithTimeout(ctx, PassBudget(timeout))
	defer cancel()

	// The preflight and the listing happen once per pass, not once per
	// row: they are the same answer for every account, and asking a
	// dozen times would mean a dozen subprocesses.
	discovery := claude.Discover(passCtx, registry, paths, s.Reader, s.Env)
	slog.DebugContext(ctx, "discovery finished",
		slog.Int("rows", len(discovery.Rows)),
		slog.String("keychain", discovery.Preflight.State.String()),
		slog.Int("services", len(discovery.Listing)))

	selected, err := selectRows(discovery.Rows, opts.Accounts)
	if err != nil {
		return err
	}

	outcomes := s.collect(passCtx, paths, selected, statusOptions{refresh: opts.Refresh, noCache: opts.NoCache, listing: discovery.Listing})
	markSameIdentity(outcomes)

	failed := 0
	for i := range outcomes {
		if (opts.All || outcomes[i].visibleByDefault) && outcomes[i].state.IsFailure() {
			failed++
		}
	}

	report := render.Report{
		Rows:       statusRows(outcomes, opts.ByIdentity),
		Now:        s.now(),
		Zone:       s.zone(),
		ShowAll:    opts.All,
		ByIdentity: opts.ByIdentity,
	}

	// The JSON document replaces the table rather than accompanying it:
	// the document is the whole of stdout, so a caller can pipe it
	// straight into a parser. Log lines are on stderr already, which is
	// what makes that safe.
	if opts.JSON {
		document := jsonReport(outcomes, &report, opts.Raw)
		out, err := render.MarshalStatusReport(&document)
		if err != nil {
			return errs.NewConfig(fmt.Sprintf("the JSON report could not be serialized: %v", err))
		}
		if err := render.Print(s.stdout(), string(out)); err != nil {
			return errs.NewIO("write the JSON report", err)
		}
	} else {
		if err := render.Print(s.stdout(), render.Render(&report)); err != nil {
			return errs.NewIO("write the status table", err)
		}
		if opts.Raw {
			if err := s.printRaw(outcomes, opts.All); err != nil {
				return errs.NewIO("write the raw usage bodies", err)
			}
		}
	}

	return errs.PartialFromShown(failed)
}

// collect runs the fan-out and returns one outcome per selected row, in
// discovery order. Each worker owns its row — and its outcome slot —
// outright; nothing is shared but immutable collaborators, so a stuck
// account cannot block another.
func (s *Status) collect(ctx context.Context, paths *config.Paths, rows []claude.AccountRow, options statusOptions) []rowOutcome {
	outcomes := make([]rowOutcome, len(rows))
	completed := make([]bool, len(rows))
	jobs := make([]func(context.Context), 0, len(rows))
	for i := range rows {
		jobs = append(jobs, func(ctx context.Context) {
			outcomes[i] = s.runRow(ctx, i, rows[i], paths, options)
			completed[i] = true
		})
	}
	runner := s.Runner
	if runner == nil {
		runner = CoordinatedRunner{}
	}
	runner.Run(ctx, jobs)
	finished := outcomes[:0]
	for i, done := range completed {
		if done {
			finished = append(finished, outcomes[i])
		}
	}
	return finished
}

// runRow produces one row: cache, refresh barrier, fetch, normalize.
// The order of its checks is the invariant; reordering them would let a
// cached value hide a rate-limit window, or a fetch run for a row whose
// discovery state already forbade it.
func (s *Status) runRow(ctx context.Context, index int, row claude.AccountRow, paths *config.Paths, options statusOptions) rowOutcome {
	outcome := rowOutcome{
		index:            index,
		id:               row.ID,
		record:           row.Record,
		source:           row.Source,
		account:          row.ID,
		org:              row.Record.OrganizationUUID,
		state:            row.State,
		lockState:        "none",
		note:             row.Note,
		visibleByDefault: row.VisibleByDefault,
	}
	if row.Record.Email != nil {
		outcome.account = *row.Record.Email
	}
	if row.Record.OrgName != nil {
		outcome.org = *row.Record.OrgName
	}
	credentials := row.Credentials
	if credentials != nil && credentials.SubscriptionType != nil {
		outcome.plan = *credentials.SubscriptionType
	}

	nowMillis := s.now().UnixMilli()
	// Every row gets a cache entry, including one keyed by a keychain
	// service name: the cache names a file for any identifier at all, so
	// a row that cannot be refreshed is still not made to re-fetch on
	// every pass.
	cachePath := usage.CachePath(paths, row.Record.AccountUUID, row.Record.OrganizationUUID)
	entry := usage.LoadCache(ctx, cachePath)
	cachedUsage := func() *usage.UsageSnapshot {
		if entry == nil {
			return nil
		}
		snapshot, err := claude.ParseUsage(entry.Body, time.UnixMilli(entry.FetchedAtMs), true)
		if err != nil {
			return nil
		}
		return snapshot
	}

	owns := row.Record.Kind.Owned != nil
	var migrated *migratedRefreshTarget
	if outcome.state.Kind == claude.StateMigratedToKeychain {
		var refusal string
		migrated, refusal = migratedRefreshItem(paths, &row.Record, outcome.state.Service, options.listing)
		if migrated != nil {
			outcome.state = claude.StateOfOK()
			outcome.note = fmt.Sprintf("keychain service `%s`", migrated.service)
		} else {
			outcome.note, outcome.lockState = refusal, "migrated"
		}
	}
	// Pending decisions do not determine whether discovery allowed network I/O.
	networkAllowed := outcome.state.AllowsNetwork()
	refreshable := owns && outcome.state.Kind != claude.StateClaudeSessionDetected && outcome.state.Kind != claude.StateMigratedToKeychain
	if owns {
		pending := filepath.Join(paths.NamespaceDir(row.Record.AccountUUID, row.Record.OrganizationUUID), secret.PendingFile)
		if _, err := os.Lstat(pending); err == nil {
			result := s.underNamespaceLock(ctx, paths, &row.Record, options.listing, false)
			applyRefresh(&outcome, result)
			if result.credentials == nil {
				outcome.usage = cachedUsage()
				return outcome
			}
			credentials, networkAllowed = result.credentials, true
			if credentials.SubscriptionType != nil {
				outcome.plan = *credentials.SubscriptionType
			}
		}
	}
	refresh := func() refreshOutcome {
		if migrated != nil {
			return s.refreshMigrated(ctx, paths, migrated, credentials)
		}
		return s.refreshExpired(ctx, paths, &row.Record, options.listing)
	}

	// A server-imposed wait outlives the process that was told about it:
	// a second run inside the window must not call at all.
	if entry != nil {
		if seconds, limited := entry.RateLimitedFor(nowMillis); limited {
			outcome.state = claude.StateOfRateLimitedAfter(uint64(seconds))
			outcome.usage = cachedUsage()
			if outcome.usage != nil {
				outcome.note = "showing the cached value"
			}
			return outcome
		}
	}

	if credentials != nil && options.mayServeCache() && entry != nil && entry.IsFresh(nowMillis, usage.CacheTTL) {
		if snapshot, err := claude.ParseUsage(entry.Body, time.UnixMilli(entry.FetchedAtMs), true); err == nil {
			outcome.state = classifySnapshot(snapshot, outcome.state)
			outcome.usage = snapshot
			return outcome
		}
	}

	// Rows that cannot make a request at all: no credential, a locked
	// keychain, a hidden sibling. Discovery already said so; the pass
	// only has to not undo it.
	if !networkAllowed {
		return outcome
	}
	if credentials == nil {
		outcome.state = claude.StateOfNeedsLogin()
		return outcome
	}

	expired := credentials.AccessExpired(nowMillis, claude.RefreshMarginMillis)
	switch {
	case expired && refreshable:
		result := refresh()
		applyRefresh(&outcome, result)
		if result.credentials == nil {
			outcome.usage = cachedUsage()
			return outcome
		}
		credentials = result.credentials
		if credentials.SubscriptionType != nil {
			outcome.plan = *credentials.SubscriptionType
		}
	case expired:
		// Not ours to refresh: report it and spend no request on it. An
		// owned row that is expired and unrefreshable keeps the state
		// that made it unrefreshable.
		if !owns {
			outcome.state = claude.StateOfExpired(true)
		}
		outcome.usage = cachedUsage()
		return outcome
	}

	// The fetch, with at most one refresh barrier in the middle of it.
	carried := outcome.state
	refreshedOnce := false
	for {
		snapshot, err := s.Client.Fetch(ctx, provider.AccountRef{ID: row.ID, Auth: credentials})
		if err == nil {
			if snapshot.Raw != nil {
				s.storeCache(ctx, cachePath, usage.NewCacheEntry(snapshot.FetchedAt.UnixMilli(), snapshot.Raw))
			}
			outcome.state = mergeStates(carried, classifySnapshot(snapshot, claude.StateOfOK()))
			outcome.usage = snapshot
			return outcome
		}

		fetchErr, classified := errors.AsType[*provider.FetchError](err)
		if !classified {
			outcome.usage = cachedUsage()
			outcome.state = claude.StateOfError(err.Error())
			outcome.note = err.Error()
			return outcome
		}
		switch fetchErr.Kind {
		case provider.FetchUnauthorized:
			// The token expired between the expiry check and the request.
			// Routine: the two use different clocks.
			if refreshable && !refreshedOnce {
				refreshedOnce = true
				result := refresh()
				applyRefresh(&outcome, result)
				if result.credentials == nil {
					if outcome.state == carried {
						outcome.state = claude.StateOfNeedsLogin()
					}
					outcome.usage = cachedUsage()
					return outcome
				}
				credentials = result.credentials
				continue
			}
			outcome.state = claude.StateOfNeedsLogin()
			outcome.usage = cachedUsage()
			return outcome
		case provider.FetchRateLimited:
			// Persist the window so the next invocation also declines to
			// call, not merely the rest of this pass.
			if entry != nil && fetchErr.HasRetryAfter {
				persisted := *entry
				until := nowMillis + fetchErr.RetryAfter.Milliseconds()
				persisted.RateLimitedUntilMs = &until
				s.storeCache(ctx, cachePath, persisted)
			}
			if fetchErr.HasRetryAfter {
				outcome.state = claude.StateOfRateLimitedAfter(uint64(fetchErr.RetryAfter / time.Second))
			} else {
				outcome.state = claude.StateOfRateLimited()
			}
			outcome.usage = cachedUsage()
			if outcome.usage != nil {
				outcome.note = "showing the cached value"
			}
			return outcome
		default:
			// A cached value is only worth showing behind stale when
			// another pass could replace it. A permanent failure gets the
			// error, so the row says what to fix rather than what to wait
			// for.
			outcome.usage = cachedUsage()
			if fetchErr.IsTransient() && outcome.usage != nil {
				outcome.state = claude.StateOfStale()
			} else {
				outcome.state = claude.StateOfError(fetchErr.Error())
			}
			outcome.note = fetchErr.Error()
			return outcome
		}
	}
}

// applyRefresh folds one refresh-barrier outcome into the row.
func applyRefresh(outcome *rowOutcome, result refreshOutcome) {
	if result.lockState != "" {
		outcome.lockState = result.lockState
	}
	if result.note != "" {
		outcome.note = result.note
	}
	if result.state != nil {
		outcome.state = *result.state
	}
}

// storeCache writes a cache entry, reporting a failure as a log line and
// nothing more: a cache that cannot be written is not a reason to fail a
// run that already has its numbers.
func (s *Status) storeCache(ctx context.Context, path string, entry usage.CacheEntry) {
	if err := usage.StoreCache(ctx, path, &entry); err != nil {
		slog.DebugContext(ctx, "could not write the usage cache", slog.String("path", path), slog.Any("error", err))
	}
}

// classifySnapshot is the state a successfully parsed snapshot deserves.
// A response that described no window at all is what an API or console
// account looks like; saying ok next to an empty row would be a lie.
func classifySnapshot(snapshot *usage.UsageSnapshot, fallback claude.AccountState) claude.AccountState {
	if len(snapshot.Windows) == 0 {
		return claude.StateOfNoSubscriptionLimits()
	}
	return fallback
}

// mergeStates keeps the more informative of the state a row carried in
// and the state its fetch produced. A row that cleared its refresh
// barrier and then fetched cleanly should keep what the barrier said,
// not say ok: the barrier's answer is the thing the user might need to
// know about.
func mergeStates(carried, fetched claude.AccountState) claude.AccountState {
	if carried.Kind == claude.StateOK || fetched.Kind == claude.StateNoSubscriptionLimits {
		return fetched
	}
	return carried
}

// markSameIdentity marks every owned row whose identity is the live
// credential's.
//
// It happens here rather than per row because the answer is not a
// property of one account, it is a comparison between two rows, and no
// worker can see another's. Doing it once the pass has finished also
// costs nothing — both identities are already in hand, so there is no
// second keychain read and no token is compared or even touched. Only
// the two UUIDs are.
//
// A live row with no identity matches nothing: "we could not tell who
// is live" is not evidence that this account is.
func markSameIdentity(outcomes []rowOutcome) {
	type key struct{ acct, org string }
	var live []key
	for i := range outcomes {
		if outcomes[i].record.Kind.Live && outcomes[i].record.AccountUUID != "" {
			live = append(live, key{acct: outcomes[i].record.AccountUUID, org: outcomes[i].record.OrganizationUUID})
		}
	}
	if len(live) == 0 {
		return
	}
	for i := range outcomes {
		// Owned alone: a config-dir or foreign row is somebody else's
		// item, and a live row is not its own twin.
		if outcomes[i].record.Kind.Owned == nil {
			continue
		}
		outcomes[i].sameIdentityAsLive = slices.Contains(live, key{acct: outcomes[i].record.AccountUUID, org: outcomes[i].record.OrganizationUUID})
	}
}

// statusRows is the rows the table renders, with the identity view
// applied.
//
// Without the flag this is one row per credential source, unchanged.
// With it, an identity that has both a live credential and a store this
// binary owns renders once: the owned row survives — it is the one
// anything can be done to — its Kind cell reads live+owned, and the
// live row is dropped. A live row in a failing state is never folded
// away: the exit status is computed from the outcomes, not from these
// rows, so hiding a failing row would leave the run exiting partial
// with nothing on screen to explain it.
func statusRows(outcomes []rowOutcome, byIdentity bool) []render.StatusRow {
	if !byIdentity {
		rows := make([]render.StatusRow, 0, len(outcomes))
		for i := range outcomes {
			rows = append(rows, outcomes[i].statusRow())
		}
		return rows
	}

	type key struct{ acct, org string }
	var folded []key
	for i := range outcomes {
		if outcomes[i].sameIdentityAsLive {
			folded = append(folded, key{acct: outcomes[i].record.AccountUUID, org: outcomes[i].record.OrganizationUUID})
		}
	}

	var rows []render.StatusRow
	for i := range outcomes {
		outcome := &outcomes[i]
		identity := key{acct: outcome.record.AccountUUID, org: outcome.record.OrganizationUUID}
		if outcome.record.Kind.Live && !outcome.state.IsFailure() && slices.Contains(folded, identity) {
			continue
		}
		row := outcome.statusRow()
		if outcome.sameIdentityAsLive {
			row.Kind = render.LiveAndOwnedKind
			// The Kind cell now says what the note said, so the note
			// would only repeat it in the next column but one.
			row.SameIdentityAsLive = false
		}
		rows = append(rows, row)
	}
	return rows
}

// statusRow is the row as the table renders it.
func (o *rowOutcome) statusRow() render.StatusRow {
	return render.StatusRow{
		Account:            o.account,
		Org:                o.org,
		Plan:               o.plan,
		State:              o.state.Label(),
		Note:               o.note,
		Usage:              o.usage,
		VisibleByDefault:   o.visibleByDefault,
		SameIdentityAsLive: o.sameIdentityAsLive,
		Kind:               o.record.Kind.Name(),
	}
}

// jsonRow is the row as the JSON document publishes it.
func (o *rowOutcome) jsonRow() render.JSONRow {
	row := render.JSONRow{
		ID:               o.id,
		AccountUUID:      o.record.AccountUUID,
		OrganizationUUID: o.record.OrganizationUUID,
		Email:            o.record.Email,
		OrgName:          o.record.OrgName,
		Kind:             o.record.Kind.Name(),
		Source:           o.source.Name(),
		State:            o.state.Name(),
		StateLabel:       o.state.Label(),
		LockState:        o.lockState,
		Windows:          render.WindowsOf(o.usage),
		Credits:          render.CreditsOf(o.usage),
		NextReset:        render.NextResetOf(o.usage),
		SessionReset:     render.SessionResetOf(o.usage),
		WeeklyReset:      render.WeeklyResetOf(o.usage),
	}
	if o.note != "" {
		row.Note = &o.note
	}
	if o.sameIdentityAsLive {
		row.SameIdentityAs = new(render.SameIdentityLive)
	}
	// Read off the state rather than carried as a second field, so the
	// two cannot disagree: the occupant is only knowable when the row is
	// adopted, and it is exactly what that state holds.
	if o.state.Kind == claude.StateAdopted {
		row.OccupiedBy = &o.state.Occupant
	}
	return row
}

// jsonReport builds the version 1 document from a finished pass. The
// rows are the ones the table would have shown, in the same order, so
// the two renderings of one pass never disagree about what exists;
// hidden is the same count the table's footer prints.
func jsonReport(outcomes []rowOutcome, report *render.Report, raw bool) render.StatusReport {
	document := render.NewStatusReport(report.Now, report.HiddenCount())
	var bodies render.RawBodies
	for i := range outcomes {
		outcome := &outcomes[i]
		if !report.ShowAll && !outcome.visibleByDefault {
			continue
		}
		document.Rows = append(document.Rows, outcome.jsonRow())
		if raw && outcome.usage != nil && outcome.usage.Raw != nil {
			bodies.Add(outcome.id, outcome.usage.Raw)
		}
	}
	if raw {
		document.Raw = &bodies
	}
	return document
}

// printRaw prints each shown row's untouched response body after the
// table. Separate from the table because a usage body is a couple of
// kilobytes of JSON and a table cell is not where anyone would read it.
// The bodies carry usage figures and no token material.
func (s *Status) printRaw(outcomes []rowOutcome, showAll bool) error {
	for i := range outcomes {
		outcome := &outcomes[i]
		if !showAll && !outcome.visibleByDefault {
			continue
		}
		if outcome.usage == nil || outcome.usage.Raw == nil {
			continue
		}
		body := outcome.usage.Raw.Clone()
		if err := body.Indent(); err != nil {
			slog.Warn("the raw body could not be re-serialized", slog.Any("error", err))
			continue
		}
		if err := render.Print(s.stdout(), fmt.Sprintf("\n--- raw: %s ---\n%s", outcome.account, body)); err != nil {
			return err
		}
	}
	return nil
}

// selectRows narrows the discovered rows to the ones the account flag
// asked for. A selector that matches nothing is an error naming what was
// available — silently rendering an empty table would look like "you
// have no accounts" rather than "that is not one of them".
func selectRows(rows []claude.AccountRow, selectors []string) ([]claude.AccountRow, error) {
	if len(selectors) == 0 {
		return rows, nil
	}
	for _, selector := range selectors {
		if !slices.ContainsFunc(rows, func(row claude.AccountRow) bool { return matchesSelector(&row, selector) }) {
			known := make([]string, 0, len(rows))
			for i := range rows {
				known = append(known, rows[i].ID)
			}
			return nil, errs.NewConfig(fmt.Sprintf("no account matches `%s`; known accounts: %s", selector, strings.Join(known, ", ")))
		}
	}
	var selected []claude.AccountRow
	for i := range rows {
		if slices.ContainsFunc(selectors, func(selector string) bool { return matchesSelector(&rows[i], selector) }) {
			selected = append(selected, rows[i])
		}
	}
	return selected, nil
}

// matchesSelector reports whether one account selector names this row:
// the row id, the account UUID, the email address, or the
// account/organization pair.
func matchesSelector(row *claude.AccountRow, selector string) bool {
	record := &row.Record
	return row.ID == selector ||
		record.AccountUUID == selector ||
		(record.Email != nil && *record.Email == selector) ||
		record.AccountUUID+"/"+record.OrganizationUUID == selector
}

// stdout is the destination stream, defaulting to the process stdout.
func (s *Status) stdout() io.Writer {
	if s.Stdout != nil {
		return s.Stdout
	}
	return os.Stdout
}

// now is the clock, defaulting to the system one.
func (s *Status) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// zone is the rendering zone, defaulting to the system one.
func (s *Status) zone() *time.Location {
	if s.Zone != nil {
		return s.Zone
	}
	return time.Local
}
