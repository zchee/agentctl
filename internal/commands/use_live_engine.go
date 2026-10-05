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
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/fault"
	"github.com/zchee/agentctl/internal/secret"
)

const (
	useSwapDeadline = 120 * time.Second
	useBackendNote  = "agentctl inspected its own environment for a secure-storage backend and found none; it cannot inspect the target session's"
)

type useLiveSwap struct {
	paths     *config.Paths
	config    *config.Registry
	env       *claude.EnvView
	live      bool
	inherited string
	process   SessionProcess
}

type useSource struct {
	kind useSourceKind
	dir  string
}

type useUndoneEntry struct {
	installed claude.Identity
}

type useIncoming struct {
	record    *config.AccountRecord
	direction secret.WriteDirection
	source    useSource
	undone    *useUndoneEntry
}

func (swap useLiveSwap) swapIn(ctx context.Context, incoming useIncoming, store *config.AccountRecord, opts cli.ClaudeUseOptions) *useReport {
	ctx, cancel := context.WithTimeout(ctx, useSwapDeadline)
	defer cancel()
	var target *string
	var warnings []string
	report := swap.swapPhases(ctx, incoming, store, opts, &target, &warnings)
	report.target = target
	report.warnings = append(warnings, report.warnings...)
	return report
}

func (swap useLiveSwap) swapPhases(ctx context.Context, incoming useIncoming, store *config.AccountRecord, opts cli.ClaudeUseOptions, target **string, warnings *[]string) *useReport {
	subject, refused := useBuildSubject(swap.paths, swap.env, swap.live, store, swap.inherited)
	if refused != nil {
		return refused
	}
	*target = new(string(subject.audit))
	service := subject.service
	if swap.env.OAuthTokenSet {
		return useRefused(claude.SwapRefusal{Kind: claude.SwapEnvToken}, service, "`CLAUDE_CODE_OAUTH_TOKEN` is set in agentctl's own environment, which short-circuits every credential store; unset it and run this again")
	}
	swap.noteUse(ctx, warnings, useBackendNote)
	var hints useSessionHints
	if swap.live {
		hints = useScanSessions(ctx, claude.SessionsDir(swap.env))
		if hints.unreadable {
			swap.noteUse(ctx, warnings, "agentctl could not read Claude Code's session registry, so it cannot say whether a running session has Remote Control on. A session that does keeps its claude.ai history only if Remote Control is disconnected there before the swap: decline this swap (answer n, or run without `--yes`), disconnect it there, and run this command again")
		}
	}
	item, err := useReadKeychain(ctx, secret.NewReader(), service)
	if err != nil {
		return useCannotAdopt(claude.AdoptionUnreadable, service)
	}
	itemPresent := item != nil
	displaced := item
	if !itemPresent {
		if swap.live {
			return useRefused(claude.SwapRefusal{Kind: claude.SwapLiveItemAbsent}, service, fmt.Sprintf("the live store `%s` has not migrated into the keychain, so there is no `%s` item to swap and its credential is still in the plaintext store; run `claude` once to migrate it, then run this again", subject.storeDir, service))
		}
		displaced, err = useReadStored(subject.storeDir, useSourceOwn)
		if err != nil {
			return useCannotAdopt(claude.AdoptionUnreadable, service)
		}
	}
	var before *claude.Digests
	var from8 *string
	if item != nil {
		digest, err := item.Digests()
		if err != nil {
			return useCannotAdopt(claude.AdoptionUnreadable, service)
		}
		before = &digest
		if short, ok := secret.Digest8(digest.AccessSHA256); ok {
			from8 = &short
		}
	}
	var identity *claude.Identity
	var profiles claude.ProfileSource
	var itemProfile *claude.Profile
	if displaced != nil {
		identity = displaced.Identity()
		if swap.live {
			client, err := claude.NewOAuthClientFromEnv()
			if err != nil {
				return (&useIdentityFailure{kind: claude.SwapProfileUnavailable, detail: err.Error()}).report(service)
			}
			profiles = client
			short := ""
			if from8 != nil {
				short = *from8
			}
			var failure *useIdentityFailure
			identity, itemProfile, failure = useIdentify(ctx, profiles, displaced, short, func() (secret.AuditTail, error) { return secret.TailAuditLog(swap.paths, math.MaxInt) }, time.Now().UnixMilli())
			if failure != nil {
				return failure.report(service)
			}
		}
	}
	catchUp := func(report *useReport) *useReport {
		if !swap.live {
			return report
		}
		return swap.process.catchUpUse(ctx, useCatchUp{paths: swap.paths, env: swap.env, record: incoming.record, profile: itemProfile, direction: incoming.direction, opts: opts, hints: hints}, report)
	}
	if incoming.undone != nil && identity != nil && !claude.IdentitiesAgree(identity, &incoming.undone.installed) {
		if claude.IdentityIs(identity, incoming.record) {
			return catchUp(useAlreadyActive(service, from8, from8, fmt.Sprintf("`%s`'s credential is already the one the live store holds: the session has logged in as that account since the swap, so there is nothing to put back; this swap is already reversed, and later namespace swaps become undoable after the next live swap", useDisplayIdentity(identity))))
		}
		return useRefused(claude.SwapRefusal{Kind: claude.SwapLiveUndoForeignLogin}, service, fmt.Sprintf("the live session has logged in as `%s` since that swap, which is neither the account being put back nor the one the swap installed; run `agentctl claude use --live` to adopt it, or resolve it by hand", useDisplayIdentity(identity)))
	}
	dir := incoming.source.dir
	if incoming.source.kind == useSourceOwn {
		dir = swap.paths.NamespaceDir(incoming.record.AccountUUID, incoming.record.OrganizationUUID)
	}
	credentials, err := useReadStored(dir, incoming.source.kind)
	if err != nil || credentials == nil {
		note := "the adopted copy that swap displaced is gone or unreadable, so there is nothing to put back"
		if incoming.source.kind == useSourceOwn {
			who := incoming.record.AccountUUID
			if incoming.record.Email != nil {
				who = *incoming.record.Email
			}
			note = fmt.Sprintf("`%s` has no readable credential to swap in; run `agentctl claude login` for it", who)
			if err == nil && useMigrated(ctx, swap.paths, incoming.record) {
				note = fmt.Sprintf("`%s`'s credential lives in its keychain item; swapping it in is not supported yet", who)
			}
		}
		return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionUnreadable}, "", note)
	}
	derived, err := credentials.Digests()
	if err != nil {
		return useCannotAdopt(claude.AdoptionUnreadable, service)
	}
	planned, _ := secret.Digest8(derived.AccessSHA256)
	if displaced != nil {
		current, err := displaced.Digests()
		if err != nil {
			return useCannotAdopt(claude.AdoptionUnreadable, service)
		}
		if current == derived {
			return catchUp(useAlreadyActive(service, from8, &planned, "that account's credential is already the one this store holds"))
		}
		if swap.live && incoming.direction == secret.DirectionForward && claude.IdentityIs(identity, incoming.record) && displaced.ExpiresAtMillis >= credentials.ExpiresAtMillis {
			return catchUp(useAlreadyActive(service, from8, from8, "that account's credential is already the one the live store holds"))
		}
	}
	account := secret.CurrentAccount()
	if _, err := credentials.ToKeychainStdinLine(account, service); err != nil {
		if _, tooLong := errors.AsType[*secret.LineTooLongError](err); tooLong {
			return useLineTooLong(service)
		}
	}
	var third *config.AccountRecord
	var ambiguous *[2]config.AccountRecord
	if displaced != nil && (swap.live || incoming.direction == secret.DirectionForward) {
		third, ambiguous = useThirdNamespace(swap.config, store, incoming.record, identity, swap.live)
	}
	locked := [][2]string{{incoming.record.AccountUUID, incoming.record.OrganizationUUID}}
	for _, record := range []*config.AccountRecord{store, third} {
		if record != nil {
			locked = append(locked, [2]string{record.AccountUUID, record.OrganizationUUID})
		}
	}
	slices.SortFunc(locked, func(a, b [2]string) int {
		if order := strings.Compare(a[0], b[0]); order != 0 {
			return order
		}
		return strings.Compare(a[1], b[1])
	})
	locked = slices.Compact(locked)
	deadline, _ := ctx.Deadline()
	for _, key := range locked {
		path := swap.paths.LockPath(key[0], key[1])
		guard, err := secret.Acquire(ctx, swap.paths.LocksDir(), filepath.Base(path), deadline)
		if err != nil {
			return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionUnreadable}, service, fmt.Sprintf("the namespace lock could not be taken: %v", err))
		}
		defer func() { _ = guard.Release() }()
	}
	var log *os.File
	if swap.live {
		log, err = secret.OpenAuditLog(swap.paths, secret.AuditLogPath(swap.paths))
		if err != nil {
			return useRefused(claude.SwapRefusal{Kind: claude.SwapAuditRefused}, service, fmt.Sprintf("a swap of the live store will not proceed unrecorded: %v", err))
		}
		defer func() { _ = log.Close() }()
	}
	if ambiguous != nil {
		return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionIdentityMismatch}, service, fmt.Sprintf("the outgoing credential cannot be adopted: its account is both `%s/%s` and `%s/%s` in agentctl's registry, and nothing says which of the two namespaces is its own; agentctl will not guess", ambiguous[0].AccountUUID, ambiguous[0].OrganizationUUID, ambiguous[1].AccountUUID, ambiguous[1].OrganizationUUID))
	}
	plan := useAdoptionPlan{kind: useAdoptionNothing}
	if displaced != nil {
		plan, refused = useDecideAdoption(ctx, useAdoptionParties{paths: swap.paths, store: store, subject: subject, incoming: incoming.record, incomingCredentials: credentials, third: third, identity: identity, displaced: displaced, live: swap.live, direction: incoming.direction})
		if refused != nil {
			return refused
		}
	}
	var configPath *string
	if swap.live {
		configPath = new(claude.GlobalConfigPath(swap.env))
	}
	if opts.JSON {
		if err := swap.process.emitUsePlan(subject, incoming.record, from8, planned, incoming.direction, configPath); err != nil {
			return useCannotAdopt(claude.AdoptionUnreadable, service)
		}
	}
	if !opts.Yes {
		verb := "replace"
		if incoming.direction == secret.DirectionUndo {
			verb = "put back"
		}
		from := "none"
		if from8 != nil {
			from = *from8
		}
		who := incoming.record.AccountUUID
		if incoming.record.Email != nil {
			who = *incoming.record.Email
		}
		configClause := ""
		if configPath != nil {
			configClause = claude.ConfigPlanLine(claude.ConfigShownPath(*configPath, swap.env.Home))
		}
		question := fmt.Sprintf("%s the credential in `%s` (digest %s) with `%s`'s (digest %s)%s? It takes effect on your next message, within 30 s; run `/model` once afterwards to refresh model access", verb, subject.storeDir, from, who, planned, configClause) + hints.consent()
		in, _ := swap.process.In.(*os.File)
		confirmed, err := (TerminalPrompt{In: in, Out: swap.process.Out}).Confirm(ctx, question)
		if !confirmed || err != nil {
			note := "cancelled at the confirmation prompt"
			if err != nil {
				note = err.Error()
			}
			return &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapCancelled}, service: service, note: &note}
		}
	}
	refreshUnsaved := false
	if credentials.AccessExpired(time.Now().UnixMilli(), claude.RefreshMarginMillis) {
		if incoming.source.kind == useSourceOwn && useMigrated(ctx, swap.paths, incoming.record) {
			return &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapNeedsRefresh}, service: service, note: new(fmt.Sprintf("`%s`'s credential has expired and its store has migrated into the keychain, so this swap cannot refresh it without discarding the result; run `agentctl claude status` to refresh that item in place and run this again", incoming.record.AccountUUID))}
		}
		if swap.live && incoming.source.kind == useSourceAdopted {
			if err := useAdoptedTakesRefresh(dir, derived); err != nil {
				return &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapNeedsRefresh}, service: service, note: new(fmt.Sprintf("`%s`'s parked credential has expired, and %v, so a refresh of it could not be saved; nothing was written — run `agentctl claude status` to settle that namespace, then run this again", usePrintable(incoming.record.AccountUUID), err))}
			}
		}
		client, err := claude.NewOAuthClientFromEnv()
		if err != nil {
			return useCannotAdopt(claude.AdoptionUnreadable, service)
		}
		credentials, err = client.RefreshAccess(ctx, credentials)
		if err != nil {
			return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionUnreadable}, service, err.Error())
		}
		var saveErr error
		home, consequence := "its own store", "that account may need `agentctl claude login`"
		if incoming.source.kind == useSourceOwn {
			saveErr = useGuardedWriteBack(ctx, swap.paths, incoming.record, credentials, derived)
		} else if swap.live {
			home, consequence = "its adopted copy", "the live item holds the only copy of it once this undo applies"
			saveErr = useWriteBackAdopted(ctx, swap.paths, dir, credentials, derived)
		} else {
			refreshUnsaved = true
		}
		if saveErr != nil {
			refreshUnsaved = true
			swap.noteUse(ctx, warnings, fmt.Sprintf("the incoming account's refreshed credential was not saved back to %s: %v; %s", home, saveErr, consequence))
		}
	}
	var installedProfile *claude.Profile
	if profiles != nil && !credentials.AccessExpired(time.Now().UnixMilli(), 0) {
		installedProfile, _ = profiles.ProfileOf(ctx, credentials)
		if installedProfile != nil && !claude.IdentityIs(&claude.Identity{AccountUUID: installedProfile.AccountUUID, OrganizationUUID: new(installedProfile.OrganizationUUID)}, incoming.record) {
			note := fmt.Sprintf("the credential to be installed cannot be written: the server says it belongs to `%s/%s`, but it was read as `%s`'s; agentctl will not install a credential under the wrong account", usePrintable(installedProfile.AccountUUID), usePrintable(installedProfile.OrganizationUUID), usePrintable(incoming.record.AccountUUID))
			if refreshUnsaved {
				note += "; it was refreshed on the way and the refreshed pair was not saved anywhere"
			}
			return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionIdentityMismatch}, service, note)
		}
	}
	after, err := credentials.Digests()
	if err != nil {
		return useCannotAdopt(claude.AdoptionUnreadable, service)
	}
	line, err := credentials.ToKeychainStdinLine(account, service)
	if err != nil {
		if _, tooLong := errors.AsType[*secret.LineTooLongError](err); tooLong {
			return useLineTooLong(service)
		}
		return useCannotAdopt(claude.AdoptionUnreadable, service)
	}
	to8, ok := secret.Digest8(after.AccessSHA256)
	if !ok {
		return useCannotAdopt(claude.AdoptionUnreadable, service)
	}
	adopted, staged, refusal := usePerformAdoption(ctx, swap.paths, plan, displaced)
	if refusal != "" {
		return useCannotAdopt(refusal, service)
	}
	if staged != nil {
		defer staged.Discard()
	}
	if !swap.live {
		fd, err := secret.OpenNamespaceDir(swap.paths, subject.storeDir)
		if err != nil {
			return useRefused(claude.SwapRefusal{Kind: claude.SwapCannotAdopt, Adoption: claude.AdoptionUnreadable}, service, fmt.Sprintf("the store directory could not be opened: %v", err))
		}
		_ = unix.Close(fd)
	}
	fault.Active().WaitIf("before_swap_write")
	var incomingIdentity *secret.IncomingIdentity
	restored := ""
	if swap.live {
		incomingIdentity = &secret.IncomingIdentity{AccountUUID: incoming.record.AccountUUID, OrganizationUUID: new(incoming.record.OrganizationUUID)}
		if incoming.direction == secret.DirectionUndo && incoming.source.kind == useSourceAdopted {
			restored = dir
		}
	}
	report := useWrite(ctx, useWritePhase{paths: swap.paths, env: swap.env, subject: subject, log: log, before: before, after: after, fromDigest8: from8, toDigest8: to8, adoptedTo: adopted, staged: staged, direction: incoming.direction, shadowingStore: !itemPresent && displaced != nil, restoredAdopted: restored, incomingIdentity: incomingIdentity}, line)
	if swap.live {
		report.config = useConfigStep(ctx, report.outcome, swap.env, installedProfile)
		if report.config != nil {
			useAppendAudit(ctx, swap.paths, log, report.config.Record(report.auditID))
			swap.process.tellUseConfig(ctx, swap.env, report.config, claude.ConfigRecovery{ID: incoming.record.AccountUUID, SameAgain: incoming.direction == secret.DirectionForward}, report)
		}
		if report.outcome.Kind == claude.SwapApplied {
			swap.process.tellUseSessions(ctx, hints, report)
		}
	}
	return report
}

func (swap useLiveSwap) noteUse(ctx context.Context, warnings *[]string, message string) {
	*warnings = append(*warnings, message)
	if err := tell(swap.process.Err, "note: "+message); err != nil {
		slog.ErrorContext(ctx, "the swap notice could not be written", slog.Any("error", err))
	}
}

func useAlreadyActive(service string, from, to *string, note string) *useReport {
	return &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapAlreadyActive}, service: service, fromDigest8: from, toDigest8: to, note: &note}
}

func useLineTooLong(service string) *useReport {
	return useRefused(claude.SwapRefusal{Kind: claude.SwapLineTooLong}, service, fmt.Sprintf("the credential does not fit the %d-byte keychain line the transport allows, so it cannot be written at all", secret.KeychainLineLimit))
}
