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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/runtime/coordinator"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/usage"
)

const tornRetry = 50 * time.Millisecond

// RowPlan is one credential source and its command-thread refresh result.
type RowPlan struct {
	Index     int
	Source    codexprovider.Source
	LiveError string
	PrePass   *codexprovider.RefreshReport
	Retry     retryKind
}

type retryKind uint8

const (
	retryNone retryKind = iota
	retryAfterSend
	retryAfterAdopt
	retryVerify
)

func (p RowPlan) mayRefresh() bool {
	return p.Source.Record != nil && p.Source.Record.Kind.Owned != nil && p.Source.Record.Kind.Owned.Refresh == config.RefreshAuto
}

type rowPass struct {
	account      codexprovider.Account
	plan         RowPlan
	accessDigest string
	rejected     string
	floorRaised  bool
	fetched      bool
}

type passOptions struct {
	refresh   bool
	noCache   bool
	allowPost bool
}

type passShared struct {
	paths   *config.Paths
	client  *codexprovider.UsageClient
	listing keyringListing
	options passOptions
	signals *signals.Controller
}

type keyringListing struct {
	entries   []secret.ServiceEntry
	available bool
}

func (l keyringListing) probe(home string) codexprovider.KeyringProbe {
	if !l.available {
		return codexprovider.KeyringUnknown
	}
	account := codexprovider.KeyringAccount(home)
	for _, entry := range l.entries {
		if entry.Service == codexprovider.KeyringService && (entry.Account == "" || entry.Account == account) {
			return codexprovider.KeyringPresent
		}
	}
	return codexprovider.KeyringAbsent
}

func listKeyring(ctx context.Context, plans []RowPlan, reader secret.Reader) keyringListing {
	needed := false
	for _, plan := range plans {
		if (plan.Source.Kind == codexprovider.SourceLive || plan.Source.Kind == codexprovider.SourceHomeReadOnly) && codexprovider.LoadConfig(ctx, plan.Source.Home).Store == codexprovider.StoreAuto {
			needed = true
			break
		}
	}
	if !needed {
		return keyringListing{}
	}
	entries, err := reader.ListServices(ctx, codexprovider.KeyringService)
	if err != nil {
		slog.WarnContext(ctx, "the keychain listing for Codex homes failed", "error", err)
		return keyringListing{}
	}
	return keyringListing{entries: entries, available: true}
}

func planRows(ctx context.Context, paths *config.Paths, records []config.CodexAccountRecord, env codexprovider.Env) []RowPlan {
	sources, liveError := codexprovider.Discover(ctx, paths, records, env)
	plans := make([]RowPlan, 0, len(sources)+1)
	if liveError != nil {
		plans = append(plans, RowPlan{LiveError: liveError.Error()})
	}
	for _, source := range sources {
		if source.Kind == codexprovider.SourceLive {
			if _, err := os.Lstat(source.Home); errors.Is(err, fs.ErrNotExist) {
				continue
			}
		}
		plans = append(plans, RowPlan{Index: len(plans), Source: source})
	}
	return plans
}

func selectRows(plans []RowPlan, selectors []string) ([]RowPlan, error) {
	if len(selectors) == 0 {
		return plans, nil
	}
	matches := func(plan RowPlan, selector string) bool {
		record := plan.Source.Record
		if record == nil {
			return selector == "live"
		}
		return record.ChatGPTUserID == selector || record.ChatGPTUserID+"/"+record.ChatGPTAccountID == selector || record.Email != nil && *record.Email == selector
	}
	for _, selector := range selectors {
		if !slices.ContainsFunc(plans, func(plan RowPlan) bool { return matches(plan, selector) }) {
			known := make([]string, len(plans))
			for i, plan := range plans {
				known[i] = "live"
				if plan.Source.Record != nil {
					known[i] = plan.Source.Record.ChatGPTUserID + "/" + plan.Source.Record.ChatGPTAccountID
				}
			}
			return nil, errs.NewConfig(fmt.Sprintf("no Codex account matches `%s`; known accounts: %s", selector, strings.Join(known, ", ")))
		}
	}
	selected := make([]RowPlan, 0, len(plans))
	for _, plan := range plans {
		if slices.ContainsFunc(selectors, func(selector string) bool { return matches(plan, selector) }) {
			selected = append(selected, plan)
		}
	}
	return selected, nil
}

func collectRows(ctx context.Context, plans []RowPlan, shared *passShared, deadline time.Time) []rowPass {
	jobs := make([]coordinator.Job[rowPass], 0, len(plans))
	for _, plan := range plans {
		jobs = append(jobs, func(pass *coordinator.PassCtx) rowPass { return runRow(pass.Context(), plan, shared) })
	}
	passes := make([]rowPass, 0, len(plans))
	for pass := range coordinator.RunPass(ctx, shared.signals, jobs, deadline, coordinator.DefaultMaxWorkers) {
		passes = append(passes, pass)
	}
	slices.SortFunc(passes, func(a, b rowPass) int { return a.account.Index - b.account.Index })
	return passes
}

type rowRead struct {
	credentials *codexprovider.Credentials
	state       codexprovider.State
	note        *string
}

func readHome(ctx context.Context, home string, listing keyringListing) rowRead {
	configuration := codexprovider.LoadConfig(ctx, home)
	var note *string
	if configuration.Note != nil {
		note = new("config.toml could not be read; file store assumed")
	}
	read, modeNote := codexprovider.FileInEffect(configuration.Store, func() codexprovider.KeyringProbe { return listing.probe(home) })
	if !read {
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateStoreUnsupported, Mode: configuration.Store.Label()}, note: note}
	}
	pushNote(&note, modeNote)
	resolved := codexprovider.ReadAuth(ctx, home)
	if resolved.Kind == codexprovider.ResolvedTorn && waitTorn(ctx) {
		resolved = codexprovider.ReadAuth(ctx, home)
	}
	switch resolved.Kind {
	case codexprovider.ResolvedCredentials:
		return rowRead{credentials: resolved.Credentials, note: note}
	case codexprovider.ResolvedAbsent:
		pushNote(&note, "no credential in this Codex home")
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateNeedsLogin}, note: note}
	case codexprovider.ResolvedTorn:
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateTornRead}, note: note}
	default:
		return rowRead{state: codexprovider.State{Kind: codexprovider.StateError, Reason: resolved.Reason}, note: note}
	}
}

func waitTorn(ctx context.Context) bool {
	timer := time.NewTimer(tornRetry)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runRow(ctx context.Context, plan RowPlan, shared *passShared) rowPass {
	record := plan.Source.Record
	account := codexprovider.Account{Index: plan.Index, Kind: codexprovider.RowLive, State: codexprovider.State{Kind: codexprovider.StateOK}, LockState: "none", Visible: true}
	if record != nil {
		account.UserID, account.AccountID, account.Email, account.Plan = record.ChatGPTUserID, record.ChatGPTAccountID, record.Email, record.PlanType
		account.Kind = codexprovider.RowOwned
		if plan.Source.Kind == codexprovider.SourceHomeReadOnly {
			account.Kind = codexprovider.RowHomeReadOnly
		}
	}
	slog.DebugContext(ctx, "codex_account", "account.id", account.UserID, "kind", account.Kind.Name())
	pass := rowPass{account: account, plan: plan}
	if plan.PrePass != nil && plan.Retry == retryNone {
		pushReportNotes(&account.Note, plan.PrePass)
	}
	var read rowRead
	marker := markerView{}
	switch {
	case plan.LiveError != "":
		read.state = codexprovider.State{Kind: codexprovider.StateHomeUnreadable, Reason: plan.LiveError}
	case plan.Source.Kind == codexprovider.SourceInvalidOwned:
		read.state = codexprovider.State{Kind: codexprovider.StateError, Reason: "the record's ids cannot name a namespace"}
	case plan.Source.Kind == codexprovider.SourceOwned:
		if plan.Source.Evidence.Kind != codexprovider.DaemonNone {
			pushNote(&account.Note, "codex session detected ("+daemonLabel(plan.Source.Evidence)+")")
		}
		read, account.LockState, marker, pass.floorRaised = readOwned(ctx, plan.Source, shared.paths)
	default:
		read = readHome(ctx, plan.Source.Home, shared.listing)
	}
	pushOptionalNote(&account.Note, read.note)
	if read.credentials == nil {
		account.State = read.state
		pass.account = account
		return pass
	}
	credentials := read.credentials
	if identity := credentials.Identity(); identity != nil {
		account.UserID, account.AccountID = identity.UserID, identity.AccountID
		if identity.Email != nil {
			account.Email = identity.Email
		}
		if identity.Plan != nil {
			account.Plan = identity.Plan
		}
	}
	pass.accessDigest, _ = credentials.AccessDigest8()
	if !credentials.HasUsageSource() {
		account.State = codexprovider.State{Kind: codexprovider.StateNoUsageSource, Mode: credentials.AuthMode().Label()}
		pass.account = account
		return pass
	}
	now := time.Now()
	var kept *codexprovider.State
	switch marker.kind {
	case markerDead:
		account.State = codexprovider.State{Kind: codexprovider.StateNeedsLogin}
		pushNote(&account.Note, "the token host rejected this grant; run agentctl codex login")
		pass.account = account
		return pass
	case markerUnknown:
		state, note := unknownState(marker.since, marker.class, marker.resendAt, now)
		pushNote(&account.Note, note)
		if credentials.AccessExpired(now, 0) {
			account.State = codexprovider.State{Kind: codexprovider.StateNeedsLogin}
			pushNote(&account.Note, "refresh outcome unknown")
			pass.account = account
			return pass
		}
		kept = &state
	case markerUnavailable:
		kept = &codexprovider.State{Kind: codexprovider.StateRefreshUnavailable, Reason: marker.reason}
	}
	if kept == nil && credentials.AccessExpired(now, codexprovider.AccessRefreshMargin) {
		account.State, read.note = expiredState(account.Kind, record, shared.options.allowPost, plan.PrePass)
		pushOptionalNote(&account.Note, read.note)
		pass.account = account
		return pass
	}
	cachePath := ""
	if account.UserID != "" && account.AccountID != "" {
		cachePath = usage.CachePathIn(shared.paths.CodexCacheDir(), account.UserID, account.AccountID)
	}
	cached := usage.LoadCache(ctx, cachePath)
	cachedUsage := usageFromCache(cached)
	if !shared.options.refresh && !shared.options.noCache && plan.Retry == retryNone && cached != nil && cached.IsFresh(now.UnixMilli(), 300*time.Second) && cached.RateLimitedUntilMs == nil && cachedUsage != nil {
		account.State = usageState(cachedUsage)
		if kept != nil {
			account.State = *kept
		}
		applyUsage(&account, cachedUsage)
		pass.fetched = true
		pass.account = account
		return pass
	}
	if cached != nil {
		if seconds, limited := cached.RateLimitedFor(now.UnixMilli()); limited {
			account.State = codexprovider.State{Kind: codexprovider.StateRateLimited, RetryAfter: new(uint64(seconds))}
			applyUsage(&account, cachedUsage)
			pass.account = account
			return pass
		}
	}
	fetched, err := shared.client.Fetch(ctx, provider.AccountRef{ID: account.UserID, Auth: credentials})
	if err == nil {
		if cachePath != "" && fetched.Raw != nil {
			entry := usage.NewCacheEntry(fetched.FetchedAt.UnixMilli(), fetched.Raw)
			if err := usage.StoreCache(ctx, cachePath, &entry); err != nil {
				slog.WarnContext(ctx, "the Codex usage cache could not be written", "error", err)
			}
		}
		account.State = usageState(fetched)
		if kept != nil {
			account.State = *kept
			kept = nil
		}
		applyUsage(&account, fetched)
		pass.fetched = true
	} else {
		failure, ok := errors.AsType[*provider.FetchError](err)
		if !ok {
			account.State = codexprovider.State{Kind: codexprovider.StateError, Reason: err.Error()}
		} else {
			switch failure.Kind {
			case provider.FetchUnauthorized:
				account.State = codexprovider.State{Kind: codexprovider.StateUnauthorized, RefreshedRecently: plan.Retry == retryAfterSend}
				switch {
				case plan.Retry == retryVerify || plan.Retry == retryNone && plan.PrePass != nil && plan.PrePass.Step.Kind == codexprovider.RefreshStepRacedExternal:
					account.State.Kind = codexprovider.StateAdoptedDead
				case plan.Retry == retryNone && kept != nil && kept.Kind == codexprovider.StateRefreshUnknown:
					account.State.Kind = codexprovider.StateNeedsLogin
					kept = nil
					pushNote(&account.Note, "refresh outcome unknown")
				case plan.Retry == retryNone && plan.mayRefresh() && shared.options.allowPost && kept == nil:
					pass.rejected, _ = credentials.AccessDigest8()
				case plan.Retry == retryNone && plan.mayRefresh() && !shared.options.allowPost:
					pushNote(&account.Note, "run agentctl codex status")
				}
			case provider.FetchRateLimited:
				account.State.Kind = codexprovider.StateRateLimited
				if failure.HasRetryAfter {
					account.State.RetryAfter = new(uint64(failure.RetryAfter / time.Second))
					if cachePath != "" {
						storeRateLimit(ctx, cachePath, cached, now.UnixMilli(), failure.RetryAfter)
					}
				}
			case provider.FetchTransport, provider.FetchCancelled:
				account.State.Kind = codexprovider.StateStale
				pushNote(&account.Note, err.Error())
			default:
				account.State = codexprovider.State{Kind: codexprovider.StateError, Reason: err.Error()}
			}
		}
		applyUsage(&account, cachedUsage)
		if kept != nil {
			pushNote(&account.Note, account.State.Label())
			account.State = *kept
		}
	}
	pass.account = account
	return pass
}

func usageState(snapshot *codexprovider.Usage) codexprovider.State {
	kind := codexprovider.StateOK
	if !snapshot.HasRateLimit {
		kind = codexprovider.StateNoUsageWindows
	}
	return codexprovider.State{Kind: kind}
}

func usageFromCache(entry *usage.CacheEntry) *codexprovider.Usage {
	if entry == nil {
		return nil
	}
	snapshot, err := codexprovider.Normalize(entry.Body, time.UnixMilli(entry.FetchedAtMs), true)
	if err != nil {
		return nil
	}
	return snapshot
}

func storeRateLimit(ctx context.Context, path string, cached *usage.CacheEntry, now int64, wait time.Duration) {
	until := now + wait.Milliseconds()
	if until < now {
		return
	}
	entry := usage.NewCacheEntry(0, []byte("{}"))
	if cached != nil {
		entry = *cached
	}
	entry.RateLimitedUntilMs = &until
	if err := usage.StoreCache(ctx, path, &entry); err != nil {
		slog.WarnContext(ctx, "the Codex rate-limit window could not be cached", "error", err)
	}
}

func applyUsage(account *codexprovider.Account, snapshot *codexprovider.Usage) {
	if snapshot == nil {
		return
	}
	if snapshot.PlanType != nil {
		account.Plan = snapshot.PlanType
	}
	pushOptionalNote(&account.Note, snapshot.Note)
	account.Usage = snapshot
}

func finishRows(passes []rowPass) []codexprovider.Account {
	counts := make(map[string]int, len(passes))
	liveDigest, liveHome := "", ""
	for _, pass := range passes {
		counts[pass.account.UserID]++
		if pass.account.Kind == codexprovider.RowLive {
			liveDigest = pass.accessDigest
			liveHome, _ = filepath.EvalSymlinks(pass.plan.Source.Home)
		}
	}
	accounts := make([]codexprovider.Account, 0, len(passes))
	for _, pass := range passes {
		account := pass.account
		account.ID = account.UserID
		if account.ID == "" {
			account.ID = "live"
		} else if counts[account.UserID] > 1 {
			account.ID += "/" + account.AccountID
		}
		if pass.plan.Source.Kind == codexprovider.SourceHomeReadOnly {
			home, err := filepath.EvalSymlinks(pass.plan.Source.Home)
			sameHome := err == nil && liveHome != "" && home == liveHome
			record := pass.plan.Source.Record
			namesRecord := record != nil && account.UserID == record.ChatGPTUserID && account.AccountID == record.ChatGPTAccountID
			if sameHome && !namesRecord && account.UserID != "" {
				account.State.Kind = codexprovider.StateStaleSibling
				account.Usage = nil
				account.Visible = false
			} else if liveDigest != "" && pass.accessDigest == liveDigest {
				account.Visible = false
				pushNote(&account.Note, "same credential as live")
			}
		}
		accounts = append(accounts, account)
	}
	return accounts
}

func pushNote(note **string, more string) {
	if more == "" {
		return
	}
	if *note != nil && **note != "" {
		more = **note + "; " + more
	}
	*note = &more
}

func pushOptionalNote(note **string, more *string) {
	if more != nil {
		pushNote(note, *more)
	}
}
