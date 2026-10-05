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

package render

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strconv"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/provider/codex"
)

// StatusReportVersionV2 is the provider-neutral status document version.
const StatusReportVersionV2 = 2

// StatusReportV2 is the ordered machine-readable status document.
type StatusReportV2 struct {
	Version     int         `json:"version"`
	GeneratedAt string      `json:"generated_at"`
	Rows        []JSONRowV2 `json:"rows"`
	Hidden      int         `json:"hidden"`
	Raw         *RawBodies  `json:"raw,omitzero"`
}

// JSONRowV2 publishes identity and usage figures, never credentials.
type JSONRowV2 struct {
	Provider     string         `json:"provider"`
	ID           string         `json:"id"`
	Identity     JSONIdentityV2 `json:"identity"`
	Kind         string         `json:"kind"`
	Source       *string        `json:"source"`
	State        string         `json:"state"`
	StateLabel   string         `json:"state_label"`
	LockState    string         `json:"lock_state"`
	Note         *string        `json:"note"`
	Windows      []JSONWindowV2 `json:"windows"`
	Credits      JSONCreditsV2  `json:"credits"`
	NextReset    *string        `json:"next_reset"`
	SessionReset *string        `json:"session_reset"`
	WeeklyReset  *string        `json:"weekly_reset"`
}

// JSONIdentityV2 names a person and workspace in provider-neutral terms.
type JSONIdentityV2 struct {
	UserID    string  `json:"user_id"`
	AccountID string  `json:"account_id"`
	PlanType  *string `json:"plan_type"`
	Email     *string `json:"email"`
	OrgName   *string `json:"org_name"`
}

// JSONWindowV2 is a normalized window with optional vendor feature names.
type JSONWindowV2 struct {
	Kind            string         `json:"kind"`
	Label           string         `json:"label"`
	Percent         *JSONPercentV2 `json:"percent"`
	PercentFloor    *int           `json:"percent_floor"`
	ResetsAt        *string        `json:"resets_at"`
	IsActive        bool           `json:"is_active"`
	LimitName       *string        `json:"limit_name"`
	MeteredFeature  *string        `json:"metered_feature"`
	NormalModelSlug *string        `json:"normal_model_slug"`
}

// JSONPercentV2 retains a floating-point spelling even for whole percentages.
type JSONPercentV2 float64

// MarshalJSONTo matches the canonical configuration serializer's number spelling
// for bounded percentages so both serializers retain floating-point values.
func (p JSONPercentV2) MarshalJSONTo(encoder *jsontext.Encoder) error {
	text := strconv.FormatFloat(float64(p), 'f', -1, 64)
	// Very small percentages use exponent notation with no zero padding.
	if p != 0 && p > -0.00001 && p < 0.00001 {
		text = strconv.FormatFloat(float64(p), 'e', -1, 64)
		mantissa, exponent, _ := strings.Cut(text, "e")
		number, err := strconv.Atoi(exponent)
		if err != nil {
			return err
		}
		text = mantissa + "e" + strconv.Itoa(number)
	} else if !strings.Contains(text, ".") {
		text += ".0"
	}
	return encoder.WriteValue(jsontext.Value(text))
}

// JSONCreditsV2 is a tagged, flat credits union with nullable fields.
type JSONCreditsV2 struct {
	Kind           string  `json:"kind"`
	UsedMinor      *int64  `json:"used_minor"`
	Currency       *string `json:"currency"`
	Exponent       *int    `json:"exponent"`
	LimitMinor     *int64  `json:"limit_minor"`
	Percent        *int    `json:"percent"`
	Balance        *string `json:"balance"`
	Unlimited      *bool   `json:"unlimited"`
	DisabledReason *string `json:"disabled_reason"`
	Scope          string  `json:"scope"`
}

// NewStatusReportV2 returns an empty document with the supplied stamp.
func NewStatusReportV2(now time.Time, hidden int) StatusReportV2 {
	return StatusReportV2{Version: StatusReportVersionV2, GeneratedAt: FormatInstant(now), Rows: []JSONRowV2{}, Hidden: hidden}
}

// MarshalStatusReportV2 writes a two-space-indented ordered JSON document.
func MarshalStatusReportV2(report *StatusReportV2) ([]byte, error) {
	out, err := json.Marshal(report, jsontext.WithIndent("  "))
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out, []byte("\n")), nil
}

// UnavailableCreditsV2 returns the explicit no-credits object.
func UnavailableCreditsV2() JSONCreditsV2 {
	return JSONCreditsV2{Kind: "unavailable", Scope: CreditsScope}
}

// CodexCreditsV2 preserves the wire's decimal-string balance.
func CodexCreditsV2(credits codex.Credits) JSONCreditsV2 {
	out := UnavailableCreditsV2()
	if credits.Available {
		out.Kind = "balance"
		out.Balance = credits.Balance
		out.Unlimited = new(credits.Unlimited)
	}
	return out
}

// CodexWindowsV2 builds each window in response order.
func CodexWindowsV2(snapshot *codex.Usage) []JSONWindowV2 {
	if snapshot == nil {
		return []JSONWindowV2{}
	}
	out := make([]JSONWindowV2, 0, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		shared := windowJSON(&window.Window)
		var percent *JSONPercentV2
		if shared.Percent != nil {
			percent = new(JSONPercentV2(*shared.Percent))
		}
		out = append(out, JSONWindowV2{Kind: shared.Kind, Label: shared.Label, Percent: percent, PercentFloor: shared.PercentFloor, ResetsAt: shared.ResetsAt, IsActive: shared.IsActive, LimitName: window.LimitName, MeteredFeature: window.MeteredFeature, NormalModelSlug: window.NormalModelSlug})
	}
	return out
}
