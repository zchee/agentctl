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
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/zchee/agentctl/internal/usage"
)

// IsKnownPlan reports whether a tier is safe to publish by name.
func IsKnownPlan(plan string) bool {
	switch plan {
	case "guest", "free", "go", "plus", "pro", "prolite", "free_workspace", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "education", "quorum", "k12", "enterprise", "edu", "edu_plus", "edu_pro", "unknown":
		return true
	default:
		return false
	}
}

// SessionMaxSeconds is the longest duration classified as a session.
const SessionMaxSeconds int64 = 21600

// WeeklyMinSeconds is the shortest duration classified as weekly.
const WeeklyMinSeconds int64 = 518400

// WeeklyMaxSeconds is the longest duration classified as weekly.
const WeeklyMaxSeconds int64 = 691200

// Window adds vendor feature names to a normalized limit window.
type Window struct {
	Window          usage.LimitWindow
	LimitName       *string
	MeteredFeature  *string
	NormalModelSlug *string
}

// Credits preserves the decimal balance without inventing a currency.
type Credits struct {
	Available bool
	Balance   *string
	Unlimited bool
}

// Usage contains one response's normalized figures and email-free raw body.
type Usage struct {
	FetchedAt    time.Time
	Windows      []Window
	Credits      Credits
	PlanType     *string
	Note         *string
	Raw          jsontext.Value
	HasRateLimit bool
}

// String summarizes normalized usage without echoing unknown raw members.
func (u Usage) String() string {
	return fmt.Sprintf("Usage{FetchedAt: %s, Windows: %d, HasRateLimit: %t}", u.FetchedAt.Format(time.RFC3339Nano), len(u.Windows), u.HasRateLimit)
}

// GoString summarizes normalized usage without echoing unknown raw members.
func (u Usage) GoString() string { return u.String() }

// Normalize reads a response object and optionally retains it without email.
// Unrecognized durations remain visible; malformed optional figures are absent.
func Normalize(body jsontext.Value, fetchedAt time.Time, keepRaw bool) (*Usage, error) {
	object, err := usageObject(body)
	if err != nil {
		return nil, fmt.Errorf("the response was not a JSON object")
	}
	out := &Usage{FetchedAt: fetchedAt}
	limit, limitErr := usageObject(object.get("rate_limit"))
	out.HasRateLimit = limitErr == nil
	if out.HasRateLimit {
		for _, slot := range []string{"primary_window", "secondary_window"} {
			if window, err := usageObject(limit.get(slot)); err == nil {
				out.Windows = append(out.Windows, Window{Window: normalizeWindow(window, durationKind(window), "", fetchedAt)})
			}
		}
	}
	var additional []jsontext.Value
	if object.get("additional_rate_limits").Kind() == '[' {
		_ = json.Unmarshal(object.get("additional_rate_limits"), &additional)
	}
	for index, raw := range additional {
		row, err := usageObject(raw)
		if err != nil {
			continue
		}
		name, feature, model := usageLabel(row.get("limit_name")), usageLabel(row.get("metered_feature")), usageLabel(row.get("normal_model_slug"))
		base := fmt.Sprintf("additional[%d]", index)
		if name != nil {
			base = *name
		} else if feature != nil {
			base = *feature
		}
		limits, err := usageObject(row.get("rate_limit"))
		if err != nil {
			continue
		}
		for _, slot := range []string{"primary_window", "secondary_window"} {
			window, err := usageObject(limits.get(slot))
			if err != nil {
				continue
			}
			kind := usage.WindowKind{Class: usage.WindowUnknown, Name: base + ":" + strings.TrimSuffix(slot, "_window")}
			out.Windows = append(out.Windows, Window{Window: normalizeWindow(window, kind, base, fetchedAt), LimitName: name, MeteredFeature: feature, NormalModelSlug: model})
		}
	}
	if credits, err := usageObject(object.get("credits")); err == nil {
		out.Credits.Available = true
		out.Credits.Unlimited, _ = usageBool(credits.get("unlimited"))
		if text, ok := usageString(credits.get("balance")); ok && decimalBalance(text) {
			out.Credits.Balance = new(text)
		}
	}
	if plan, ok := usageString(object.get("plan_type")); ok {
		if !IsKnownPlan(plan) {
			plan = "unknown"
		}
		out.PlanType = new(plan)
	}
	if reached, err := usageObject(object.get("rate_limit_reached_type")); err == nil {
		if label := usageLabel(reached.get("type")); label != nil {
			out.Note = new("limit reached: " + *label)
		}
	}
	if out.Note == nil {
		if reached, _ := usageBool(limit.get("limit_reached")); reached {
			out.Note = new("limit reached")
		} else if allowed, ok := usageBool(limit.get("allowed")); ok && !allowed {
			out.Note = new("requests are not currently allowed")
		}
	}
	if keepRaw {
		for i, member := range object {
			if member.name == "email" {
				last := len(object) - 1
				object[i] = object[last]
				object = object[:last]
				break
			}
		}
		var buffer bytes.Buffer
		encoder := jsontext.NewEncoder(&buffer)
		if err := encoder.WriteToken(jsontext.BeginObject); err != nil {
			return nil, err
		}
		for _, member := range object {
			if err := encoder.WriteToken(jsontext.String(member.name)); err != nil {
				return nil, err
			}
			if err := encoder.WriteValue(member.value); err != nil {
				return nil, err
			}
		}
		if err := encoder.WriteToken(jsontext.EndObject); err != nil {
			return nil, err
		}
		out.Raw = bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	}
	return out, nil
}

func normalizeWindow(object usageMembers, kind usage.WindowKind, scope string, fetchedAt time.Time) usage.LimitWindow {
	window := usage.LimitWindow{Kind: kind, ScopeLabel: scope}
	if value := object.get("used_percent"); value.Kind() == '0' {
		if percent, err := strconv.ParseFloat(string(value), 64); err == nil {
			if clamped, ok := usage.ClampPercent(percent); ok {
				window.Percent = new(clamped)
			}
			if floored, ok := usage.PercentFloor(percent); ok {
				window.PercentFloor = new(floored)
			}
		}
	}
	if second, ok := usageInteger(object.get("reset_at")); ok {
		window.ResetsAt = usageTimestamp(second)
	}
	if window.ResetsAt.IsZero() {
		if after, ok := usageInteger(object.get("reset_after_seconds")); ok {
			second := fetchedAt.Unix()
			if (after <= 0 || second <= math.MaxInt64-after) && (after >= 0 || second >= math.MinInt64-after) {
				window.ResetsAt = usageTimestamp(second + after)
			}
		}
	}
	return window
}

func usageTimestamp(second int64) time.Time {
	// Limit timestamps to the published RFC 3339 calendar range.
	if second < -377705116800 || second > 253402300799 {
		return time.Time{}
	}
	return time.Unix(second, 0).UTC()
}

func durationKind(window usageMembers) usage.WindowKind {
	seconds, ok := usageInteger(window.get("limit_window_seconds"))
	if !ok {
		return usage.WindowKind{Class: usage.WindowUnknown, Name: "unknown duration"}
	}
	switch {
	case seconds > 0 && seconds <= SessionMaxSeconds:
		return usage.WindowKind{Class: usage.WindowSession}
	case seconds >= WeeklyMinSeconds && seconds <= WeeklyMaxSeconds:
		return usage.WindowKind{Class: usage.WindowWeeklyAll}
	case seconds/3600 > 0:
		return usage.WindowKind{Class: usage.WindowUnknown, Name: fmt.Sprintf("%dh", seconds/3600)}
	default:
		return usage.WindowKind{Class: usage.WindowUnknown, Name: "unknown duration"}
	}
}

func usageLabel(value jsontext.Value) *string {
	text, ok := usageString(value)
	if !ok {
		return nil
	}
	plain := text != "" && len(text) <= 64 && !strings.HasPrefix(text, " ") && !strings.HasSuffix(text, " ")
	for _, ch := range []byte(text) {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && !strings.ContainsRune("-_.+ ", rune(ch)) {
			plain = false
		}
	}
	if !plain {
		text = "<unrecognised>"
	}
	return new(text)
}

func decimalBalance(text string) bool {
	digits := strings.TrimPrefix(text, "-")
	whole, fraction, dot := strings.Cut(digits, ".")
	numeric := func(part string) bool {
		if part == "" {
			return false
		}
		for _, ch := range []byte(part) {
			if ch < '0' || ch > '9' {
				return false
			}
		}
		return true
	}
	return numeric(whole) && (!dot || numeric(fraction))
}

type usageMember struct {
	name  string
	value jsontext.Value
}
type usageMembers []usageMember

func usageObject(value jsontext.Value) (usageMembers, error) {
	if value.Kind() != '{' {
		return nil, fmt.Errorf("not an object")
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(value), jsontext.AllowDuplicateNames(true))
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	var members usageMembers
	for decoder.PeekKind() != '}' {
		token, err := decoder.ReadToken()
		if err != nil {
			return nil, err
		}
		name := token.String()
		raw, err := decoder.ReadValue()
		if err != nil {
			return nil, err
		}
		replaced := false
		for i := range members {
			if members[i].name == name {
				members[i].value = bytes.Clone(raw)
				replaced = true
				break
			}
		}
		if !replaced {
			members = append(members, usageMember{name: name, value: bytes.Clone(raw)})
		}
	}
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return members, nil
}

func (members usageMembers) get(name string) jsontext.Value {
	for _, member := range members {
		if member.name == name {
			return member.value
		}
	}
	return nil
}

func usageString(value jsontext.Value) (string, bool) {
	if value.Kind() != '"' {
		return "", false
	}
	var text string
	err := json.Unmarshal(value, &text)
	return text, err == nil
}

func usageBool(value jsontext.Value) (bool, bool) {
	switch value.Kind() {
	case 't':
		return true, true
	case 'f':
		return false, true
	default:
		return false, false
	}
}

func usageInteger(value jsontext.Value) (int64, bool) {
	if value.Kind() != '0' || bytes.ContainsAny(value, ".eE") {
		return 0, false
	}
	number, err := strconv.ParseInt(string(value), 10, 64)
	return number, err == nil
}
