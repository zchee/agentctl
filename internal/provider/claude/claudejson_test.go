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
	"bytes"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// assertBytes fails with the first differing offset and a hex window on both
// sides of it, so a one-byte drift in a large document is locatable at a
// glance.
func assertBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	at := 0
	for at < min(len(got), len(want)) && got[at] == want[at] {
		at++
	}
	window := func(b []byte) string {
		lo := max(at-64, 0)
		hi := min(at+64, len(b))
		return hex.Dump(b[lo:hi])
	}
	t.Fatalf("first difference at byte %d (lengths %d vs %d)\n--- got, window at %d ---\n%s--- want, window at %d ---\n%s--- diff (-want +got) ---\n%s",
		at, len(got), len(want), at, window(got), at, window(want), gocmp.Diff(string(want), string(got)))
}

// chunkOf is one top-level member exactly as the canonical printer writes it.
func chunkOf(key, value string) string {
	return `  "` + key + `": ` + value
}

// doc is a top-level object assembled from its members.
func doc(chunks ...string) string {
	return "{\n" + strings.Join(chunks, ",\n") + "\n}"
}

// pValue is the previous account's member value as a login leaves it: twenty
// keys, six of which the start-up refresh never writes.
func pValue() string {
	return strings.Join([]string{
		"{",
		`    "accountUuid": "acct-p",`,
		`    "emailAddress": "p@example.com",`,
		`    "organizationUuid": "org-p",`,
		`    "hasExtraUsageEnabled": true,`,
		`    "billingType": "stripe_subscription",`,
		`    "accountCreatedAt": "2025-01-01T00:00:00Z",`,
		`    "subscriptionCreatedAt": "2025-02-01T00:00:00Z",`,
		`    "ccOnboardingFlags": {`,
		`      "seen": true`,
		"    },",
		`    "claudeCodeTrialEndsAt": null,`,
		`    "claudeCodeTrialDurationDays": null,`,
		`    "seatTier": "premium",`,
		`    "displayName": "Pee",`,
		`    "fullName": "P Person",`,
		`    "profileFetchedAt": 1780000000000,`,
		`    "organizationRole": "admin",`,
		`    "workspaceRole": "developer",`,
		`    "organizationName": "P Org",`,
		`    "organizationType": "claude_max",`,
		`    "organizationRateLimitTier": "default_claude_max_20x",`,
		`    "userRateLimitTier": "default"`,
		"  }",
	}, "\n")
}

// tAccount is the incoming account's replacement value, handed over compact
// the way a caller would encode it.
func tAccount() jsontext.Value {
	return jsontext.Value(`{"accountUuid":"acct-t","emailAddress":"t@example.com","organizationUuid":"org-t","hasExtraUsageEnabled":false,"billingType":"stripe_subscription","accountCreatedAt":"2026-01-01T00:00:00Z","ccOnboardingFlags":{},"claudeCodeTrialEndsAt":null,"claudeCodeTrialDurationDays":null,"seatTier":null,"displayName":"Tee","profileFetchedAt":1789000000000}`)
}

// tChunk is tAccount's member as the canonical printer writes it at the top
// level, spelled by hand so the expected side never goes through the code
// under test.
func tChunk() string {
	return strings.Join([]string{
		`  "oauthAccount": {`,
		`    "accountUuid": "acct-t",`,
		`    "emailAddress": "t@example.com",`,
		`    "organizationUuid": "org-t",`,
		`    "hasExtraUsageEnabled": false,`,
		`    "billingType": "stripe_subscription",`,
		`    "accountCreatedAt": "2026-01-01T00:00:00Z",`,
		`    "ccOnboardingFlags": {},`,
		`    "claudeCodeTrialEndsAt": null,`,
		`    "claudeCodeTrialDurationDays": null,`,
		`    "seatTier": null,`,
		`    "displayName": "Tee",`,
		`    "profileFetchedAt": 1789000000000`,
		"  }",
	}, "\n")
}

// smallPair is a small document holding the previous account and one stale
// cache, and what the rewrite must turn it into.
func smallPair() (string, string) {
	before := doc(
		chunkOf("numStartups", "12"),
		chunkOf("oauthAccount", pValue()),
		chunkOf("modelAccessCache", "{\n    \"a\": true\n  }"),
		chunkOf("userID", `"u-1"`),
	)
	after := doc(chunkOf("numStartups", "12"), tChunk(), chunkOf("userID", `"u-1"`))
	return before, after
}

// fillerValue is one synthetic member value per shape class: nested objects,
// arrays, empty containers, literal Japanese and emoji, escapes, 13-digit
// timestamps, floats, negatives, null and booleans.
func fillerValue(i int) string {
	stamp := 1_789_000_000_000 + int64(i)
	switch i % 12 {
	case 0:
		return strings.Join([]string{
			"{",
			`    "enabled": true,`,
			`    "count": 3,`,
			`    "nested": {`,
			`      "list": [`,
			"        1,",
			`        "two",`,
			"        null",
			"      ],",
			`      "empty": {}`,
			"    }",
			"  }",
		}, "\n")
	case 1:
		return strings.Join([]string{
			"[",
			`    "alpha",`,
			"    -7,",
			"    {",
			`      "k": "v"`,
			"    },",
			"    []",
			"  ]",
		}, "\n")
	case 2:
		return "{}"
	case 3:
		return "[]"
	case 4:
		return fmt.Sprintf("\"日本語のテキスト %d 🦀✨\"", i)
	case 5:
		return `"line\nbreak\ttab \"quoted\" back\\slash"`
	case 6:
		return strconv.FormatInt(stamp, 10)
	case 7:
		return "0.5601675"
	case 8:
		return fmt.Sprintf("-%d", i+1)
	case 9:
		return "null"
	case 10:
		return "true"
	default:
		return "false"
	}
}

// bigPair is a 166-member document — 159 fillers, userID, the previous
// account mid-document, and the five caches at the first, middle and last
// positions — and what the rewrite must turn it into.
func bigPair(t *testing.T) (string, string) {
	t.Helper()
	var before, expected []string
	filler := 0
	for index := range 166 {
		var key, value string
		kind := byte('k')
		switch index {
		case 0:
			key, value, kind = "modelAccessCache", "{\n    \"claude-opus\": true\n  }", 'c'
		case 40:
			key, value, kind = "orgModelDefaultCache", `"claude-opus-5"`, 'c'
		case 83:
			key, value, kind = "oauthAccount", pValue(), 'o'
		case 84:
			key, value = "userID", `"0123456789abcdef"`
		case 100:
			key, value, kind = "cachedExtraUsageDisabledReason", "null", 'c'
		case 120:
			key, value, kind = "cachedUsageUtilization", "{\n    \"five_hour\": 0.25,\n    \"seven_day\": -0.5\n  }", 'c'
		case 165:
			key, value, kind = "passesEligibilityCache", "{\n    \"eligible\": false\n  }", 'c'
		default:
			filler++
			key, value = fmt.Sprintf("setting%03d", filler), fillerValue(filler)
		}
		text := chunkOf(key, value)
		before = append(before, text)
		switch kind {
		case 'k':
			expected = append(expected, text)
		case 'o':
			expected = append(expected, tChunk())
		}
	}
	if len(before) != 166 {
		t.Fatalf("the synthetic fixture holds %d members, not 166", len(before))
	}
	return doc(before...), doc(expected...)
}

// doubles are JavaScript's shortest spellings of accumulated-cost style
// doubles of 16 and 17 digits, which only exact float parsing carries over.
var doubles = []string{
	"0.30000000000000004",
	"0.18813200000000002",
	"90.28571428571429",
	"0.45202095760750516",
	"0.013142054551181559",
	"7.744551763382691",
	"2.3333333333333335",
	"8.024999999999999",
}

// doublesPair is a document whose projects member carries every double, and
// its rewritten form: each number's bytes survive untouched.
func doublesPair() (string, string) {
	costs := make([]string, len(doubles))
	for i, number := range doubles {
		costs[i] = "  " + chunkOf(fmt.Sprintf("lastCost%d", i), number)
	}
	projects := "{\n" + strings.Join(costs, ",\n") + "\n  }"
	before := doc(chunkOf("oauthAccount", pValue()), chunkOf("projects", projects))
	after := doc(tChunk(), chunkOf("projects", projects))
	return before, after
}

func TestPlan(t *testing.T) {
	t.Parallel()

	big, bigWant := bigPair(t)
	small, smallWant := smallPair()
	floats, floatsWant := doublesPair()
	tests := map[string]struct {
		document string
		account  jsontext.Value
		want     string
	}{
		"success: replaces the account, deletes the five caches, keeps every other byte": {
			document: big,
			account:  tAccount(),
			want:     bigWant,
		},
		"success: the previous account's extra members do not survive the replacement": {
			document: small,
			account:  tAccount(),
			want:     smallWant,
		},
		"success: shortest-form doubles survive byte for byte": {
			document: floats,
			account:  tAccount(),
			want:     floatsWant,
		},
		"success: the account keeps its place around a cache deleted before it": {
			document: doc(
				chunkOf("cachedUsageUtilization", "{}"),
				chunkOf("a", "1"),
				chunkOf("oauthAccount", pValue()),
				chunkOf("b", "2"),
				chunkOf("c", "3"),
			),
			account: tAccount(),
			want:    doc(chunkOf("a", "1"), tChunk(), chunkOf("b", "2"), chunkOf("c", "3")),
		},
		"success: an absent account is appended last": {
			document: doc(chunkOf("a", "1"), chunkOf("b", "2")),
			account:  tAccount(),
			want:     doc(chunkOf("a", "1"), chunkOf("b", "2"), tChunk()),
		},
		"success: an empty object gains the account": {
			document: "{}",
			account:  tAccount(),
			want:     doc(tChunk()),
		},
		"success: a replacement with nested members is encoded at the member's depth": {
			document: doc(chunkOf("a", "1")),
			account:  jsontext.Value("{\"accountUuid\":\"acct-x\",\"ccOnboardingFlags\":{\"seen\":true,\"ratio\":0.25},\"note\":\"caf\\u00e9 \\ud83e\\udd80\"}"),
			want: doc(chunkOf("a", "1"), strings.Join([]string{
				`  "oauthAccount": {`,
				`    "accountUuid": "acct-x",`,
				`    "ccOnboardingFlags": {`,
				`      "seen": true,`,
				`      "ratio": 0.25`,
				"    },",
				`    "note": "café 🦀"`,
				"  }",
			}, "\n")),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := []byte(tt.document)
			rewrite, err := Plan(document, tt.account)
			if err != nil {
				t.Fatalf("Plan refused: %v", err)
			}
			if !rewrite.Changed() {
				t.Fatalf("the rewrite reports no change")
			}
			assertBytes(t, rewrite.Updated(), []byte(tt.want))
			if bytes.HasSuffix(rewrite.Updated(), []byte("\n")) {
				t.Fatalf("the rewritten document ends with a newline")
			}
			applied, err := rewrite.Apply(slices.Clone(document))
			if err != nil {
				t.Fatalf("Apply refused identical bytes: %v", err)
			}
			assertBytes(t, applied, []byte(tt.want))
		})
	}
}

func TestPlanReplaceNeverMerges(t *testing.T) {
	t.Parallel()
	before, _ := smallPair()
	rewrite, err := Plan([]byte(before), tAccount())
	if err != nil {
		t.Fatalf("Plan refused: %v", err)
	}
	for _, extra := range []string{
		"organizationRole",
		"workspaceRole",
		"organizationName",
		"organizationType",
		"organizationRateLimitTier",
		"userRateLimitTier",
		"subscriptionCreatedAt",
		"fullName",
	} {
		if bytes.Contains(rewrite.Updated(), []byte(`"`+extra+`"`)) {
			t.Errorf("the previous account's %q survived the replacement", extra)
		}
	}
}

func TestPlanUnchanged(t *testing.T) {
	t.Parallel()
	document := []byte(doc(chunkOf("numStartups", "12"), tChunk(), chunkOf("userID", `"u-1"`)))
	rewrite, err := Plan(document, tAccount())
	if err != nil {
		t.Fatalf("Plan refused: %v", err)
	}
	if rewrite.Changed() {
		t.Fatalf("an identical account with no caches still reports a change")
	}
	assertBytes(t, rewrite.Updated(), document)
	if &rewrite.Updated()[0] != &document[0] {
		t.Fatalf("an unchanged document was re-rendered instead of returned as the original slice")
	}
}

func TestApplyRefusesChangedBytes(t *testing.T) {
	t.Parallel()
	before, want := smallPair()
	rewrite, err := Plan([]byte(before), tAccount())
	if err != nil {
		t.Fatalf("Plan refused: %v", err)
	}

	tests := map[string]struct {
		current []byte
		wantErr error
	}{
		"success: identical bytes in a fresh backing array are accepted": {
			current: []byte(before),
		},
		"error: a document that changed after planning is refused": {
			current: []byte(before + "\n"),
			wantErr: ErrChanged,
		},
		"error: an emptied document is refused": {
			current: nil,
			wantErr: ErrChanged,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			applied, err := rewrite.Apply(tt.current)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Apply returned %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply refused: %v", err)
			}
			assertBytes(t, applied, []byte(want))
		})
	}
}

func TestPlanRefusesInvalidReplacement(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		account jsontext.Value
	}{
		"error: a truncated replacement value is refused":     {account: jsontext.Value(`{"a":`)},
		"error: two replacement values in one are refused":    {account: jsontext.Value(`{} {}`)},
		"error: an empty replacement value is refused":        {account: jsontext.Value("")},
		"error: a lone surrogate in a replacement is refused": {account: jsontext.Value(`"\ud800"`)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := []byte(doc(chunkOf("a", "1")))
			if _, err := Plan(document, tt.account); !errors.Is(err, ErrUnparseable) {
				t.Fatalf("Plan returned %v, want %v", err, ErrUnparseable)
			}
		})
	}
}

func TestRefusalsNeverQuoteTheDocument(t *testing.T) {
	t.Parallel()
	const marker = "sk-ant-oat01-SECRET-MARKER"
	tests := map[string]struct {
		document string
	}{
		"error: a truncated document's refusal carries no value": {
			document: `{"token": "` + marker + `", "x": `,
		},
		"error: a compact document's refusal carries no value": {
			document: `{"token":"` + marker + `"}`,
		},
		"error: a duplicated member's refusal carries no value": {
			document: "{\n  \"token\": \"" + marker + "\",\n  \"token\": \"" + marker + "\"\n}",
		},
		"error: a lone surrogate's refusal carries no value": {
			document: "{\n  \"token\": \"" + marker + "\\ud800\"\n}",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := Reproduce([]byte(tt.document))
			if err == nil {
				t.Fatalf("the document was not refused")
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "token") {
				t.Fatalf("the refusal quotes the document: %v", err)
			}
			if _, planErr := Plan([]byte(tt.document), tAccount()); planErr == nil {
				t.Fatalf("the plan was not refused")
			} else if strings.Contains(planErr.Error(), marker) {
				t.Fatalf("the plan's refusal quotes the document: %v", planErr)
			}
		})
	}
}

func TestReproduceCorpus(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		file    string
		wantErr error
	}{
		"success: the shape the configuration writer produces": {file: "base.json"},
		"error: a trailing newline is not reproducible":        {file: "trailing-newline.json", wantErr: ErrNotReproducible},
		"error: CRLF line endings are not reproducible":        {file: "crlf.json", wantErr: ErrNotReproducible},
		"error: four-space indentation is not reproducible":    {file: "four-space-indent.json", wantErr: ErrNotReproducible},
		"error: an escaped e-acute is not reproducible":        {file: "escaped-e-acute.json", wantErr: ErrNotReproducible},
		"error: an escaped solidus is not reproducible":        {file: "escaped-solidus.json", wantErr: ErrNotReproducible},
		"error: an exponent without its sign is not reproducible": {
			file: "exponent-without-sign.json", wantErr: ErrNotReproducible,
		},
		"error: a small decimal the printer spells scientifically is not reproducible": {
			file: "small-decimal.json", wantErr: ErrNotReproducible,
		},
		"error: a duplicate member name is not reproducible": {file: "duplicate-key.json", wantErr: ErrNotReproducible},
		"error: a lone surrogate does not parse":             {file: "lone-surrogate.json", wantErr: ErrUnparseable},
		"error: a compact document is not reproducible":      {file: "compact.json", wantErr: ErrNotReproducible},
		"error: a byte order mark does not parse":            {file: "bom.json", wantErr: ErrUnparseable},
		"error: a top-level array is not an object":          {file: "top-level-array.json", wantErr: ErrNotAnObject},
		"error: an empty file does not parse":                {file: "empty.json", wantErr: ErrUnparseable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document, err := os.ReadFile("testdata/" + tt.file)
			if err != nil {
				t.Fatalf("the corpus file: %v", err)
			}
			if err := Reproduce(document); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reproduce(%s) returned %v, want %v", tt.file, err, tt.wantErr)
			}
			if _, err := Plan(document, tAccount()); tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Plan(%s) refused a reproducible document: %v", tt.file, err)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Plan(%s) returned %v, want %v", tt.file, err, tt.wantErr)
			}
		})
	}
}

func TestReproduceNumbers(t *testing.T) {
	t.Parallel()
	reproduced := []string{
		"0", "17", "-17", "1789000000000", "9007199254740993",
		"18446744073709551615", "-9223372036854775808",
		"1.0", "0.0", "-0.0", "100.0", "0.5", "123.456", "3.141592653589793",
		"1000000000000000.0", "1234567890123456.8", "1.2345678901234568e+16",
		"1e+16", "1e+21", "1e+100", "1.8446744073709552e+19", "-9.223372036854776e+18",
		"0.0001", "0.00001", "1e-6", "1e-7", "2.5e-10",
		"5e-324", "1.7976931348623157e+308",
		"0.5601675", "-0.5", "0.25",
	}
	reproduced = append(reproduced, doubles...)
	refused := []string{
		"-0", "1e2", "1E5", "1e15", "1e16", "1e21", "0.000001", "10.00",
		"18446744073709551616", "-9223372036854775809", "1e-999",
	}
	unparseable := []string{"1e999", "-1e999", "01"}

	type row struct {
		number  string
		wantErr error
	}
	tests := make(map[string]row, len(reproduced)+len(refused)+len(unparseable))
	for _, number := range reproduced {
		tests["success: "+number+" is reproduced and carried over"] = row{number: number}
	}
	for _, number := range refused {
		tests["error: "+number+" is spelled another way canonically"] = row{number: number, wantErr: ErrNotReproducible}
	}
	for _, number := range unparseable {
		tests["error: "+number+" does not parse"] = row{number: number, wantErr: ErrUnparseable}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := []byte(doc(chunkOf("a", tt.number)))
			if err := Reproduce(document); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reproduce of %s returned %v, want %v", tt.number, err, tt.wantErr)
			}
		})
	}
}

func TestReproduceStrings(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		value   string
		wantErr error
	}{
		"success: raw non-ASCII text stays raw":          {value: "\"café 日本語 🦀\""},
		"success: the two-letter escapes":                {value: `"a\b\t\n\f\r\"\\z"`},
		"success: a lower-case control escape":           {value: `"\u001b"`},
		"success: a raw line separator stays raw":        {value: "\"  \""},
		"error: an upper-case control escape":            {value: `"\u001B"`, wantErr: ErrNotReproducible},
		"error: a control with a two-letter spelling":    {value: `"\u0008"`, wantErr: ErrNotReproducible},
		"error: a raw control byte inside a string":      {value: "\"a\tb\"", wantErr: ErrUnparseable},
		"error: an escaped non-ASCII character":          {value: "\"\\u00e9\"", wantErr: ErrNotReproducible},
		"error: a surrogate pair spelled as escapes":     {value: "\"\\ud83e\\udd80\"", wantErr: ErrNotReproducible},
		"error: invalid UTF-8 inside a string":           {value: "\"\xff\"", wantErr: ErrUnparseable},
		"error: data after the document's closing brace": {value: `"x"` + "\n}", wantErr: ErrUnparseable},
		"error: a second document after the first":       {value: `"x"` + "\n}\n{", wantErr: ErrUnparseable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := []byte(doc(chunkOf("a", tt.value)))
			if err := Reproduce(document); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reproduce of %s returned %v, want %v", tt.value, err, tt.wantErr)
			}
		})
	}
}
