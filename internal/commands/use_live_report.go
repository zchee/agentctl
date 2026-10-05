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

type useBreakReport struct {
	Broke          bool                  `json:"broke"`
	Outcome        secret.BreakOutcome   `json:"outcome"`
	Reason         *secret.BreakReason   `json:"reason"`
	HolderEvidence secret.HolderEvidence `json:"holder_evidence"`
}

type useLockReport struct {
	HoldMS   *uint64         `json:"hold_ms"`
	BudgetMS *uint64         `json:"budget_ms"`
	Break    *useBreakReport `json:"break"`
}

type useDigestReport struct {
	Digest8 *string `json:"digest8"`
}

type useOutcomeDocument struct {
	Kind    string          `json:"kind"`
	Outcome string          `json:"outcome"`
	Target  *string         `json:"target"`
	Service string          `json:"service"`
	From    useDigestReport `json:"from"`
	To      useDigestReport `json:"to"`
	Audit   struct {
		ID *string `json:"id"`
	} `json:"audit"`
	AdoptedTo *string                  `json:"adopted_to"`
	Lock      useLockReport            `json:"lock"`
	Warnings  []string                 `json:"warnings"`
	Note      *string                  `json:"note"`
	Refusal   *string                  `json:"refusal"`
	Config    *claude.ConfigReportJSON `json:"config"`
	Reason    *string                  `json:"reason,omitzero"`
}

type usePlanDocument struct {
	Kind       string          `json:"kind"`
	Direction  string          `json:"direction"`
	StoreDir   string          `json:"store_dir"`
	Service    string          `json:"service"`
	Account    string          `json:"account"`
	From       useDigestReport `json:"from"`
	To         useDigestReport `json:"to"`
	ConfigPath *string         `json:"config_path"`
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
		doc := useOutcomeDocument{
			Kind:      "outcome",
			Outcome:   report.outcome.Word(),
			Target:    report.target,
			Service:   report.service,
			From:      useDigestReport{Digest8: report.fromDigest8},
			To:        useDigestReport{Digest8: report.toDigest8},
			AdoptedTo: report.adoptedTo,
			Lock:      report.lock,
			Warnings:  report.warnings,
			Note:      report.note,
		}
		doc.Audit.ID = report.auditID
		if report.config != nil {
			doc.Config = new(report.config.JSON())
		}
		if report.outcome.Kind == claude.SwapRefused {
			if reason := report.outcome.Refusal.Reason(); reason != "" {
				doc.Reason = &reason
			} else {
				doc.Refusal = new(report.outcome.Refusal.Letter())
			}
		}
		return p.emitUseJSON(&doc, "could not render the swap as JSON")
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
	doc := usePlanDocument{
		Kind:       "plan",
		Direction:  word,
		StoreDir:   subject.storeDir,
		Service:    subject.service,
		Account:    account,
		From:       useDigestReport{Digest8: fromDigest8},
		To:         useDigestReport{Digest8: &toDigest8},
		ConfigPath: configPath,
	}
	return p.emitUseJSON(&doc, "the swap plan could not be rendered")
}

func (p SessionProcess) emitUseJSON(doc any, message string) error {
	data, err := json.Marshal(doc, jsontext.WithIndent("  "), jsontext.EscapeForHTML(false), json.Deterministic(true))
	if err != nil {
		return errs.NewConfig(message)
	}
	return render.Print(p.Out, string(data))
}
