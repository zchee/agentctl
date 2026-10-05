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
	"log/slog"
	"os"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func useConfigStep(ctx context.Context, outcome claude.SwapOutcome, env *claude.EnvView, profile *claude.Profile) *claude.ConfigReport {
	if env == nil {
		return nil
	}
	switch outcome.Kind {
	case claude.SwapApplied:
		if profile == nil {
			return new(claude.ConfigNotAttempted(secret.ConfigReasonProfileUnavailable))
		}
		return new(claude.RewriteConfig(ctx, env, profile, nil))
	case claude.SwapUnknown:
		return new(claude.ConfigNotAttempted(secret.ConfigReasonSwapUnknown))
	default:
		return nil
	}
}

type useCatchUp struct {
	paths     *config.Paths
	env       *claude.EnvView
	record    *config.AccountRecord
	profile   *claude.Profile
	direction secret.WriteDirection
	opts      cli.ClaudeUseOptions
	hints     useSessionHints
}

func (p SessionProcess) catchUpUse(ctx context.Context, input useCatchUp, report *useReport) *useReport {
	path := claude.GlobalConfigPath(input.env)
	shown := claude.ConfigShownPath(path, input.env.Home)
	configuration := claude.ConfigNotAttempted(secret.ConfigReasonProfileUnavailable)
	if input.profile != nil {
		log, err := secret.OpenAuditLog(input.paths, secret.AuditLogPath(input.paths))
		if err != nil {
			slog.DebugContext(ctx, "the configuration catch-up met a refused audit log", slog.Any("error", err))
			configuration = claude.ConfigNotAttempted(secret.ConfigReasonAuditRefused)
			configuration.Outcome = secret.ConfigRefused
		} else {
			defer func() { _ = log.Close() }()
			checked, stopped := claude.CheckConfigCatchUp(input.env, input.profile)
			shouldAudit := true
			if stopped != nil {
				configuration = *stopped
			} else {
				if input.opts.JSON {
					if err := p.emitUseConfigPlan(shown, input.profile); err != nil {
						slog.ErrorContext(ctx, "the configuration plan could not be rendered", slog.Any("error", err))
					}
				}
				confirmed := input.opts.Yes
				if !confirmed {
					who := input.record.AccountUUID
					if input.record.Email != nil {
						who = *input.record.Email
					}
					in, _ := p.In.(*os.File)
					confirmed, _ = (TerminalPrompt{In: in, Out: p.Out}).Confirm(ctx, claude.ConfigCatchUpQuestion(who, shown)+input.hints.consent())
				}
				if confirmed {
					configuration = claude.WriteConfigCatchUp(ctx, checked, input.env, input.profile, nil)
				} else {
					configuration = claude.ConfigNotAttempted(secret.ConfigReasonDeclined)
					shouldAudit = false
				}
			}
			if shouldAudit {
				useAppendAudit(ctx, input.paths, log, configuration.Record(nil))
			}
		}
	}
	if configuration.NotUpdated() == nil && report.note != nil {
		report.note = new(*report.note + "; " + claude.ConfigCatchUpClause(shown))
	}
	recovery := claude.ConfigRecovery{ID: input.record.AccountUUID, SameAgain: input.direction == secret.DirectionForward, AfterMessage: input.profile == nil}
	p.tellUseConfig(ctx, input.env, &configuration, recovery, report)
	report.config = &configuration
	if configuration.NotUpdated() == nil {
		p.tellUseSessions(ctx, input.hints, report)
	}
	return report
}

func (p SessionProcess) tellUseConfig(ctx context.Context, env *claude.EnvView, configuration *claude.ConfigReport, recovery claude.ConfigRecovery, report *useReport) {
	reason := configuration.NotUpdated()
	if reason == nil {
		return
	}
	notice := claude.ConfigNoticeFor(*reason, claude.GlobalConfigPath(env), env.Home, recovery)
	if notice == nil {
		return
	}
	prefix := "note: "
	if notice.Warning {
		prefix = "warning: "
		report.warnings = append(report.warnings, notice.Message)
	}
	if err := tell(p.Err, prefix+notice.Message); err != nil {
		slog.ErrorContext(ctx, "the configuration notice could not be written", slog.Any("error", err))
	}
}

func (p SessionProcess) emitUseConfigPlan(shown string, profile *claude.Profile) error {
	return p.emitUseJSON(map[string]any{
		"kind": "plan", "direction": "config", "config_path": shown,
		"account": map[string]string{"account_uuid": profile.AccountUUID, "organization_uuid": profile.OrganizationUUID},
	}, "the configuration plan could not be rendered")
}

func useAppendAudit(ctx context.Context, paths *config.Paths, held *os.File, event secret.AuditEvent) *string {
	entry := secret.NewAuditEntry(event)
	var id secret.AuditID
	var err error
	if held != nil {
		id, err = secret.AuditAppendThrough(held, secret.AuditLogPath(paths), entry)
	} else {
		id, err = secret.AuditAppend(context.WithoutCancel(ctx), paths, entry)
	}
	if err != nil {
		slog.ErrorContext(ctx, "an audit entry could not be appended", slog.Any("error", err))
		return nil
	}
	return new(id.String())
}
