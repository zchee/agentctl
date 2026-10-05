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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/secret"
)

// fixtureBlob reads a credential fixture from testdata/credentials.
func fixtureBlob(t *testing.T, name string) []byte {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "credentials", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return blob
}

// parseFixture parses a credential fixture.
func parseFixture(t *testing.T, name string) *Credentials {
	t.Helper()
	credentials, err := ParseBlob(fixtureBlob(t, name))
	if err != nil {
		t.Fatalf("fixture %s should parse: %v", name, err)
	}
	return credentials
}

// mustSecret seals text for a test.
func mustSecret(t *testing.T, text string) *secret.Secret {
	t.Helper()
	sealed, err := secret.NewSecret([]byte(text))
	if err != nil {
		t.Fatalf("sealing %q: %v", text, err)
	}
	return sealed
}

// mustDigests returns the credential's digests.
func mustDigests(t *testing.T, c *Credentials) Digests {
	t.Helper()
	digests, err := c.Digests()
	if err != nil {
		t.Fatalf("digests: %v", err)
	}
	return digests
}

// mustBlobJSON serializes the credential document.
func mustBlobJSON(t *testing.T, c *Credentials) []byte {
	t.Helper()
	blob, err := c.BlobJSON()
	if err != nil {
		t.Fatalf("blob JSON: %v", err)
	}
	return blob
}

// tokenResponse builds the response the merge vectors share.
func tokenResponse(t *testing.T, expiresIn int64, refresh string) *TokenResponse {
	t.Helper()
	bearer := "Bearer"
	response := &TokenResponse{
		AccessToken: mustSecret(t, "new-access"),
		ExpiresIn:   expiresIn,
		TokenType:   &bearer,
	}
	if refresh != "" {
		response.RefreshToken = mustSecret(t, refresh)
	}
	return response
}

func TestParseBlobReadsAnOldBlobWithoutTheNewerFields(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-old-blob.json")
	if credentials.ExpiresAtMillis != 1_756_000_000_000 {
		t.Fatalf("expiresAt = %d", credentials.ExpiresAtMillis)
	}
	if credentials.RefreshTokenExpiresAtMillis != nil {
		t.Fatalf("an old blob has no refresh expiry, got %d", *credentials.RefreshTokenExpiresAtMillis)
	}
	if credentials.ClientID != nil || credentials.TokenAccount != nil {
		t.Fatal("an old blob has no clientId and no tokenAccount")
	}
	if diff := gocmp.Diff([]string{"user:inference", "user:profile"}, credentials.Scopes); diff != "" {
		t.Fatalf("scopes mismatch (-want +got):\n%s", diff)
	}
	if credentials.SubscriptionType == nil || *credentials.SubscriptionType != "max" {
		t.Fatalf("subscriptionType = %v", credentials.SubscriptionType)
	}
	if credentials.RefreshToken == nil {
		t.Fatal("the old blob carries a refresh token")
	}
	// An old blob carries no identity, which is what makes an unknown
	// identity a state a real account can be in.
	if identity := credentials.Identity(); identity != nil {
		t.Fatalf("an old blob must have no identity, got %+v", identity)
	}
}

func TestParseBlobReadsANewBlobIncludingTheIdentityBlock(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	if credentials.ClientID == nil || *credentials.ClientID != ClientID {
		t.Fatalf("clientId = %v", credentials.ClientID)
	}
	if diff := gocmp.Diff(DefaultScopes, credentials.Scopes); diff != "" {
		t.Fatalf("scopes mismatch (-want +got):\n%s", diff)
	}
	if credentials.RefreshTokenExpiresAtMillis == nil || *credentials.RefreshTokenExpiresAtMillis != 1_759_900_000_000 {
		t.Fatalf("refreshTokenExpiresAt = %v", credentials.RefreshTokenExpiresAtMillis)
	}

	identity := credentials.Identity()
	if identity == nil {
		t.Fatal("a new blob names its account")
	}
	want := &Identity{
		AccountUUID:      "11111111-1111-4111-8111-111111111111",
		OrganizationUUID: new("22222222-2222-4222-8222-222222222222"),
		Email:            new("user@example.com"),
		OrgName:          new("Example Org"),
	}
	if diff := gocmp.Diff(want, identity); diff != "" {
		t.Fatalf("identity mismatch (-want +got):\n%s", diff)
	}
}

func TestParseBlobRejectsABlobMissingARequiredField(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		blob string
		want func(error) bool
	}{
		"error: not json at all": {
			blob: "{",
			want: func(err error) bool { return strings.Contains(err.Error(), "not valid JSON") },
		},
		"error: no root object": {
			blob: `{"other": {}}`,
			want: func(err error) bool { return errors.Is(err, ErrMissingRoot) },
		},
		"error: a non-object root member": {
			blob: `{"claudeAiOauth": 7}`,
			want: func(err error) bool { return errors.Is(err, ErrMissingRoot) },
		},
		"error: no access token": {
			blob: `{"claudeAiOauth": {"expiresAt": 1}}`,
			want: func(err error) bool {
				fieldErr, ok := errors.AsType[*MissingFieldError](err)
				return ok && fieldErr.Field == "accessToken"
			},
		},
		"error: no expiry": {
			blob: `{"claudeAiOauth": {"accessToken": "a"}}`,
			want: func(err error) bool {
				fieldErr, ok := errors.AsType[*MissingFieldError](err)
				return ok && fieldErr.Field == "expiresAt"
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseBlob([]byte(tt.blob))
			if err == nil {
				t.Fatal("the blob must not parse")
			}
			if !tt.want(err) {
				t.Fatalf("wrong failure: %v", err)
			}
		})
	}
}

func TestBlobJSONRoundTripsUnknownKeys(t *testing.T) {
	t.Parallel()

	// profile is a real field this build does not model, and the exchange
	// response recently grew tokenUuid. Losing either on a refresh would
	// silently degrade the session that reads the file next.
	blob := `{"claudeAiOauth":{"accessToken":"a","expiresAt":5,"scopes":[],"profile":{"x":1},"tokenUuid":"u"}}`
	credentials, err := ParseBlob([]byte(blob))
	if err != nil {
		t.Fatalf("the blob should parse: %v", err)
	}
	if len(credentials.Extra) != 2 {
		t.Fatalf("unknown keys are kept, got %v", credentials.Extra)
	}

	var written struct {
		Root struct {
			AccessToken  string         `json:"accessToken"`
			RefreshToken *string        `json:"refreshToken"`
			Profile      map[string]int `json:"profile"`
			TokenUUID    string         `json:"tokenUuid"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(mustBlobJSON(t, credentials), &written); err != nil {
		t.Fatalf("the output should be JSON: %v", err)
	}
	if diff := gocmp.Diff(map[string]int{"x": 1}, written.Root.Profile); diff != "" {
		t.Fatalf("profile mismatch (-want +got):\n%s", diff)
	}
	if written.Root.TokenUUID != "u" || written.Root.AccessToken != "a" {
		t.Fatalf("unknown or known members lost: %+v", written.Root)
	}
	if written.Root.RefreshToken != nil {
		t.Fatal("an absent optional is omitted, not null")
	}
	if bytes.Contains(mustBlobJSON(t, credentials), []byte("refreshToken")) {
		t.Fatal("an absent optional must not appear at all")
	}
}

func TestBlobJSONWritesTheFixtureInItsAuthorsShape(t *testing.T) {
	t.Parallel()

	// The exact compact document the blob's author would write for the new
	// fixture: known members in their fixed order, then the unrecognised
	// profile member, all on one line.
	credentials := parseFixture(t, "credentials-new-blob.json")
	want := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-FAKE-NEW-ACCESS-TOKEN-NOT-A-REAL-CREDENTIAL",` +
		`"refreshToken":"sk-ant-ort01-FAKE-NEW-REFRESH-TOKEN-NOT-A-REAL-CREDENTIAL",` +
		`"expiresAt":1757600000000,"refreshTokenExpiresAt":1759900000000,` +
		`"scopes":["user:file_upload","user:inference","user:mcp_servers","user:profile","user:sessions:claude_code"],` +
		`"subscriptionType":"max","rateLimitTier":"default_claude_max_20x",` +
		`"clientId":"9d1c250a-e61b-44d9-88ed-5944d1962f5e",` +
		`"tokenAccount":{"uuid":"11111111-1111-4111-8111-111111111111","emailAddress":"user@example.com",` +
		`"organizationUuid":"22222222-2222-4222-8222-222222222222","organizationName":"Example Org",` +
		`"workspaceId":null,"workspaceName":null},` +
		`"profile":{"display_name":"Example User","has_claude_max":true}}}`
	if diff := gocmp.Diff(want, string(mustBlobJSON(t, credentials))); diff != "" {
		t.Fatalf("blob mismatch (-want +got):\n%s", diff)
	}
}

func TestBlobJSONRoundTripsAFixtureFieldForField(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	reparsed, err := ParseBlob(mustBlobJSON(t, credentials))
	if err != nil {
		t.Fatalf("the output should reparse: %v", err)
	}

	if diff := gocmp.Diff(mustDigests(t, credentials), mustDigests(t, reparsed)); diff != "" {
		t.Fatalf("digests mismatch (-want +got):\n%s", diff)
	}
	if reparsed.ExpiresAtMillis != credentials.ExpiresAtMillis {
		t.Fatal("expiresAt changed")
	}
	if diff := gocmp.Diff(credentials.RefreshTokenExpiresAtMillis, reparsed.RefreshTokenExpiresAtMillis); diff != "" {
		t.Fatalf("refresh expiry mismatch (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(credentials.Scopes, reparsed.Scopes); diff != "" {
		t.Fatalf("scopes mismatch (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(credentials.TokenAccount, reparsed.TokenAccount); diff != "" {
		t.Fatalf("tokenAccount mismatch (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(credentials.ClientID, reparsed.ClientID); diff != "" {
		t.Fatalf("clientId mismatch (-want +got):\n%s", diff)
	}
	// The written form is compact, so the unrecognised members compare by
	// what they mean rather than by their original whitespace.
	compactExtras := gocmp.Transformer("compact", func(m ExtraMember) ExtraMember {
		value := jsontext.Value(bytes.Clone(m.Value))
		if err := value.Compact(); err != nil {
			t.Fatalf("compacting %s: %v", m.Name, err)
		}
		return ExtraMember{Name: m.Name, Value: value}
	})
	if diff := gocmp.Diff(credentials.Extra, reparsed.Extra, compactExtras); diff != "" {
		t.Fatalf("extra mismatch (-want +got):\n%s", diff)
	}
}

func TestAccessExpiredUsesTheFiveMinuteMargin(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	now := int64(1_000_000_000_000)

	credentials.ExpiresAtMillis = now + 4*60*1000
	if !credentials.AccessExpired(now, RefreshMarginMillis) {
		t.Fatal("4 minutes to expiry should refresh")
	}
	credentials.ExpiresAtMillis = now + 6*60*1000
	if credentials.AccessExpired(now, RefreshMarginMillis) {
		t.Fatal("6 minutes to expiry should not refresh")
	}
}

func TestAccessExpiredTreatsOverflowAsExpired(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	credentials.ExpiresAtMillis = int64(1<<63 - 1)
	if !credentials.AccessExpired(int64(1<<63-1), RefreshMarginMillis) {
		t.Fatal("an overflowing sum must not wrap into a distant future")
	}
}

func TestMergeRefreshKeepsTheOldRefreshTokenWhenTheServerSendsNone(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	before := mustDigests(t, credentials)

	if err := credentials.MergeRefresh(tokenResponse(t, 3600, ""), 1_000_000_000_000); err != nil {
		t.Fatalf("merge: %v", err)
	}

	after := mustDigests(t, credentials)
	if after.AccessSHA256 == before.AccessSHA256 {
		t.Fatal("the access token was not replaced")
	}
	if after.RefreshSHA256 != before.RefreshSHA256 {
		t.Fatal("the refresh chain must be kept")
	}
	if credentials.ExpiresAtMillis != 1_000_000_000_000+3_600_000 {
		t.Fatalf("seconds must become millis, got %d", credentials.ExpiresAtMillis)
	}
}

func TestMergeRefreshTakesARotatedRefreshToken(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	before := mustDigests(t, credentials)
	if err := credentials.MergeRefresh(tokenResponse(t, 60, "rotated-refresh"), 0); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if mustDigests(t, credentials).RefreshSHA256 == before.RefreshSHA256 {
		t.Fatal("a rotated refresh token must replace the stored one")
	}
}

func TestMergeRefreshReplacesScopesOnlyWhenTheServerNamesThem(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-old-blob.json")
	response := tokenResponse(t, 60, "")
	response.Scope = new("user:inference user:profile user:sessions:claude_code")
	if err := credentials.MergeRefresh(response, 0); err != nil {
		t.Fatalf("merge: %v", err)
	}
	want := []string{"user:inference", "user:profile", "user:sessions:claude_code"}
	if diff := gocmp.Diff(want, credentials.Scopes); diff != "" {
		t.Fatalf("scopes mismatch (-want +got):\n%s", diff)
	}

	if err := credentials.MergeRefresh(tokenResponse(t, 60, ""), 0); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(credentials.Scopes) != 3 {
		t.Fatalf("an absent scope leaves the stored set alone, got %v", credentials.Scopes)
	}
}

func TestMergeRefreshReportsAnOverflowingExpiry(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	err := credentials.MergeRefresh(tokenResponse(t, int64(1<<63-1), ""), 0)
	overflowErr, ok := errors.AsType[*ExpiryOverflowError](err)
	if !ok || overflowErr.Field != "expires_in" {
		t.Fatalf("an absurd lifetime must not wrap, got %v", err)
	}
}

func TestMergeRefreshCarriesANewRefreshExpiryAndKeepsAnAbsentOne(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	response := tokenResponse(t, 60, "")
	response.RefreshTokenExpiresIn = new(int64(120))
	if err := credentials.MergeRefresh(response, 1000); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if credentials.RefreshTokenExpiresAtMillis == nil || *credentials.RefreshTokenExpiresAtMillis != 121_000 {
		t.Fatalf("refresh expiry = %v", credentials.RefreshTokenExpiresAtMillis)
	}

	if err := credentials.MergeRefresh(tokenResponse(t, 60, ""), 1000); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if credentials.RefreshTokenExpiresAtMillis == nil || *credentials.RefreshTokenExpiresAtMillis != 121_000 {
		t.Fatal("an absent refresh expiry keeps the old value")
	}
}

func TestRefreshBodyCarriesTheScopesAndClientID(t *testing.T) {
	t.Parallel()

	// The body must include scope, or the server narrows the grant to its
	// default single scope.
	credentials := parseFixture(t, "credentials-new-blob.json")
	body, err := credentials.RefreshBody(ClientID)
	if err != nil {
		t.Fatalf("there is a refresh token: %v", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("the body should be JSON: %v", err)
	}
	want := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClientID,
		"scope":         strings.Join(DefaultScopes, " "),
		"refresh_token": "sk-ant-ort01-FAKE-NEW-REFRESH-TOKEN-NOT-A-REAL-CREDENTIAL",
	}
	if diff := gocmp.Diff(want, parsed); diff != "" {
		t.Fatalf("body mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshBodyRefusesAnAccountWithNoRefreshToken(t *testing.T) {
	t.Parallel()

	credentials, err := ParseBlob([]byte(`{"claudeAiOauth":{"accessToken":"a","expiresAt":1,"scopes":[]}}`))
	if err != nil {
		t.Fatalf("the blob should parse: %v", err)
	}
	if _, err := credentials.RefreshBody(ClientID); !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("want the no-refresh-token refusal, got %v", err)
	}
}

func TestDigestsAreTheSHA256OfTheTokenText(t *testing.T) {
	t.Parallel()

	credentials, err := ParseBlob([]byte(`{"claudeAiOauth":{"accessToken":"abc","refreshToken":"def","expiresAt":1}}`))
	if err != nil {
		t.Fatalf("the blob should parse: %v", err)
	}
	// sha256("abc") and sha256("def"), so the digest scheme is pinned
	// rather than merely self-consistent.
	want := Digests{
		AccessSHA256:  "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		RefreshSHA256: "cb8379ac2098aa165029e3938a51da0bcecfc008fd6795f401178647f96c5b34",
	}
	if diff := gocmp.Diff(want, mustDigests(t, credentials)); diff != "" {
		t.Fatalf("digest mismatch (-want +got):\n%s", diff)
	}
}

func TestEveryRenderOfCredentialsRedactsTheTokens(t *testing.T) {
	t.Parallel()

	credentials := parseFixture(t, "credentials-new-blob.json")
	digests := mustDigests(t, credentials)

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, nil))
	logger.Info("loaded", "credentials", credentials)

	marshalled, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	renders := map[string]string{
		"String()":    credentials.String(),
		"%v":          fmt.Sprintf("%v", credentials),
		"%+v":         fmt.Sprintf("%+v", credentials),
		"%#v":         fmt.Sprintf("%#v", credentials),
		"%s":          fmt.Sprintf("%s", credentials),
		"log line":    logBuffer.String(),
		"MarshalJSON": string(marshalled),
		"error wrap":  fmt.Errorf("reading credentials %v failed", credentials).Error(),
	}
	for name, rendered := range renders {
		if strings.Contains(rendered, "sk-ant-") {
			t.Errorf("%s leaked a token: %q", name, rendered)
		}
	}
	if !strings.Contains(credentials.String(), digests.AccessSHA256) {
		t.Fatalf("the render must carry the digest instead of the token: %q", credentials.String())
	}
	if !strings.Contains(credentials.String(), "<redacted>") {
		t.Fatalf("the render must mark the redaction: %q", credentials.String())
	}
}

func TestAWrongTypedKnownKeyKeepsItsPlaceAmongTheUnknownOnes(t *testing.T) {
	t.Parallel()

	// A blob whose subscriptionType is a number rather than a string is
	// not something to lose, and not something to reorder either: writing
	// it back anywhere else would change the bytes of a document this
	// build did not author.
	tests := map[string]struct {
		blob      string
		wantOrder []string
	}{
		"success: a numeric subscriptionType stays put": {
			blob:      `{"claudeAiOauth":{"accessToken":"a","expiresAt":5,"alpha":1,"subscriptionType":7,"omega":2}}`,
			wantOrder: []string{"alpha", "subscriptionType", "omega"},
		},
		"success: a string refreshTokenExpiresAt stays put": {
			blob:      `{"claudeAiOauth":{"accessToken":"a","expiresAt":5,"alpha":1,"refreshTokenExpiresAt":"soon","omega":2}}`,
			wantOrder: []string{"alpha", "refreshTokenExpiresAt", "omega"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			credentials, err := ParseBlob([]byte(tt.blob))
			if err != nil {
				t.Fatalf("the blob should parse: %v", err)
			}
			if credentials.SubscriptionType != nil {
				t.Fatal("a number is not a subscription type")
			}
			if credentials.RefreshTokenExpiresAtMillis != nil {
				t.Fatal("a string is not an expiry")
			}
			names := make([]string, 0, len(credentials.Extra))
			for _, extra := range credentials.Extra {
				names = append(names, extra.Name)
			}
			if diff := gocmp.Diff(tt.wantOrder, names); diff != "" {
				t.Fatalf("the key moved (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshMarginIsFiveMinutes(t *testing.T) {
	t.Parallel()

	if RefreshMarginMillis != 300_000 {
		t.Fatalf("the refresh lead is five minutes, got %d ms", RefreshMarginMillis)
	}
}
