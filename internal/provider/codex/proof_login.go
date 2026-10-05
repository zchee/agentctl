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
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/zchee/agentctl/internal/config"
)

// ScratchSurvey records a complete inspection of the child's scratch home.
// Only the child runner can populate its private fields.
type ScratchSurvey struct {
	daemonDir           bool
	heldLocks, oddLocks []string
	truncated           bool
}

// PostExitReport is evidence from one exited login child and two keychain listings.
type PostExitReport struct {
	gainedCodexAuth, survivors []string
	survey                     ScratchSurvey
	exit                       *os.ProcessState
}

func postExitReportFromChild(gainedCodexAuth, survivors []string, survey ScratchSurvey, exit *os.ProcessState) *PostExitReport {
	survey.heldLocks = slices.Clone(survey.heldLocks)
	survey.oddLocks = slices.Clone(survey.oddLocks)
	return &PostExitReport{gainedCodexAuth: slices.Clone(gainedCodexAuth), survivors: slices.Clone(survivors), survey: survey, exit: exit}
}

// GainedCodexAuth returns an isolated copy of the gained keychain account names.
func (r *PostExitReport) GainedCodexAuth() []string {
	if r == nil {
		return nil
	}
	return slices.Clone(r.gainedCodexAuth)
}

func (r *PostExitReport) clean() bool {
	return r != nil && r.exit != nil && r.exit.Success() && len(r.gainedCodexAuth) == 0 && len(r.survivors) == 0 && !r.survey.daemonDir && len(r.survey.heldLocks) == 0 && len(r.survey.oddLocks) == 0 && !r.survey.truncated
}

func (r *PostExitReport) anomalies() []string {
	if r == nil {
		return []string{"the login produced no post-exit report"}
	}
	var found []string
	if r.exit == nil {
		found = append(found, "the login produced no exit status")
	} else if !r.exit.Success() {
		found = append(found, "the login exited with "+r.exit.String())
	}
	if len(r.gainedCodexAuth) > 0 {
		var named []string
		other := 0
		for _, account := range r.gainedCodexAuth {
			if IsHomeAccount(account) {
				named = append(named, account)
			} else {
				other++
			}
		}
		which := ""
		if len(named) > 0 {
			which = ": " + strings.Join(named, ", ")
		}
		rest := ""
		if other > 0 {
			rest = fmt.Sprintf(" (%d of them under an account agentctl would not have written, so it is not printed; look in Keychain Access)", other)
		}
		found = append(found, fmt.Sprintf("the login created %d `Codex Auth` keychain item(s)%s%s", len(r.gainedCodexAuth), which, rest))
	}
	if len(r.survivors) > 0 {
		found = append(found, fmt.Sprintf("%d process(es) still use the scratch home", len(r.survivors)))
	}
	if r.survey.daemonDir {
		found = append(found, "the login started a Codex daemon in the scratch home")
	}
	if len(r.survey.oddLocks) > 0 {
		var named []string
		other := 0
		for _, path := range r.survey.oddLocks {
			name := filepath.Base(path)
			if config.ValidateCodexSegment(name) == nil {
				named = append(named, name)
			} else {
				other++
			}
		}
		which := ""
		if len(named) > 0 {
			which = ": " + strings.Join(named, ", ")
		}
		rest := ""
		if other > 0 {
			rest = fmt.Sprintf(" (%d unnameable lock file(s), not printed)", other)
		}
		found = append(found, fmt.Sprintf("%d entr(y/ies) named `*.lock` in the scratch home are not regular files%s%s", len(r.survey.oddLocks), which, rest))
	}
	if r.survey.truncated {
		found = append(found, "the scratch home was too large or too deep to survey completely")
	}
	if len(r.survey.heldLocks) > 0 {
		found = append(found, fmt.Sprintf("%d lock file(s) are still held in the scratch home", len(r.survey.heldLocks)))
	}
	return found
}

type verifiedLoginState struct {
	doc           *Credentials
	user, account string
	consumed      atomic.Bool
}

// VerifiedLogin holds one parsed, validated login; its shared state installs at most once.
type VerifiedLogin struct{ state *verifiedLoginState }

// Identity returns only the verified login's non-secret labels.
func (v *VerifiedLogin) Identity() Identity {
	if v == nil || v.state == nil {
		return Identity{}
	}
	result := Identity{UserID: v.state.user, AccountID: v.state.account}
	if identity := v.state.doc.Identity(); identity != nil {
		result.Email = identity.Email
		result.Plan = identity.Plan
	}
	return result
}

// OwnedRecord returns the validated proof for the login's future namespace.
func (v *VerifiedLogin) OwnedRecord() *OwnedRecord {
	if v == nil || v.state == nil {
		return nil
	}
	return &OwnedRecord{user: v.state.user, account: v.state.account, refresh: config.RefreshAuto}
}

// String prevents formatting from traversing credential material.
func (VerifiedLogin) String() string { return "VerifiedLogin{[REDACTED]}" }

// Format redacts every formatting verb.
func (VerifiedLogin) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "VerifiedLogin{[REDACTED]}")
}

// LogValue redacts structured logging.
func (VerifiedLogin) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalJSON redacts generic serialization.
func (VerifiedLogin) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
