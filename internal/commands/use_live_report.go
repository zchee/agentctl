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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
	"github.com/zchee/agentctl/internal/secret"
)

type useLockReport struct {
	BudgetMS *uint64        `json:"budget_ms"`
	Break    jsontext.Value `json:"break"`
	HoldMS   *uint64        `json:"hold_ms"`
}

type useReport struct {
	outcome     claude.SwapOutcome
	target      *string
	service     string
	fromDigest8 *string
	toDigest8   *string
	auditID     *string
	adoptedTo   *string
	lock        useLockReport
	warnings    []string
	note        *string
	config      *claude.ConfigReport
}

func useRefused(refusal claude.SwapRefusal, service, note string) *useReport {
	return &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapRefused, Refusal: refusal}, service: service, note: &note}
}

func (p SessionProcess) emitUse(report *useReport, asJSON bool) error {
	if asJSON {
		var configuration any
		if report.config != nil {
			configuration = report.config.JSON()
		}
		doc := map[string]any{
			"kind":       "outcome",
			"outcome":    report.outcome.Word(),
			"target":     report.target,
			"service":    report.service,
			"from":       map[string]any{"digest8": report.fromDigest8},
			"to":         map[string]any{"digest8": report.toDigest8},
			"audit":      map[string]any{"id": report.auditID},
			"adopted_to": report.adoptedTo,
			"lock":       report.lock,
			"warnings":   report.warnings,
			"note":       report.note,
			"refusal":    nil,
			"config":     configuration,
		}
		if report.outcome.Kind == claude.SwapRefused {
			if reason := report.outcome.Refusal.Reason(); reason != "" {
				doc["reason"] = reason
			} else {
				doc["refusal"] = report.outcome.Refusal.Letter()
			}
		}
		return p.emitUseJSON(doc, "could not render the swap as JSON")
	}
	if report.outcome.Kind == claude.SwapApplied {
		digest := "unknown"
		if report.toDigest8 != nil {
			digest = *report.toDigest8
		}
		clause := ""
		if report.config != nil && report.config.NotUpdated() == nil {
			clause = claude.CompletionClause()
		}
		if err := tell(p.Out, fmt.Sprintf("swapped: `%s` now holds the incoming credential (digest %s). It takes effect on your next message, within 30 s; %srun `/model` once to refresh model access.", report.service, digest, clause)); err != nil {
			return err
		}
		if report.note != nil {
			if err := tell(p.Err, "warning: "+*report.note); err != nil {
				return err
			}
		}
	} else if report.note != nil {
		if err := tell(p.Out, report.outcome.Word()+": "+*report.note); err != nil {
			return err
		}
	} else if err := tell(p.Out, report.outcome.Word()); err != nil {
		return err
	}
	if report.adoptedTo != nil {
		if err := tell(p.Out, fmt.Sprintf("the displaced credential was adopted into `%s`", *report.adoptedTo)); err != nil {
			return err
		}
	}
	if report.auditID != nil {
		if err := tell(p.Out, "audit: "+*report.auditID); err != nil {
			return err
		}
	}
	return nil
}

func (p SessionProcess) emitUsePlan(subject *useSubject, incoming *config.AccountRecord, fromDigest8 *string, toDigest8 string, direction secret.WriteDirection, configPath *string) error {
	account := incoming.AccountUUID
	if incoming.Email != nil {
		account = *incoming.Email
	}
	word := "forward"
	if direction == secret.DirectionUndo {
		word = "reverse"
	}
	doc := map[string]any{
		"kind":        "plan",
		"direction":   word,
		"store_dir":   subject.storeDir,
		"service":     subject.service,
		"account":     account,
		"from":        map[string]any{"digest8": fromDigest8},
		"to":          map[string]any{"digest8": toDigest8},
		"config_path": configPath,
	}
	return p.emitUseJSON(doc, "the swap plan could not be rendered")
}

func (p SessionProcess) emitUseJSON(doc any, message string) error {
	data, err := json.Marshal(doc, jsontext.WithIndent("  "), jsontext.EscapeForHTML(false), json.Deterministic(true))
	if err != nil {
		return errs.NewConfig(message)
	}
	return render.Print(p.Out, string(data))
}
