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
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
)

func TestProfileOfSendsTheBearerAndParsesTheDocument(t *testing.T) {
	fixture, err := os.ReadFile("testdata/oauth/profile-response.json")
	if err != nil {
		t.Fatalf("reading the profile fixture: %v", err)
	}

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if got, want := r.URL.Path, "/api/oauth/profile"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		for header, want := range map[string]string{
			"Authorization": "Bearer sk-ant-oat01-stored-access",
			"Accept":        "application/json",
			"Cache-Control": "no-cache",
			"User-Agent":    testUserAgent,
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("header %s = %q, want %q", header, got, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	profile, err := oauthClientFor(t, server.URL).ProfileOf(t.Context(), storedCredentials(t))
	if err != nil {
		t.Fatalf("ProfileOf() = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the endpoint was contacted %d times, want 1", got)
	}

	if want := "11111111-1111-4111-8111-111111111111"; profile.AccountUUID != want {
		t.Errorf("AccountUUID = %q, want %q", profile.AccountUUID, want)
	}
	if want := "user@example.com"; profile.Email != want {
		t.Errorf("Email = %q, want %q", profile.Email, want)
	}
	if want := "22222222-2222-4222-8222-222222222222"; profile.OrganizationUUID != want {
		t.Errorf("OrganizationUUID = %q, want %q", profile.OrganizationUUID, want)
	}
	if name, ok := profile.OrganizationName(); !ok || name != "Example Org" {
		t.Errorf("OrganizationName() = %q, %t, want Example Org", name, ok)
	}
	if plan, ok := PlanOf(profile); !ok || plan != "max" {
		t.Errorf("PlanOf() = %q, %t, want max", plan, ok)
	}
	if tier, ok := RateLimitTierOf(profile); !ok || tier != "default_claude_max_20x" {
		t.Errorf("RateLimitTierOf() = %q, %t, want the fixture's tier", tier, ok)
	}

	identity := profile.Identity()
	if identity.AccountUUID != profile.AccountUUID {
		t.Errorf("Identity().AccountUUID = %q", identity.AccountUUID)
	}
	if identity.OrganizationUUID == nil || *identity.OrganizationUUID != profile.OrganizationUUID {
		t.Errorf("Identity().OrganizationUUID = %v", identity.OrganizationUUID)
	}
	if identity.Email != nil || identity.OrgName != nil {
		t.Error("Identity() must copy no personal data")
	}
}

func TestProfileOfClassifiesEveryFailure(t *testing.T) {
	tests := map[string]struct {
		status    int
		body      string
		precancel bool
		wantCalls int64
		check     func(t *testing.T, err error)
	}{
		"error: a rejected token keeps its status": {
			status:    http.StatusUnauthorized,
			body:      `{"error":{"type":"authentication_error"}}`,
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				httpErr, ok := errors.AsType[*errs.HTTPError](err)
				if !ok || httpErr.Status != http.StatusUnauthorized {
					t.Errorf("err = %v, want HTTPError{Status: 401}", err)
				}
			},
		},
		"error: a server failure carries its redacted body": {
			status:    http.StatusServiceUnavailable,
			body:      "token sk-ant-oat01-echo down",
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				httpErr, ok := errors.AsType[*errs.HTTPError](err)
				if !ok || httpErr.Status != http.StatusServiceUnavailable {
					t.Errorf("err = %v, want HTTPError{Status: 503}", err)
				}
				if !strings.Contains(err.Error(), "token <redacted> down") {
					t.Errorf("err = %q, want the token run replaced", err)
				}
			},
		},
		"error: a non-JSON body is a parse failure with a fixed sentence": {
			status:    http.StatusOK,
			body:      "<html>not json</html>",
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchParse {
					t.Errorf("err = %v, want a parse FetchError", err)
				}
				if !strings.Contains(err.Error(), "it is not a JSON document") {
					t.Errorf("err = %q, want the fixed sentence", err)
				}
				if strings.Contains(err.Error(), "html") {
					t.Errorf("err = %q, the document leaked into the message", err)
				}
			},
		},
		"error: a document missing a member names the member and never the values": {
			status:    http.StatusOK,
			body:      `{"account":{"email":"leak@example.com"},"organization":{"uuid":"org-1"}}`,
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchParse {
					t.Errorf("err = %v, want a parse FetchError", err)
				}
				if !strings.Contains(err.Error(), "account.uuid") {
					t.Errorf("err = %q, want the member named", err)
				}
				if strings.Contains(err.Error(), "leak@example.com") {
					t.Errorf("err = %q, the email leaked into the message", err)
				}
			},
		},
		"error: a cancelled run never contacts the endpoint": {
			precancel: true,
			wantCalls: 0,
			check: func(t *testing.T, err error) {
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchCancelled {
					t.Errorf("err = %v, want a cancelled FetchError", err)
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			ctx := t.Context()
			if tt.precancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			_, err := oauthClientFor(t, server.URL).ProfileOf(ctx, storedCredentials(t))
			if err == nil {
				t.Fatal("ProfileOf() = nil, want an error")
			}
			tt.check(t, err)
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("the endpoint was contacted %d times, want %d", got, tt.wantCalls)
			}
			if message := err.Error(); strings.Contains(message, "sk-ant") {
				t.Errorf("the error leaked token material: %q", message)
			}
		})
	}
}

func TestParseProfileHoldsTheDocumentToTheSchema(t *testing.T) {
	tests := map[string]struct {
		document   string
		wantEmail  string
		wantMember string
	}{
		"success: the profile spelling is read": {
			document:  `{"account":{"uuid":"acct-1","email":"v14@example.com"},"organization":{"uuid":"org-1"}}`,
			wantEmail: "v14@example.com",
		},
		"success: the exchange spelling is the fallback": {
			document:  `{"account":{"uuid":"acct-1","email_address":"exchange@example.com"},"organization":{"uuid":"org-1"}}`,
			wantEmail: "exchange@example.com",
		},
		"success: the profile spelling wins when both are present": {
			document:  `{"account":{"uuid":"acct-1","email":"v14@example.com","email_address":"old@example.com"},"organization":{"uuid":"org-1"}}`,
			wantEmail: "v14@example.com",
		},
		"error: no account uuid": {
			document:   `{"account":{"email":"e@x"},"organization":{"uuid":"o"}}`,
			wantMember: "account.uuid",
		},
		"error: an empty account uuid": {
			document:   `{"account":{"uuid":"","email":"e@x"},"organization":{"uuid":"o"}}`,
			wantMember: "account.uuid",
		},
		"error: no email of either spelling": {
			document:   `{"account":{"uuid":"a"},"organization":{"uuid":"o"}}`,
			wantMember: "account.email",
		},
		"error: no organization": {
			document:   `{"account":{"uuid":"a","email":"e@x"}}`,
			wantMember: "organization.uuid",
		},
		"error: a flat top-level uuid names nobody": {
			document:   `{"uuid":"a","email":"e@x"}`,
			wantMember: "account.uuid",
		},
		"error: a non-object document names nobody": {
			document:   `[1,2,3]`,
			wantMember: "account.uuid",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			profile, err := ParseProfile(jsontext.Value(tt.document))
			if tt.wantMember != "" {
				parseErr, ok := errors.AsType[*ProfileParseError](err)
				if !ok {
					t.Fatalf("err = %v, want a ProfileParseError", err)
				}
				if parseErr.Member != tt.wantMember {
					t.Errorf("Member = %q, want %q", parseErr.Member, tt.wantMember)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProfile() = %v", err)
			}
			if profile.Email != tt.wantEmail {
				t.Errorf("Email = %q, want %q", profile.Email, tt.wantEmail)
			}
			if !bytes.Equal(profile.Document, []byte(tt.document)) {
				t.Error("the document must be kept untouched")
			}
		})
	}
}

func TestProfileRendersRedactedEverywhere(t *testing.T) {
	profile, err := ParseProfile(jsontext.Value(`{"account":{"uuid":"acct-1","email":"v14@example.com","display_name":"Someone"},"organization":{"uuid":"org-1","name":"Org"}}`))
	if err != nil {
		t.Fatalf("ParseProfile() = %v", err)
	}

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.Info("profile", slog.Any("profile", profile))

	for name, rendered := range map[string]string{
		"%v":   fmt.Sprintf("%v", profile),
		"%+v":  fmt.Sprintf("%+v", profile),
		"%#v":  fmt.Sprintf("%#v", profile),
		"%s":   fmt.Sprintf("%s", profile),
		"slog": logged.String(),
	} {
		if strings.Contains(rendered, "v14@example.com") || strings.Contains(rendered, "Someone") {
			t.Errorf("%s leaked personal data: %s", name, rendered)
		}
		if !strings.Contains(rendered, "acct-1") || !strings.Contains(rendered, "org-1") {
			t.Errorf("%s must keep the two ids: %s", name, rendered)
		}
	}
}

func TestPlanOfMapsOnlyTheFourKnownOrganizationTypes(t *testing.T) {
	tests := map[string]struct {
		organizationType string
		wantPlan         string
		wantOK           bool
	}{
		"success: claude_max is max":               {organizationType: "claude_max", wantPlan: "max", wantOK: true},
		"success: claude_pro is pro":               {organizationType: "claude_pro", wantPlan: "pro", wantOK: true},
		"success: claude_enterprise is enterprise": {organizationType: "claude_enterprise", wantPlan: "enterprise", wantOK: true},
		"success: claude_team is team":             {organizationType: "claude_team", wantPlan: "team", wantOK: true},
		"error: an unknown type maps to nothing":   {organizationType: "claude_free", wantOK: false},
		"error: case is never folded":              {organizationType: "Claude_Max", wantOK: false},
		"error: an absent type maps to nothing":    {organizationType: "", wantOK: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			document := `{"account":{"uuid":"a","email":"e@x"},"organization":{"uuid":"o"}}`
			if tt.organizationType != "" {
				document = fmt.Sprintf(`{"account":{"uuid":"a","email":"e@x"},"organization":{"uuid":"o","organization_type":%q}}`, tt.organizationType)
			}
			profile, err := ParseProfile(jsontext.Value(document))
			if err != nil {
				t.Fatalf("ParseProfile() = %v", err)
			}
			plan, ok := PlanOf(profile)
			if ok != tt.wantOK || plan != tt.wantPlan {
				t.Errorf("PlanOf() = %q, %t, want %q, %t", plan, ok, tt.wantPlan, tt.wantOK)
			}
		})
	}
}

func TestRateLimitTierOfAdmitsOnlyPlanWords(t *testing.T) {
	tests := map[string]struct {
		tier   string
		wantOK bool
	}{
		"success: a plain word is kept":             {tier: "default", wantOK: true},
		"success: digits and underscores are kept":  {tier: "default_claude_max_20x", wantOK: true},
		"success: 64 bytes is the last admitted":    {tier: "a" + strings.Repeat("b", 63), wantOK: true},
		"error: 65 bytes is refused":                {tier: "a" + strings.Repeat("b", 64), wantOK: false},
		"error: an uppercase letter is refused":     {tier: "Default", wantOK: false},
		"error: a leading digit is refused":         {tier: "9lives", wantOK: false},
		"error: an interior space is refused":       {tier: "two words", wantOK: false},
		"error: an empty tier is nothing to record": {tier: "", wantOK: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			document := fmt.Sprintf(`{"account":{"uuid":"a","email":"e@x"},"organization":{"uuid":"o","rate_limit_tier":%q}}`, tt.tier)
			profile, err := ParseProfile(jsontext.Value(document))
			if err != nil {
				t.Fatalf("ParseProfile() = %v", err)
			}
			tier, ok := RateLimitTierOf(profile)
			if ok != tt.wantOK {
				t.Errorf("RateLimitTierOf() = %q, %t, want ok=%t", tier, ok, tt.wantOK)
			}
			if tt.wantOK && tier != tt.tier {
				t.Errorf("RateLimitTierOf() = %q, want %q verbatim", tier, tt.tier)
			}
		})
	}
}

func TestFillPlanReplacesOnAnAnswerAndKeepsOnNone(t *testing.T) {
	stored := func(t *testing.T, subscription, tier string) *Credentials {
		t.Helper()
		credentials := storedCredentials(t)
		if subscription != "" {
			credentials.SubscriptionType = &subscription
		}
		if tier != "" {
			credentials.RateLimitTier = &tier
		}
		return credentials
	}
	profileFor := func(t *testing.T, organization string) *Profile {
		t.Helper()
		profile, err := ParseProfile(jsontext.Value(`{"account":{"uuid":"a","email":"e@x"},"organization":` + organization + `}`))
		if err != nil {
			t.Fatalf("ParseProfile() = %v", err)
		}
		return profile
	}
	deref := func(value *string) string {
		if value == nil {
			return "<nil>"
		}
		return *value
	}

	tests := map[string]struct {
		subscription     string
		tier             string
		organization     string
		wantSubscription string
		wantTier         string
	}{
		"success: both halves are filled from the profile": {
			organization:     `{"uuid":"o","organization_type":"claude_pro","rate_limit_tier":"default"}`,
			wantSubscription: "pro",
			wantTier:         "default",
		},
		"success: an answer replaces what is stored": {
			subscription:     "max",
			tier:             "old_tier",
			organization:     `{"uuid":"o","organization_type":"claude_team","rate_limit_tier":"new_tier"}`,
			wantSubscription: "team",
			wantTier:         "new_tier",
		},
		"success: an unmapped type never clears a stored plan": {
			subscription:     "max",
			tier:             "stored",
			organization:     `{"uuid":"o","organization_type":"claude_free","rate_limit_tier":"UPPER"}`,
			wantSubscription: "max",
			wantTier:         "stored",
		},
		"success: an absent organization block keeps everything": {
			subscription:     "pro",
			organization:     `{"uuid":"o"}`,
			wantSubscription: "pro",
			wantTier:         "<nil>",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			credentials := stored(t, tt.subscription, tt.tier)
			before := NeedsPlan(credentials)
			FillPlan(credentials, profileFor(t, tt.organization))
			if got := deref(credentials.SubscriptionType); got != tt.wantSubscription {
				t.Errorf("SubscriptionType = %s, want %s", got, tt.wantSubscription)
			}
			if got := deref(credentials.RateLimitTier); got != tt.wantTier {
				t.Errorf("RateLimitTier = %s, want %s", got, tt.wantTier)
			}
			if credentials.SubscriptionType != nil && credentials.RateLimitTier != nil && NeedsPlan(credentials) {
				t.Error("NeedsPlan must report false once both halves are stored")
			}
			_ = before
		})
	}
}

func TestNeedsPlanAsksUntilBothHalvesAreStored(t *testing.T) {
	credentials := storedCredentials(t)
	if !NeedsPlan(credentials) {
		t.Error("a credential with neither half must ask")
	}
	subscription := "max"
	credentials.SubscriptionType = &subscription
	if !NeedsPlan(credentials) {
		t.Error("a credential with half a plan must still ask")
	}
	tier := "default"
	credentials.RateLimitTier = &tier
	if NeedsPlan(credentials) {
		t.Error("a credential with both halves must never ask again")
	}
}
