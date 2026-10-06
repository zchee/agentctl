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
	"log/slog"
	"time"

	"github.com/zchee/agentctl/internal/config"
	codexprovider "github.com/zchee/agentctl/internal/provider/codex"
	"github.com/zchee/agentctl/internal/render"
)

func refreshPrePass(ctx context.Context, permit *codexprovider.PostPermit, plans []RowPlan, paths *config.Paths, deadline time.Time) []RowPlan {
	options := codexprovider.RefreshContext{Paths: paths, Deadline: deadline, LockBudget: time.Second}
	for i := range plans {
		plan := &plans[i]
		if !plan.mayRefresh() {
			continue
		}
		owned := codexprovider.Owned(plan.Source.Record)
		if owned == nil {
			continue
		}
		report := codexprovider.RunRefresh(ctx, permit, owned, codexprovider.SendMode{Kind: codexprovider.SendProactive}, options)
		slog.DebugContext(ctx, "codex refresh pre-pass", "step", report.Step.Kind)
		plan.PrePass = &report
	}
	return plans
}

func afterUnauthorized(ctx context.Context, permit *codexprovider.PostPermit, passes []rowPass, shared *passShared, deadline time.Time) []rowPass {
	options := codexprovider.RefreshContext{Paths: shared.paths, Deadline: deadline, LockBudget: time.Second}
	for i := range passes {
		pass := passes[i]
		owned := codexprovider.Owned(pass.plan.Source.Record)
		if pass.rejected == "" || owned == nil {
			settleRetry(ctx, permit, &pass, options)
			continue
		}
		report := codexprovider.RunRefresh(ctx, permit, owned, codexprovider.SendMode{Kind: codexprovider.SendAfterUnauthorized, RejectedAccessDigest8: pass.rejected}, options)
		slog.DebugContext(ctx, "codex refresh after a 401", "step", report.Step.Kind)
		retry := retryNone
		switch report.Step.Kind {
		case codexprovider.RefreshStepRefreshed:
			retry = retryAfterSend
		case codexprovider.RefreshStepAdopted, codexprovider.RefreshStepDiscardedExternal:
			retry = retryAfterAdopt
		case codexprovider.RefreshStepRacedExternal:
			retry = retryVerify
		}
		if retry != retryNone {
			plan := pass.plan
			plan.Retry, plan.PrePass = retry, &report
			pass = runRow(ctx, plan, shared)
			settleRetry(ctx, permit, &pass, options)
		} else {
			unrefreshedUnauthorized(&pass, &report)
		}
		passes[i] = pass
	}
	return passes
}

func settleRetry(ctx context.Context, permit *codexprovider.PostPermit, pass *rowPass, options codexprovider.RefreshContext) {
	sent := pass.plan.Retry == retryAfterSend || pass.plan.PrePass != nil && pass.plan.PrePass.Step.Kind == codexprovider.RefreshStepRefreshed
	answer := codexprovider.RetryGetResult(0)
	hasAnswer := false
	switch {
	case pass.plan.Retry == retryAfterSend && pass.fetched:
		answer, hasAnswer = codexprovider.RetryGetSucceeded, true
	case pass.plan.Retry == retryAfterSend && pass.account.State.Kind == codexprovider.StateUnauthorized:
		answer, hasAnswer = codexprovider.RetryGetUnauthorized, true
	case pass.fetched && sent && pass.floorRaised:
		answer, hasAnswer = codexprovider.RetryGetSucceeded, true
	}
	if !hasAnswer {
		return
	}
	owned := codexprovider.Owned(pass.plan.Source.Record)
	if owned == nil {
		return
	}
	if err := codexprovider.RecordRetryGet(ctx, permit, owned, answer, options); err != nil {
		slog.WarnContext(ctx, "the answer to a retried Codex usage request was not recorded", "error", err)
	}
}

func unrefreshedUnauthorized(pass *rowPass, report *codexprovider.RefreshReport) {
	var note *string
	switch report.Step.Kind {
	case codexprovider.RefreshStepUnauthorizedFloor:
		pass.account.State = codexprovider.State{Kind: codexprovider.StateUnauthorizedFloor}
		note = new("no refresh before " + render.FormatInstant(report.Step.Until))
	case codexprovider.RefreshStepUnauthorizedTerminal:
		pass.account.State = codexprovider.State{Kind: codexprovider.StateUnauthorizedTerminal}
		note = new("run agentctl codex login, or agentctl codex accounts refresh " + pass.account.UserID + "/" + pass.account.AccountID + " --reset-floor")
	default:
		state, stateNote, ok := stepState(report.Step)
		if ok {
			pass.account.State, note = state, stateNote
		} else {
			pass.account.State = codexprovider.State{Kind: codexprovider.StateUnauthorized}
			note = stepNote(report.Step)
		}
	}
	pushOptionalNote(&pass.account.Note, note)
	pushReportNotes(&pass.account.Note, report)
}
