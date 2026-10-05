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
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"

	"github.com/awnumar/memguard"
)

// MaxTokenBytes bounds JWT decoding before allocation.
const MaxTokenBytes = 256 * 1024

var (
	// ErrJWTShape means the token is not three nonempty segments.
	ErrJWTShape = errors.New("the token is not a three-part JWT")
	// ErrJWTTooLarge means the token exceeds the decode limit.
	ErrJWTTooLarge = errors.New("the token is larger than the 262144-byte limit")
	// ErrJWTEncoding means the payload is not base64url.
	ErrJWTEncoding = errors.New("the token's payload is not base64url")
	// ErrJWTJSON means the payload is not a JSON object.
	ErrJWTJSON = errors.New("the token's payload is not a JSON object")
)

// Claims is an identity-label allowlist, not an authorization assertion.
type Claims struct {
	Email            *string
	ChatGPTUserID    *string
	ChatGPTAccountID *string
	PlanType         *string
	IsFedramp        bool
	Exp              *int64
}

// ParseClaims decodes JWT labels without checking its signature, never quoting input in an error.
func ParseClaims(token []byte) (Claims, error) {
	payload, err := jwtPayload(token)
	if err != nil {
		return Claims{}, err
	}
	defer memguard.WipeBytes(payload)
	// Decoding into raw values tolerates wrong-typed optional labels without carrying unknown claims.
	var root map[string]jsontext.Value
	defer func() {
		for _, value := range root {
			memguard.WipeBytes(value)
		}
	}()
	if json.Unmarshal(payload, &root) != nil || root == nil {
		return Claims{}, ErrJWTJSON
	}
	result := Claims{Email: jsonString(root["email"]), Exp: jsonInteger(root["exp"])}
	var profile map[string]jsontext.Value
	if json.Unmarshal(root["https://api.openai.com/profile"], &profile) == nil && result.Email == nil {
		result.Email = jsonString(profile["email"])
	}
	var auth map[string]jsontext.Value
	if json.Unmarshal(root["https://api.openai.com/auth"], &auth) == nil {
		result.ChatGPTUserID = jsonString(auth["chatgpt_user_id"])
		if result.ChatGPTUserID == nil {
			result.ChatGPTUserID = jsonString(auth["user_id"])
		}
		result.ChatGPTAccountID = jsonString(auth["chatgpt_account_id"])
		result.PlanType = jsonString(auth["chatgpt_plan_type"])
		_ = json.Unmarshal(auth["chatgpt_account_is_fedramp"], &result.IsFedramp)
	}
	for _, m := range []map[string]jsontext.Value{root, profile, auth} {
		for _, value := range m {
			memguard.WipeBytes(value)
		}
	}
	return result, nil
}

// ParseExpiry decodes only the integer expiry of an access token.
func ParseExpiry(token []byte) (*int64, error) {
	payload, err := jwtPayload(token)
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(payload)
	var root map[string]jsontext.Value
	defer func() {
		for _, value := range root {
			memguard.WipeBytes(value)
		}
	}()
	if json.Unmarshal(payload, &root) != nil || root == nil {
		return nil, ErrJWTJSON
	}
	return jsonInteger(root["exp"]), nil
}

func jwtPayload(token []byte) ([]byte, error) {
	if len(token) > MaxTokenBytes {
		return nil, ErrJWTTooLarge
	}
	header, rest, ok := bytes.Cut(token, []byte("."))
	if !ok || len(header) == 0 {
		return nil, ErrJWTShape
	}
	body, signature, ok := bytes.Cut(rest, []byte("."))
	if !ok || len(body) == 0 || len(signature) == 0 || bytes.ContainsRune(signature, '.') {
		return nil, ErrJWTShape
	}
	body = bytes.TrimRight(body, "=")
	payload := make([]byte, base64.RawURLEncoding.DecodedLen(len(body)))
	n, err := base64.RawURLEncoding.Decode(payload, body)
	if err != nil {
		memguard.WipeBytes(payload)
		return nil, ErrJWTEncoding
	}
	return payload[:n], nil
}

func jsonString(raw jsontext.Value) *string {
	if raw.Kind() != '"' {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func jsonInteger(raw jsontext.Value) *int64 {
	if raw.Kind() != '0' || bytes.ContainsAny(raw, ".eE") {
		return nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}
