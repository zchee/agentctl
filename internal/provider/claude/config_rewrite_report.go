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

package claude

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/zchee/agentctl/internal/secret"
)

// ConfigReport describes a configuration rewrite without document values.
type ConfigReport struct {
	Outcome  secret.ConfigOutcome
	Reason   *secret.ConfigReason
	Account  *secret.IncomingIdentity
	FromSHA8 *string
	ToSHA8   *string
	Backup   *string
	HoldMS   *uint64
}

// ConfigNotAttempted records a configuration step deliberately not run.
func ConfigNotAttempted(reason secret.ConfigReason) ConfigReport {
	return ConfigReport{Outcome: secret.ConfigNotAttempted, Reason: &reason}
}

// NotUpdated returns the reason the file was not updated, or nil on success.
func (r ConfigReport) NotUpdated() *secret.ConfigReason {
	if r.Outcome == secret.ConfigApplied {
		return nil
	}
	return r.Reason
}

// Record forms the audit record following the named credential write.
func (r ConfigReport) Record(after *string) *secret.ConfigWriteRecord {
	return &secret.ConfigWriteRecord{After: after, Outcome: r.Outcome, Reason: r.Reason, Account: r.Account, FromSHA8: r.FromSHA8, ToSHA8: r.ToSHA8, Backup: r.Backup, HoldMS: r.HoldMS}
}

// ConfigReportJSON is the public configuration rewrite document, excluding account identifiers.
type ConfigReportJSON struct {
	Outcome  secret.ConfigOutcome `json:"outcome"`
	Reason   *secret.ConfigReason `json:"reason"`
	Backup   *string              `json:"backup"`
	HoldMS   *uint64              `json:"hold_ms"`
	BudgetMS uint64               `json:"budget_ms"`
}

// JSON returns the public configuration member, excluding account identifiers.
func (r ConfigReport) JSON() ConfigReportJSON {
	return ConfigReportJSON{Outcome: r.Outcome, Reason: r.Reason, Backup: r.Backup, HoldMS: r.HoldMS, BudgetMS: uint64(secret.ConfigHoldBudget.Milliseconds())}
}

// ConfigNotice is a sentence and whether it is a warning rather than a note.
type ConfigNotice struct {
	Warning bool
	Message string
}

// ConfigRecovery describes how the operator can catch up a configuration file.
type ConfigRecovery struct {
	ID           string
	SameAgain    bool
	AfterMessage bool
}

// ConfigPlanLine is the clause added to the live swap confirmation.
func ConfigPlanLine(shown string) string {
	return fmt.Sprintf(", and rewrite the account `%s` names", shown)
}

// CompletionClause describes when running sessions observe a rewritten file.
func CompletionClause() string { return "running sessions show the new account within a second; " }

// ConfigCatchUpQuestion asks permission to rewrite only the configuration file.
func ConfigCatchUpQuestion(who, shown string) string {
	return fmt.Sprintf("`%s`'s credential is already the one the live store holds; rewrite the account `%s` names to match it? Running sessions show it within a second; run `/model` once afterwards to refresh model access", who, shown)
}

// ConfigCatchUpClause describes a completed configuration-only rewrite.
func ConfigCatchUpClause(shown string) string {
	return fmt.Sprintf("`%s` now names that account too — running sessions show it within a second; run `/model` once to refresh model access", shown)
}

// ConfigNoticeFor returns no notice for an already-current or declined rewrite.
func ConfigNoticeFor(reason secret.ConfigReason, path, home string, recovery ConfigRecovery) *ConfigNotice {
	shown := ConfigShownPath(path, home)
	if reason == secret.ConfigReasonAlreadyCurrent || reason == secret.ConfigReasonDeclined {
		return nil
	}
	if reason == secret.ConfigReasonAbsent {
		return &ConfigNotice{Message: fmt.Sprintf("no `%s` to update; Claude Code writes `oauthAccount` at its next start", shown)}
	}
	clause := fmt.Sprintf("run `agentctl claude use --live %s`", configEscapeControls(recovery.ID))
	if recovery.SameAgain {
		clause = fmt.Sprintf("run the same `agentctl claude use --live %s` again", configEscapeControls(recovery.ID))
	}
	if recovery.AfterMessage {
		clause = "send one message in Claude Code, which refreshes the live credential, then " + clause
	}
	return &ConfigNotice{Warning: true, Message: fmt.Sprintf("`%s` was not updated (%s); running sessions keep showing the previous account — %s to update it", shown, configReasonPhrase(reason), clause)}
}

// ConfigShownPath abbreviates the home prefix and escapes control characters.
func ConfigShownPath(path, home string) string {
	if home != "" {
		if rest, err := filepath.Rel(home, path); err == nil && rest != ".." && !strings.HasPrefix(rest, ".."+string(filepath.Separator)) {
			if rest == "." {
				rest = ""
			}
			path = "~/" + rest
		}
	}
	return configEscapeControls(path)
}

func configEscapeControls(text string) string {
	var out strings.Builder
	for _, r := range text {
		switch r {
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if unicode.IsControl(r) {
				fmt.Fprintf(&out, `\u{%x}`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func configReasonPhrase(reason secret.ConfigReason) string {
	switch reason {
	case secret.ConfigReasonAbsent:
		return "there is no such file"
	case secret.ConfigReasonUnreadable:
		return "it could not be read as a regular file"
	case secret.ConfigReasonUnparseable:
		return "it is not valid JSON"
	case secret.ConfigReasonNotAnObject:
		return "its top level is not a JSON object"
	case secret.ConfigReasonNotReproducible:
		return "agentctl could not reproduce its bytes exactly, so it did not rewrite it"
	case secret.ConfigReasonBackupUnwritable:
		return "agentctl could not write a backup of it first"
	case secret.ConfigReasonLockBusy:
		return "a Claude Code session held its config lock"
	case secret.ConfigReasonLockStale:
		return "its config lock was left behind by a session that stopped"
	case secret.ConfigReasonCancelled:
		return "the run was cancelled before it could take the config lock"
	case secret.ConfigReasonChangedUnderLock:
		return "it changed while agentctl held its config lock"
	case secret.ConfigReasonCompromised:
		return "agentctl's config lock was broken while it held it"
	case secret.ConfigReasonBudget:
		return "the rewrite could not finish inside the config lock's time budget"
	case secret.ConfigReasonIO:
		return "the rewrite could not be written"
	case secret.ConfigReasonProfileUnavailable:
		return "the server could not be asked for that account's profile"
	case secret.ConfigReasonSwapUnknown:
		return "the swap's own outcome could not be confirmed"
	case secret.ConfigReasonAlreadyCurrent:
		return "it already names that account"
	case secret.ConfigReasonDeclined:
		return "the rewrite was not confirmed"
	case secret.ConfigReasonAuditRefused:
		return "agentctl's audit log is refused, and agentctl rewrites nothing unrecorded"
	default:
		return "for a reason this build does not know"
	}
}
