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

	"github.com/awnumar/memguard"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/secret"
)

// newBlobGolden is the exact compact document the blob's author writes for
// the new fixture: known members in their fixed order, then the
// unrecognised profile member, all on one line. Every serializer that does
// not add siblings must keep producing these bytes.
const newBlobGolden = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-FAKE-NEW-ACCESS-TOKEN-NOT-A-REAL-CREDENTIAL",` +
	`"refreshToken":"sk-ant-ort01-FAKE-NEW-REFRESH-TOKEN-NOT-A-REAL-CREDENTIAL",` +
	`"expiresAt":1757600000000,"refreshTokenExpiresAt":1759900000000,` +
	`"scopes":["user:file_upload","user:inference","user:mcp_servers","user:profile","user:sessions:claude_code"],` +
	`"subscriptionType":"max","rateLimitTier":"default_claude_max_20x",` +
	`"clientId":"9d1c250a-e61b-44d9-88ed-5944d1962f5e",` +
	`"tokenAccount":{"uuid":"11111111-1111-4111-8111-111111111111","emailAddress":"user@example.com",` +
	`"organizationUuid":"22222222-2222-4222-8222-222222222222","organizationName":"Example Org",` +
	`"workspaceId":null,"workspaceName":null},` +
	`"profile":{"display_name":"Example User","has_claude_max":true}}}`

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

	credentials := parseFixture(t, "credentials-new-blob.json")
	if diff := documentDiff(t, []byte(newBlobGolden), mustBlobJSON(t, credentials)); diff != "" {
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

func TestRefreshBodyNeverOutgrowsTheArrayItWipes(t *testing.T) {
	t.Parallel()

	// The refresh body carries the refresh token, so it is held to the
	// rule the credential documents follow: one array, never outgrown,
	// all of it zero once the body is wiped. Each case is far longer
	// encoded than decoded.
	escapes := strings.Repeat(`\"\\\u0002`, 8<<10)
	tests := map[string]struct {
		document string
		clientID string
	}{
		"success: a refresh token that is mostly escapes": {
			document: `{"claudeAiOauth":{"accessToken":"a","refreshToken":"` + escapes + `","expiresAt":5,"scopes":["user:inference"]}}`,
			clientID: ClientID,
		},
		"success: a client id that is mostly escapes": {
			document: `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":5,"scopes":["user:inference"]}}`,
			clientID: strings.Repeat("\x01\"<", 4<<10),
		},
		"success: many scopes that need escaping": {
			document: `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":5,` +
				`"scopes":["` + strings.Repeat(`\u0003&","`, 4000) + `"]}}`,
			clientID: ClientID,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			credentials := mustParse(t, tt.document)
			scope := strings.Join(credentials.Scopes, " ")
			bound, err := credentials.refreshBodyBound(tt.clientID, scope)
			if err != nil {
				t.Fatalf("refresh body bound: %v", err)
			}

			backing := encoderBacking(bound)
			body, err := credentials.encodeRefreshBody(backing, tt.clientID, scope)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if cap(body) != cap(backing) || len(body) > bound {
				t.Fatalf("the encoder reallocated: %d bytes written into a capacity of %d, bound %d, first capacity %d",
					len(body), cap(body), bound, cap(backing))
			}
			want := sha256Hex(string(body))

			// The body is the request the server reads: each member must
			// decode to the field it came from.
			var decoded struct {
				GrantType    string `json:"grant_type"`
				RefreshToken string `json:"refresh_token"`
				ClientID     string `json:"client_id"`
				Scope        string `json:"scope"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("the body of %d bytes should be JSON: %v", len(body), err)
			}
			if same, err := credentials.RefreshToken.Equal(mustSecret(t, decoded.RefreshToken)); err != nil || !same {
				t.Fatalf("the refresh token does not round-trip (%d bytes decoded): %v", len(decoded.RefreshToken), err)
			}
			if decoded.GrantType != "refresh_token" || decoded.ClientID != tt.clientID || decoded.Scope != scope {
				t.Fatalf("a field does not round-trip: grant type %d bytes, client id %d bytes (want %d), scope %d bytes (want %d)",
					len(decoded.GrantType), len(decoded.ClientID), len(tt.clientID), len(decoded.Scope), len(scope))
			}

			memguard.WipeBytes(body)
			if n := nonZeroBytes(backing[:cap(backing)]); n != 0 {
				t.Fatalf("%d non-zero bytes left in the encoder's array after the body was wiped", n)
			}

			// The probe sees an outgrown array when the array is too small.
			small := make([]byte, 0, bound/8)
			grown, err := credentials.encodeRefreshBody(small, tt.clientID, scope)
			if err != nil {
				t.Fatalf("encode into a small array: %v", err)
			}
			if got := sha256Hex(string(grown)); got != want {
				t.Fatalf("the body depends on the array: sha256 %s, want %s", got, want)
			}
			memguard.WipeBytes(grown)
			if nonZeroBytes(small[:cap(small)]) == 0 {
				t.Fatal("an outgrown array should still hold what was written before the growth")
			}

			public, err := credentials.RefreshBody(tt.clientID)
			if err != nil {
				t.Fatalf("refresh body: %v", err)
			}
			if got := sha256Hex(string(public)); got != want {
				t.Fatalf("the public body differs: sha256 %s, want %s", got, want)
			}
			if cap(public) != cap(encoderBacking(bound)) {
				t.Fatalf("RefreshBody reallocated: capacity %d, planned %d", cap(public), cap(encoderBacking(bound)))
			}
			memguard.WipeBytes(public)
			if n := nonZeroBytes(public[:cap(public)]); n != 0 {
				t.Fatalf("%d non-zero bytes left after the public body was wiped", n)
			}
		})
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

// minimalAccount is the smallest claudeAiOauth object, written in the shape
// the serializer emits, so a document built around it round-trips byte for
// byte.
const minimalAccount = `"claudeAiOauth":{"accessToken":"a","expiresAt":5,"scopes":[]}`

// mustParse parses a document a test built.
func mustParse(t *testing.T, document string) *Credentials {
	t.Helper()
	credentials, err := ParseBlob([]byte(document))
	if err != nil {
		t.Fatalf("the document should parse (%d bytes, sha256 %s): %v", len(document), sha256Hex(document), err)
	}
	return credentials
}

// mustKeychainBlobJSON serializes the keychain document.
func mustKeychainBlobJSON(t *testing.T, c *Credentials) []byte {
	t.Helper()
	blob, err := c.KeychainBlobJSON()
	if err != nil {
		t.Fatalf("keychain blob JSON: %v", err)
	}
	return blob
}

// mustSiblingSummary returns the sibling metadata.
func mustSiblingSummary(t *testing.T, c *Credentials) []SiblingMeta {
	t.Helper()
	summary, err := c.SiblingSummary()
	if err != nil {
		t.Fatalf("sibling summary: %v", err)
	}
	return summary
}

// canonicalMember encodes name and value as one member of an object, under
// the options the keychain serializer documents, and returns the member's
// bytes without the braces: what the document carries for it.
func canonicalMember(t *testing.T, name string, value jsontext.Value) []byte {
	t.Helper()
	var buffer bytes.Buffer
	enc := jsontext.NewEncoder(&buffer, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteToken(jsontext.String(name)); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteValue(value); err != nil {
		t.Fatalf("encoding %s: %v", name, err)
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		t.Fatal(err)
	}
	object := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	return bytes.Clone(object[1 : len(object)-1])
}

// nonZeroBytes counts the bytes of every buffer that are not zero.
func nonZeroBytes(buffers ...[]byte) int {
	count := 0
	for _, buffer := range buffers {
		for _, b := range buffer {
			if b != 0 {
				count++
			}
		}
	}
	return count
}

// memberShape is what a failure message may say about one member: its
// name, length and digest, and the same for the members of an object
// value. The fixtures carry token-shaped canaries, and a diagnostic that
// printed them would copy them into the test log.
type memberShape struct {
	Name    string
	Len     int
	SHA256  string
	Members []memberShape
}

// shapeOf describes one member without its value.
func shapeOf(t *testing.T, name string, value []byte) memberShape {
	t.Helper()
	shape := memberShape{Name: name, Len: len(value), SHA256: sha256Hex(string(value))}
	if jsontext.Value(value).Kind() == '{' {
		shape.Members = objectShape(t, value)
	}
	return shape
}

// objectShape describes every member of an object in order.
func objectShape(t *testing.T, object []byte) []memberShape {
	t.Helper()
	dec := jsontext.NewDecoder(bytes.NewReader(object), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if token, err := dec.ReadToken(); err != nil || token.Kind() != '{' {
		t.Fatalf("not an object (%d bytes, sha256 %s): %v", len(object), sha256Hex(string(object)), err)
	}
	var shapes []memberShape
	for dec.PeekKind() != '}' {
		token, err := dec.ReadToken()
		if err != nil {
			t.Fatalf("reading a member name of an object of %d bytes: %v", len(object), err)
		}
		// The token is only valid until the next read.
		name := token.String()
		value, err := dec.ReadValue()
		if err != nil {
			t.Fatalf("reading member %q of an object of %d bytes: %v", name, len(object), err)
		}
		shapes = append(shapes, shapeOf(t, name, bytes.Clone(value)))
	}
	return shapes
}

// memberShapes describes each member of a list in order.
func memberShapes(t *testing.T, members []ExtraMember) []memberShape {
	t.Helper()
	shapes := make([]memberShape, 0, len(members))
	for _, m := range members {
		shapes = append(shapes, shapeOf(t, m.Name, m.Value))
	}
	return shapes
}

// documentDiff reports, member by member and without values, how got
// differs from want, or nothing when they are byte-identical.
func documentDiff(t *testing.T, want, got []byte) string {
	t.Helper()
	if bytes.Equal(want, got) {
		return ""
	}
	if diff := gocmp.Diff(objectShape(t, want), objectShape(t, got)); diff != "" {
		return diff
	}
	// Equal members, different bytes between them: only the whitespace or
	// the order of the punctuation differs.
	return fmt.Sprintf("the members match but the documents differ: want %d bytes sha256 %s, got %d bytes sha256 %s",
		len(want), sha256Hex(string(want)), len(got), sha256Hex(string(got)))
}

// memberValues returns the value buffers of members, aliased rather than
// copied, so a test can inspect them after the owner wipes them.
func memberValues(members []ExtraMember) [][]byte {
	values := make([][]byte, 0, len(members))
	for _, m := range members {
		values = append(values, m.Value)
	}
	return values
}

func TestParseBlobKeepsTheTopLevelSiblings(t *testing.T) {
	t.Parallel()

	// Claude Code keeps MCP OAuth tokens beside the account credential in
	// the one keychain item; dropping them on a rewrite logs the user out
	// of every remote MCP server.
	tests := map[string]struct {
		document string
		want     []ExtraMember
	}{
		"success: siblings are kept in arrival order around the account": {
			document: `{"mcpOAuth":{"srv|0123456789abcdef":{"accessToken":"mcp-token"}},` + minimalAccount +
				`,"mcpOAuthClientConfig":{"srv|0123456789abcdef":{"clientSecret":"client-secret"}},"pluginSecrets":{"p":"v"}}`,
			want: []ExtraMember{
				{Name: "mcpOAuth", Value: jsontext.Value(`{"srv|0123456789abcdef":{"accessToken":"mcp-token"}}`)},
				{Name: "mcpOAuthClientConfig", Value: jsontext.Value(`{"srv|0123456789abcdef":{"clientSecret":"client-secret"}}`)},
				{Name: "pluginSecrets", Value: jsontext.Value(`{"p":"v"}`)},
			},
		},
		"success: a duplicated top-level name keeps its first position and its last value": {
			document: `{"mcpOAuth":{"first":1},` + minimalAccount + `,"pluginSecrets":{},"mcpOAuth":{"last":2}}`,
			want: []ExtraMember{
				{Name: "mcpOAuth", Value: jsontext.Value(`{"last":2}`)},
				{Name: "pluginSecrets", Value: jsontext.Value(`{}`)},
			},
		},
		"success: a wrong-typed member is kept opaque": {
			document: `{` + minimalAccount + `,"mcpOAuth":"not an object","pluginSecrets":[1,{"x":2}],"empty":null,"count":7.5}`,
			want: []ExtraMember{
				{Name: "mcpOAuth", Value: jsontext.Value(`"not an object"`)},
				{Name: "pluginSecrets", Value: jsontext.Value(`[1,{"x":2}]`)},
				{Name: "empty", Value: jsontext.Value(`null`)},
				{Name: "count", Value: jsontext.Value(`7.5`)},
			},
		},
		"success: a document with only the account has no siblings": {
			document: `{` + minimalAccount + `}`,
			want:     nil,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			credentials := mustParse(t, tt.document)
			if diff := gocmp.Diff(memberShapes(t, tt.want), memberShapes(t, credentials.Siblings)); diff != "" {
				t.Fatalf("siblings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBlobJSONNeverWritesSiblings(t *testing.T) {
	t.Parallel()

	// The credential files are written through BlobJSON, and an MCP token
	// is the user's identity at a third party, not part of the account
	// being stored; adding the siblings must leave those bytes unchanged.
	withSiblings := strings.TrimSuffix(newBlobGolden, "}") +
		`,"mcpOAuth":{"srv|0123456789abcdef":{"accessToken":"SIBLING-CANARY-9f31"}},"pluginSecrets":{"p":"SIBLING-CANARY-9f31"}}`
	credentials := mustParse(t, withSiblings)
	if len(credentials.Siblings) != 2 {
		t.Fatalf("the siblings should be read, got %d", len(credentials.Siblings))
	}
	got := mustBlobJSON(t, credentials)
	if diff := documentDiff(t, []byte(newBlobGolden), got); diff != "" {
		t.Fatalf("the file document changed (-want +got):\n%s", diff)
	}
	if bytes.Contains(got, []byte("SIBLING-CANARY-9f31")) {
		t.Fatal("a sibling value reached the file document")
	}
}

func TestKeychainBlobJSONWritesTheAccountThenTheSiblings(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		document string
		want     string
	}{
		"success: without siblings it writes the golden bytes": {
			document: string(fixtureBlob(t, "credentials-new-blob.json")),
			want:     newBlobGolden,
		},
		"success: the account comes first and the siblings follow in arrival order": {
			document: `{"mcpOAuth":{"a":{"accessToken":"m"}},` + minimalAccount + `,"pluginSecrets":{"p":"v"}}`,
			want:     `{` + minimalAccount + `,"mcpOAuth":{"a":{"accessToken":"m"}},"pluginSecrets":{"p":"v"}}`,
		},
		"success: a wrong-typed sibling is written back as it arrived": {
			document: `{` + minimalAccount + `,"pluginSecrets":[1,"two",null],"mcpOAuth":"text"}`,
			want:     `{` + minimalAccount + `,"pluginSecrets":[1,"two",null],"mcpOAuth":"text"}`,
		},
		"success: a duplicated top-level name is written once with its last value": {
			document: `{"mcpOAuth":{"first":1},` + minimalAccount + `,"mcpOAuth":{"last":2}}`,
			want:     `{` + minimalAccount + `,"mcpOAuth":{"last":2}}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			credentials := mustParse(t, tt.document)
			if diff := documentDiff(t, []byte(tt.want), mustKeychainBlobJSON(t, credentials)); diff != "" {
				t.Fatalf("keychain document mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestKeychainBlobJSONSkipsASiblingThatNamesTheAccount(t *testing.T) {
	t.Parallel()

	// The field is exported, so a sibling can be set by hand; the account
	// object this build serializes is the one it vouches for.
	credentials := mustParse(t, `{`+minimalAccount+`}`)
	credentials.Siblings = []ExtraMember{
		{Name: BlobRoot, Value: jsontext.Value(`{"accessToken":"forged","expiresAt":1}`)},
		{Name: "pluginSecrets", Value: jsontext.Value(`{"p":"v"}`)},
		{Name: "pluginSecrets", Value: jsontext.Value(`{"p":"second"}`)},
	}
	want := `{` + minimalAccount + `,"pluginSecrets":{"p":"v"}}`
	if diff := documentDiff(t, []byte(want), mustKeychainBlobJSON(t, credentials)); diff != "" {
		t.Fatalf("keychain document mismatch (-want +got):\n%s", diff)
	}
}

func TestKeychainBlobJSONReencodesASiblingValueWithoutChangingIt(t *testing.T) {
	t.Parallel()

	// Each seed has insignificant whitespace and a nested duplicate name:
	// the default encoder refuses the duplicate, so the keychain serializer
	// must use its own options. What it preserves is the value, not the
	// original bytes: a JSON escape of a non-ASCII character is written as
	// the UTF-8 character it encodes, which is the encoder's canonical
	// form, and the digest covers that form.
	tests := map[string]struct {
		seed string
		// carries is a byte sequence the seed must contain, so the case
		// keeps testing what its name says.
		carries string
		// wantMember is the member as the encoder writes it.
		wantMember string
		// rewritten is a byte sequence of the seed that must not survive.
		rewritten string
	}{
		"success: non-ASCII escapes are written as the characters they encode": {
			seed:       `{ "srv" : { "a" : 1 , "a" : 2 } , "who" : "caf\u00e9 \ud83d\ude00" }`,
			carries:    `\u00e9`,
			wantMember: "\"mcpOAuth\":{\"srv\":{\"a\":1,\"a\":2},\"who\":\"caf\u00e9 \U0001F600\"}",
			rewritten:  `\u`,
		},
		"success: literal non-ASCII characters are written as they arrived": {
			seed:       "{ \"srv\" : { \"a\" : 1 , \"a\" : 2 } , \"who\" : \"caf\u00e9 \U0001F600\" }",
			carries:    "\u00e9",
			wantMember: "\"mcpOAuth\":{\"srv\":{\"a\":1,\"a\":2},\"who\":\"caf\u00e9 \U0001F600\"}",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seedValue := jsontext.Value(tt.seed)
			document := `{` + minimalAccount + `,"mcpOAuth":` + tt.seed + `}`
			if !strings.Contains(document, tt.carries) {
				t.Fatalf("the seed of %d bytes must carry the %d-byte sequence the case is about", len(document), len(tt.carries))
			}

			var strict bytes.Buffer
			if err := jsontext.NewEncoder(&strict).WriteValue(seedValue); err == nil {
				t.Fatal("the default encoder should refuse the nested duplicate name")
			}

			credentials := mustParse(t, document)
			written := mustKeychainBlobJSON(t, credentials)

			wantMember := canonicalMember(t, "mcpOAuth", seedValue)
			if !gocmp.Equal(tt.wantMember, string(wantMember)) {
				t.Fatalf("canonical member mismatch: want %d bytes sha256 %s, got %d bytes sha256 %s",
					len(tt.wantMember), sha256Hex(tt.wantMember), len(wantMember), sha256Hex(string(wantMember)))
			}
			if !bytes.Contains(written, wantMember) {
				t.Fatalf("the document of %d bytes does not carry the canonical member (sha256 %s)", len(written), sha256Hex(string(wantMember)))
			}
			if tt.rewritten != "" && bytes.Contains(written, []byte(tt.rewritten)) {
				t.Fatalf("the document of %d bytes still carries an escape the encoder rewrites", len(written))
			}

			reparsed := mustParse(t, string(written))
			if len(reparsed.Siblings) != 1 {
				t.Fatalf("one sibling expected, got %d", len(reparsed.Siblings))
			}
			decode := func(value jsontext.Value) any {
				var decoded any
				if err := json.Unmarshal(value, &decoded, jsontext.AllowDuplicateNames(true)); err != nil {
					t.Fatalf("decoding a value of %d bytes: %v", len(value), err)
				}
				return decoded
			}
			if !gocmp.Equal(decode(seedValue), decode(reparsed.Siblings[0].Value)) {
				t.Fatalf("the value changed: seed %d bytes sha256 %s, written %d bytes sha256 %s",
					len(seedValue), sha256Hex(string(seedValue)), len(reparsed.Siblings[0].Value), sha256Hex(string(reparsed.Siblings[0].Value)))
			}

			want := sha256Hex(string(wantMember))
			if got := mustSiblingSummary(t, credentials)[0].SHA256; got != want {
				t.Fatalf("the digest is not over the encoder's bytes: got %s, want %s", got, want)
			}
			if got := mustSiblingSummary(t, reparsed)[0].SHA256; got != want {
				t.Fatalf("the digest changed across the write: got %s, want %s", got, want)
			}
		})
	}
}

func TestSiblingSummaryDescribesEachSiblingWithoutItsValue(t *testing.T) {
	t.Parallel()

	two, one, zero := 2, 1, 0
	tests := map[string]struct {
		document    string
		wantNames   []string
		wantEntries []*int
	}{
		"success: object siblings count their distinct names": {
			document: `{` + minimalAccount + `,"mcpOAuth":{"a|1":{"accessToken":"SIBLING-CANARY-9f31"},"b|2":{"accessToken":"x"},"a|1":{"accessToken":"y"}},` +
				`"mcpOAuthClientConfig":{"a|1":{"clientSecret":"SIBLING-CANARY-9f31"}},"pluginSecrets":{}}`,
			wantNames:   []string{"mcpOAuth", "mcpOAuthClientConfig", "pluginSecrets"},
			wantEntries: []*int{&two, &one, &zero},
		},
		"success: a non-object sibling has no entry count": {
			document:    `{` + minimalAccount + `,"mcpOAuth":"SIBLING-CANARY-9f31","pluginSecrets":[1,2],"flag":true}`,
			wantNames:   []string{"mcpOAuth", "pluginSecrets", "flag"},
			wantEntries: []*int{nil, nil, nil},
		},
		"success: no siblings summarize empty": {
			document:    `{` + minimalAccount + `}`,
			wantNames:   []string{},
			wantEntries: []*int{},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			credentials := mustParse(t, tt.document)
			summary := mustSiblingSummary(t, credentials)
			names := make([]string, 0, len(summary))
			entries := make([]*int, 0, len(summary))
			for i, meta := range summary {
				names = append(names, meta.Name)
				entries = append(entries, meta.Entries)
				want := sha256Hex(string(canonicalMember(t, credentials.Siblings[i].Name, credentials.Siblings[i].Value)))
				if meta.SHA256 != want {
					t.Errorf("%s: digest %s, want %s", meta.Name, meta.SHA256, want)
				}
			}
			if diff := gocmp.Diff(tt.wantNames, names); diff != "" {
				t.Fatalf("names mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantEntries, entries); diff != "" {
				t.Fatalf("entries mismatch (-want +got):\n%s", diff)
			}
			if rendered := fmt.Sprintf("%+v %#v", summary, summary); strings.Contains(rendered, "SIBLING-CANARY-9f31") {
				t.Fatalf("the summary carries a value (rendered %d bytes)", len(rendered))
			}
		})
	}
}

func TestSiblingSummaryIsStableAcrossAReparse(t *testing.T) {
	t.Parallel()

	// The readback after a keychain write compares the summary of what was
	// written with the summary of what is read back; the same members must
	// summarize equal however they were formatted when they arrived.
	tests := map[string]struct {
		document string
	}{
		"success: compact siblings": {
			document: `{` + minimalAccount + `,"mcpOAuth":{"a":{"accessToken":"m","expiresAt":1}},"pluginSecrets":{"p":"v"}}`,
		},
		"success: siblings with whitespace and escapes": {
			document: "{\n  \"mcpOAuth\" : { \"a\" : { \"accessToken\" : \"m\\u0041\\/\" } },\n  " + minimalAccount +
				",\n  \"pluginSecrets\" : [ 1 , 2 ]\n}",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			first := mustParse(t, tt.document)
			second := mustParse(t, string(mustKeychainBlobJSON(t, first)))
			third := mustParse(t, string(mustKeychainBlobJSON(t, second)))
			want := mustSiblingSummary(t, first)
			if diff := gocmp.Diff(want, mustSiblingSummary(t, second)); diff != "" {
				t.Fatalf("summary changed across a reparse (-first +second):\n%s", diff)
			}
			if diff := gocmp.Diff(want, mustSiblingSummary(t, third)); diff != "" {
				t.Fatalf("summary changed across two reparses (-first +third):\n%s", diff)
			}
			if diff := gocmp.Diff(mustDigests(t, first), mustDigests(t, second)); diff != "" {
				t.Fatalf("account digests changed (-first +second):\n%s", diff)
			}
			if diff := documentDiff(t, mustKeychainBlobJSON(t, second), mustKeychainBlobJSON(t, third)); diff != "" {
				t.Fatalf("the re-encoded document is not a fixed point (-second +third):\n%s", diff)
			}
		})
	}
}

func TestDigestsIgnoreTheSiblings(t *testing.T) {
	t.Parallel()

	// Digests are the account identity "already active" checks and undo
	// selection compare; two copies of one account must not look different
	// because one item also holds MCP tokens.
	bare := mustParse(t, `{`+minimalAccount+`}`)
	withSiblings := mustParse(t, `{`+minimalAccount+`,"mcpOAuth":{"a":{"accessToken":"m"}}}`)
	if diff := gocmp.Diff(mustDigests(t, bare), mustDigests(t, withSiblings)); diff != "" {
		t.Fatalf("siblings changed the account digests (-bare +with):\n%s", diff)
	}
}

func TestWipeZeroesEverySiblingAndUnrecognisedValue(t *testing.T) {
	t.Parallel()

	credentials := mustParse(t, `{"claudeAiOauth":{"accessToken":"a","expiresAt":5,"scopes":[],"profile":{"secret":"inner"}},`+
		`"mcpOAuth":{"a":{"accessToken":"SIBLING-CANARY-9f31"}},"pluginSecrets":{"p":"v"}}`)
	values := append(memberValues(credentials.Siblings), memberValues(credentials.Extra)...)
	if len(values) != 3 || nonZeroBytes(values...) == 0 {
		t.Fatalf("the fixture should carry three non-empty values, got %d", len(values))
	}

	credentials.Wipe()

	if n := nonZeroBytes(values...); n != 0 {
		t.Fatalf("%d non-zero bytes left in the wiped values", n)
	}
	if len(credentials.Siblings) != 0 || len(credentials.Extra) != 0 {
		t.Fatalf("the lists should be empty, got %d siblings and %d extra", len(credentials.Siblings), len(credentials.Extra))
	}
	if credentials.AccessToken == nil {
		t.Fatal("the sealed token has its own lifetime and is not wiped")
	}

	var absent *Credentials
	absent.Wipe()
}

func TestParseBlobWipesItsCopiesWhenARequiredFieldIsMissing(t *testing.T) {
	t.Parallel()

	// A refused blob still carried tokens, and ParseBlob copied them while
	// reading: the whole document, every member and every unrecognised
	// value. None of those copies reaches the caller, so each must be
	// zeroed before the error returns.
	const accountCanary, siblingCanary = "ACCOUNT-CANARY-51c2", "SIBLING-CANARY-9f31"
	sibling := `,"mcpOAuth":{"a":{"accessToken":"` + siblingCanary + `"}}}`
	tests := map[string]struct {
		document  string
		wantField string
	}{
		"error: no access token": {
			document:  `{"claudeAiOauth":{"refreshToken":"` + accountCanary + `","expiresAt":5,"profile":{"k":"` + accountCanary + `"}}` + sibling,
			wantField: "accessToken",
		},
		"error: an access token of the wrong type": {
			document:  `{"claudeAiOauth":{"accessToken":["` + accountCanary + `"],"expiresAt":5,"profile":{"k":"` + accountCanary + `"}}` + sibling,
			wantField: "accessToken",
		},
		"error: no expiry": {
			document:  `{"claudeAiOauth":{"accessToken":"` + accountCanary + `","profile":{"k":"` + accountCanary + `"}}` + sibling,
			wantField: "expiresAt",
		},
		"error: an expiry of the wrong type": {
			document:  `{"claudeAiOauth":{"accessToken":"` + accountCanary + `","expiresAt":"soon","profile":{"k":"` + accountCanary + `"}}` + sibling,
			wantField: "expiresAt",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var copies [][]byte
			carried := map[string]int{}
			observe := func(value jsontext.Value) {
				copies = append(copies, value)
				for _, canary := range []string{accountCanary, siblingCanary} {
					if bytes.Contains(value, []byte(canary)) {
						carried[canary]++
					}
				}
			}

			credentials, err := parseBlob([]byte(tt.document), observe)

			var missing *MissingFieldError
			if !errors.As(err, &missing) || missing.Field != tt.wantField {
				t.Fatalf("want a missing %s error, got %v", tt.wantField, err)
			}
			if credentials != nil {
				t.Fatal("a refused blob should yield no credentials")
			}
			// The probe saw the plaintext: the account and the sibling
			// canaries each sat in at least the document copy and the
			// copy of their own member.
			for _, canary := range []string{accountCanary, siblingCanary} {
				if carried[canary] < 2 {
					t.Fatalf("the parse should have copied the %d-byte canary at least twice, saw %d copies of it in %d buffers",
						len(canary), carried[canary], len(copies))
				}
			}
			if n := nonZeroBytes(copies...); n != 0 {
				t.Fatalf("%d non-zero bytes left across the %d buffers the parse copied into", n, len(copies))
			}
		})
	}
}

func TestCarrySiblingsFromReplacesTheSiblings(t *testing.T) {
	t.Parallel()

	source := `{` + minimalAccount + `,"mcpOAuth":{"a":{"accessToken":"carried"}},"pluginSecrets":{"p":"v"}}`
	tests := map[string]struct {
		receiver string
		source   *string
		self     bool
		want     []ExtraMember
	}{
		"success: a nil source clears the siblings": {
			receiver: `{` + minimalAccount + `,"mcpOAuth":{"stale":{"accessToken":"from a file"}}}`,
			source:   nil,
			want:     nil,
		},
		"success: a source without siblings clears them": {
			receiver: `{` + minimalAccount + `,"mcpOAuth":{"stale":{"accessToken":"from a file"}}}`,
			source:   new(`{` + minimalAccount + `}`),
			want:     nil,
		},
		"success: the source's siblings replace the receiver's in order": {
			receiver: `{` + minimalAccount + `,"mcpOAuth":{"stale":{"accessToken":"from a file"}},"other":1}`,
			source:   &source,
			want: []ExtraMember{
				{Name: "mcpOAuth", Value: jsontext.Value(`{"a":{"accessToken":"carried"}}`)},
				{Name: "pluginSecrets", Value: jsontext.Value(`{"p":"v"}`)},
			},
		},
		"success: carrying from itself keeps the siblings": {
			receiver: source,
			self:     true,
			want: []ExtraMember{
				{Name: "mcpOAuth", Value: jsontext.Value(`{"a":{"accessToken":"carried"}}`)},
				{Name: "pluginSecrets", Value: jsontext.Value(`{"p":"v"}`)},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			receiver := mustParse(t, tt.receiver)
			previous := memberValues(receiver.Siblings)

			var src *Credentials
			switch {
			case tt.self:
				src = receiver
			case tt.source != nil:
				src = mustParse(t, *tt.source)
			}
			receiver.CarrySiblingsFrom(src)

			if diff := gocmp.Diff(memberShapes(t, tt.want), memberShapes(t, receiver.Siblings)); diff != "" {
				t.Fatalf("siblings mismatch (-want +got):\n%s", diff)
			}
			if tt.self {
				return
			}
			if n := nonZeroBytes(previous...); n != 0 {
				t.Fatalf("%d non-zero bytes left in the replaced siblings", n)
			}
			if src == nil {
				return
			}
			// The carried values are copies: wiping the source must not
			// reach into the receiver.
			src.Wipe()
			if diff := gocmp.Diff(memberShapes(t, tt.want), memberShapes(t, receiver.Siblings)); diff != "" {
				t.Fatalf("the carry aliased the source (-want +got):\n%s", diff)
			}
		})
	}
}

func TestKeychainBlobJSONRoundTripsAOneMebibyteDocument(t *testing.T) {
	t.Parallel()

	// Well past what the measured item holds today, to show the serializer
	// has no size assumption of its own; the transport sets the ceiling.
	var document strings.Builder
	document.WriteString(`{` + minimalAccount + `,"mcpOAuth":{`)
	token := strings.Repeat("0123456789abcdef", 64)
	entries := 0
	for document.Len() < 1<<20 {
		if entries > 0 {
			document.WriteByte(',')
		}
		fmt.Fprintf(&document, `"server-%05d|0123456789abcdef":{"serverName":"server-%05d","accessToken":"%s-%05d","expiresAt":%d}`,
			entries, entries, token, entries, 1757600000000+int64(entries))
		entries++
	}
	document.WriteString(`},"pluginSecrets":{"p":"v"}}`)
	seed := document.String()

	credentials := mustParse(t, seed)
	written := mustKeychainBlobJSON(t, credentials)
	if len(written) < 1<<20 {
		t.Fatalf("the document is %d bytes, want at least 1 MiB", len(written))
	}
	if !bytes.Equal([]byte(seed), written) {
		t.Fatalf("a compact document in the author's order must round-trip byte for byte (seed %d bytes, written %d bytes)", len(seed), len(written))
	}

	reparsed := mustParse(t, string(written))
	summary := mustSiblingSummary(t, reparsed)
	if diff := gocmp.Diff(mustSiblingSummary(t, credentials), summary); diff != "" {
		t.Fatalf("summary mismatch (-want +got):\n%s", diff)
	}
	if summary[0].Entries == nil || *summary[0].Entries != entries {
		t.Fatalf("mcpOAuth should count %d entries, got %v", entries, summary[0].Entries)
	}
	if diff := documentDiff(t, []byte(newBlobGolden), mustBlobJSON(t, parseFixture(t, "credentials-new-blob.json"))); diff != "" {
		t.Fatalf("the file document changed (-want +got):\n%s", diff)
	}
}

func TestSerializersNeverOutgrowTheArrayTheyWipe(t *testing.T) {
	t.Parallel()

	// A serializer whose buffer reallocates leaves the plaintext it had
	// already written in the outgrown array, where no wipe reaches it.
	// Every case here is far longer encoded than decoded: a name of
	// control characters escapes each byte to six, and a token of quotes,
	// backslashes and control characters to two or six.
	escapes := strings.Repeat(`\"\\\u0002`, 8<<10)
	controlName := strings.Repeat(`\u0001`, 2000)
	tests := map[string]struct {
		document string
		siblings []ExtraMember
	}{
		"success: a sibling name of control characters": {
			document: `{` + minimalAccount + `,"` + controlName + `":{"accessToken":"SIBLING-CANARY-9f31"}}`,
		},
		"success: an access token that is mostly escapes": {
			document: `{"claudeAiOauth":{"accessToken":"` + escapes + `","refreshToken":"` + escapes + `","expiresAt":5,"scopes":[]}}`,
		},
		"success: a large tokenAccount field": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":5,"scopes":[],` +
				`"tokenAccount":{"uuid":"u","emailAddress":"` + escapes + `","organizationUuid":"` + escapes + `"}}}`,
		},
		"success: a large unrecognised account member": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":5,"scopes":[],"` + controlName + `":{"k":"` + escapes + `"}}}`,
		},
		"success: scopes and optional strings that need escaping": {
			document: `{"claudeAiOauth":{"accessToken":"a","expiresAt":5,` +
				`"scopes":["` + strings.Repeat(`\u0003","`, 4000) + `"],` +
				`"subscriptionType":"` + escapes + `","rateLimitTier":"` + escapes + `","clientId":"` + escapes + `"}}`,
		},
		"success: a sibling value of invalid UTF-8 set by hand": {
			document: `{` + minimalAccount + `}`,
			// Each invalid byte is written as a three-byte replacement
			// character; the parser never produces this, but the field is
			// exported.
			siblings: []ExtraMember{{Name: "pluginSecrets", Value: jsontext.Value(`"` + strings.Repeat("\xff", 8<<10) + `"`)}},
		},
	}
	for name, tt := range tests {
		for _, siblings := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/siblings=%t", name, siblings), func(t *testing.T) {
				t.Parallel()
				credentials := mustParse(t, tt.document)
				if tt.siblings != nil {
					credentials.Siblings = tt.siblings
				}
				bound, err := credentials.documentBound(siblings)
				if err != nil {
					t.Fatalf("document bound: %v", err)
				}

				// The array handed to the encoder holds every byte it
				// wrote: once the result is wiped, nothing is left in it.
				backing := encoderBacking(bound)
				written, err := credentials.encodeDocument(backing, siblings)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				if cap(written) != cap(backing) || len(written) > bound {
					t.Fatalf("the encoder reallocated: %d bytes written into a capacity of %d, bound %d, first capacity %d",
						len(written), cap(written), bound, cap(backing))
				}
				want := sha256Hex(string(written))
				memguard.WipeBytes(written)
				if n := nonZeroBytes(backing[:cap(backing)]); n != 0 {
					t.Fatalf("%d non-zero bytes left in the encoder's array after the result was wiped", n)
				}

				// The same probe sees the outgrown array when the array is
				// too small, so a zero above is not a probe that sees
				// nothing.
				small := make([]byte, 0, bound/8)
				grown, err := credentials.encodeDocument(small, siblings)
				if err != nil {
					t.Fatalf("encode into a small array: %v", err)
				}
				if got := sha256Hex(string(grown)); got != want {
					t.Fatalf("the output depends on the array: sha256 %s, want %s", got, want)
				}
				memguard.WipeBytes(grown)
				if nonZeroBytes(small[:cap(small)]) == 0 {
					t.Fatal("an outgrown array should still hold what was written before the growth")
				}

				// Through the public API: one allocation of the planned
				// capacity, and wiping the result leaves it all zero.
				serialize := credentials.BlobJSON
				if siblings {
					serialize = credentials.KeychainBlobJSON
				}
				public, err := serialize()
				if err != nil {
					t.Fatalf("serialize: %v", err)
				}
				if got := sha256Hex(string(public)); got != want {
					t.Fatalf("the public output differs: sha256 %s, want %s", got, want)
				}
				if cap(public) != cap(encoderBacking(bound)) {
					t.Fatalf("the public serializer reallocated: capacity %d, planned %d", cap(public), cap(encoderBacking(bound)))
				}
				memguard.WipeBytes(public)
				if n := nonZeroBytes(public[:cap(public)]); n != 0 {
					t.Fatalf("%d non-zero bytes left after the public output was wiped", n)
				}

				// Each sibling digest encodes its member the same way.
				if !siblings {
					return
				}
				for _, m := range credentials.Siblings {
					memberBacking := encoderBacking(memberBound(m))
					object, err := encodeMember(memberBacking, m)
					if err != nil {
						t.Fatalf("encode member %q: %v", m.Name, err)
					}
					if cap(object) != cap(memberBacking) {
						t.Fatalf("the member encoder reallocated: capacity %d, planned %d", cap(object), cap(memberBacking))
					}
					memguard.WipeBytes(object)
					if n := nonZeroBytes(memberBacking[:cap(memberBacking)]); n != 0 {
						t.Fatalf("%d non-zero bytes left in the member encoder's array", n)
					}
				}
				summary := mustSiblingSummary(t, credentials)
				for i, meta := range summary {
					m := credentials.Siblings[i]
					if want := sha256Hex(string(canonicalMember(t, m.Name, m.Value))); meta.SHA256 != want {
						t.Errorf("sibling %d: digest %s, want %s", i, meta.SHA256, want)
					}
				}
			})
		}
	}
}

func TestEncodedBoundCoversTheEncoder(t *testing.T) {
	t.Parallel()

	// The bound is the whole defence against a reallocation, so it is
	// checked against the encoder itself on inputs chosen to expand, under
	// every escaping option, not only on the fixtures above.
	tests := map[string]struct {
		input string
		// value marks an input that is itself a JSON value, which the
		// keychain serializer re-encodes.
		value bool
	}{
		"success: every control character":            {input: "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f"},
		"success: quotes and backslashes":             {input: `"\"\\\\"`},
		"success: HTML-sensitive characters":          {input: "<script>&</script>"},
		"success: invalid UTF-8":                      {input: "\xff\xfe\x80\xc0"},
		"success: a truncated multi-byte sequence":    {input: "\xe6\x97"},
		"success: line and paragraph separators":      {input: "\u2028\u2029"},
		"success: multi-byte runes":                   {input: "café 日本 😀"},
		"success: a value with a lone surrogate":      {input: `"\ud800"`, value: true},
		"success: a value with redundant escapes":     {input: `"\u00e9\/\u0022\u0041"`, value: true},
		"success: a value with whitespace":            {input: "{ \"a\" : [ 1 , 2 ] ,\n \"b\" : \"\\u0001\" }", value: true},
		"success: a value with raw invalid UTF-8":     {input: "\"\xff\xff\"", value: true},
		"success: a value with raw separators":        {input: "\"\u2028<&>\u2029\"", value: true},
		"success: a value with a number and literals": {input: `[-1.5e+10,true,false,null]`, value: true},
	}
	optionSets := map[string][]jsontext.Options{
		"default":  nil,
		"keychain": keychainValueOptions,
		"for HTML": {jsontext.AllowInvalidUTF8(true), jsontext.EscapeForHTML(true)},
		"for JS":   {jsontext.AllowInvalidUTF8(true), jsontext.EscapeForJS(true)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for optionsName, opts := range optionSets {
				var quoted bytes.Buffer
				if err := jsontext.NewEncoder(&quoted, opts...).WriteToken(jsontext.String(tt.input)); err == nil {
					if got, bound := len(bytes.TrimSuffix(quoted.Bytes(), []byte("\n"))), quotedBound(tt.input); got > bound {
						t.Errorf("%s: quoting wrote %d bytes, bound %d", optionsName, got, bound)
					}
				}
				if !tt.value {
					continue
				}
				var reencoded bytes.Buffer
				err := jsontext.NewEncoder(&reencoded, opts...).WriteValue(jsontext.Value(tt.input))
				if err != nil {
					if optionsName == "keychain" {
						t.Fatalf("the keychain options should accept the value: %v", err)
					}
					continue
				}
				if got, bound := len(bytes.TrimSuffix(reencoded.Bytes(), []byte("\n"))), encodedBound(tt.input); got > bound {
					t.Errorf("%s: re-encoding wrote %d bytes, bound %d", optionsName, got, bound)
				}
			}
		})
	}
}

func FuzzEncodedBoundCoversTheEncoder(f *testing.F) {
	for _, seed := range []string{"", "\x00\x1f", `"\\`, "<&>", "\xff\xe6\x97", "\u2028", `"\ud800\u00e9\/"`, `{"a":[1,"\u0001"]}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		opts := []jsontext.Options{jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true)}
		var quoted bytes.Buffer
		if err := jsontext.NewEncoder(&quoted, opts...).WriteToken(jsontext.String(input)); err != nil {
			t.Fatalf("quoting an input of %d bytes: %v", len(input), err)
		}
		if got, bound := len(bytes.TrimSuffix(quoted.Bytes(), []byte("\n"))), quotedBound(input); got > bound {
			t.Fatalf("quoting an input of %d bytes wrote %d bytes, bound %d", len(input), got, bound)
		}
		var reencoded bytes.Buffer
		if err := jsontext.NewEncoder(&reencoded, opts...).WriteValue(jsontext.Value(input)); err != nil {
			return
		}
		if got, bound := len(bytes.TrimSuffix(reencoded.Bytes(), []byte("\n"))), encodedBound(input); got > bound {
			t.Fatalf("re-encoding a value of %d bytes wrote %d bytes, bound %d", len(input), got, bound)
		}
	})
}
