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

package secret

import (
	"bytes"
	"encoding"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/awnumar/memguard"
	gocmp "github.com/google/go-cmp/cmp"
)

func TestNewSecret(t *testing.T) {
	tests := map[string]struct {
		input     []byte
		wantError bool
	}{
		"success: token":  {input: []byte("private-token")},
		"success: binary": {input: []byte{0, 255, 10, 0}},
		"error: empty":    {input: []byte{}, wantError: true},
		"error: nil":      {wantError: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want := bytes.Clone(tt.input)
			s, err := NewSecret(tt.input)
			if (err != nil) != tt.wantError {
				t.Fatalf("NewSecret error = %v, want error %v", err, tt.wantError)
			}
			if tt.wantError {
				return
			}
			if diff := gocmp.Diff(make([]byte, len(want)), tt.input); diff != "" {
				t.Fatalf("input was not wiped (-want +got):\n%s", diff)
			}
			if s.Len() != len(want) {
				t.Fatalf("Len = %d, want %d", s.Len(), len(want))
			}
			if err := s.WithPlaintext(func(b []byte) error {
				if diff := gocmp.Diff(want, b); diff != "" {
					t.Errorf("plaintext (-want +got):\n%s", diff)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSecretRedaction(t *testing.T) {
	s, err := NewSecret([]byte("sk-ant-private-test-sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ value any }{
		"success: pointer": {value: s},
		"success: value":   {value: *s},
		"success: zero":    {value: Secret{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want := strings.TrimSpace(strings.Repeat("[REDACTED] ", 5))
			got := fmt.Sprintf("%v %s %q %+v %#v", tt.value, tt.value, tt.value, tt.value, tt.value)
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Fatalf("format (-want +got):\n%s", diff)
			}
			if got := errors.New(fmt.Sprint(tt.value)).Error(); got != redacted {
				t.Fatalf("error string = %q", got)
			}
			for _, verb := range []string{"%x", "%X", "%d", "%#q", "%100.2s", "%+50v"} {
				if got := fmt.Sprintf(verb, tt.value); got != redacted {
					t.Errorf("verb %s = %q", verb, got)
				}
			}
			for _, handler := range []func(*bytes.Buffer) slog.Handler{
				func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
				func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
			} {
				var out bytes.Buffer
				slog.New(handler(&out)).InfoContext(t.Context(), "event", slog.Any("token", tt.value), slog.Group("nested", slog.Any("token", tt.value)))
				if strings.Contains(out.String(), "private-test-sentinel") || strings.Count(out.String(), redacted) != 2 {
					t.Fatalf("log redaction failed: %s", &out)
				}
			}
			encoded, err := json.Marshal(struct {
				Token any `json:"token"`
			}{Token: tt.value})
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(`{"token":"[REDACTED]"}`, string(encoded)); diff != "" {
				t.Fatalf("JSON (-want +got):\n%s", diff)
			}
		})
	}
	encoded, err := json.Marshal(struct {
		Value   Secret  `json:"value"`
		Pointer *Secret `json:"pointer"`
	}{Value: *s, Pointer: s})
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(`{"value":"[REDACTED]","pointer":"[REDACTED]"}`, string(encoded)); diff != "" {
		t.Fatalf("typed JSON (-want +got):\n%s", diff)
	}
	jsonBytes, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(`"[REDACTED]"`, string(jsonBytes)); diff != "" {
		t.Fatal(diff)
	}
	text, err := s.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(redacted, string(text)); diff != "" {
		t.Fatal(diff)
	}
	text, err = s.AppendText([]byte("prefix:"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff("prefix:"+redacted, string(text)); diff != "" {
		t.Fatal(diff)
	}
	if s.String() != redacted || s.GoString() != redacted {
		t.Fatal("string hooks must redact")
	}
}

func TestSecretOperations(t *testing.T) {
	s, err := NewSecret([]byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", digest); diff != "" {
		t.Fatal(diff)
	}
	prefix, err := s.Digest8()
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff("ba7816bf", prefix); diff != "" {
		t.Fatal(diff)
	}
	clone := s.Clone()
	if clone == s {
		t.Fatal("Clone returned the original wrapper")
	}
	equal, err := s.Equal(clone)
	if err != nil || !equal {
		t.Fatalf("clone equality = %v, %v", equal, err)
	}
	tests := map[string]struct {
		other string
		equal bool
	}{
		"success: equal":            {other: "abc", equal: true},
		"success: different bytes":  {other: "abd"},
		"success: different length": {other: "abcd"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			other, err := NewSecret([]byte(tt.other))
			if err != nil {
				t.Fatal(err)
			}
			equal, err := s.Equal(other)
			if err != nil || equal != tt.equal {
				t.Fatalf("Equal = %v, %v; want %v", equal, err, tt.equal)
			}
		})
	}
	out, err := s.AppendPlaintextTo([]byte("prefix:"))
	if err != nil {
		t.Fatal(err)
	}
	defer memguard.WipeBytes(out)
	if diff := gocmp.Diff("prefix:abc", string(out)); diff != "" {
		t.Fatal(diff)
	}
	callbackErr := errors.New("operation failed")
	if err := s.WithPlaintext(func([]byte) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v", err)
	}
	if err := s.WithPlaintext(nil); err == nil {
		t.Fatal("nil callback accepted")
	}
}

func TestSecretAbsent(t *testing.T) {
	tests := map[string]struct{ secret *Secret }{
		"error: nil":  {},
		"error: zero": {secret: &Secret{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := tt.secret
			if s.Len() != 0 {
				t.Fatal("absent secret has nonzero length")
			}
			if err := s.WithPlaintext(func([]byte) error { t.Fatal("absent secret opened"); return nil }); err == nil {
				t.Fatal("missing open error")
			}
			if _, err := s.Digest(); err == nil {
				t.Fatal("missing digest error")
			}
			if _, err := s.Digest8(); err == nil {
				t.Fatal("missing digest prefix error")
			}
			if _, err := s.Equal(s); err == nil {
				t.Fatal("missing equality error")
			}
			out, err := s.AppendPlaintextTo([]byte("prefix"))
			if err == nil || string(out) != "prefix" {
				t.Fatalf("exposure = %q, %v", out, err)
			}
			if s == nil && s.Clone() != nil {
				t.Fatal("nil clone must stay nil")
			}
		})
	}
}

func TestSecretConcurrentPlaintext(t *testing.T) {
	s, err := NewSecret([]byte("concurrent-test-token"))
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 8)
	release := make(chan struct{})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := s.WithPlaintext(func(b []byte) error {
				ready <- struct{}{}
				<-release
				if diff := gocmp.Diff([]byte("concurrent-test-token"), b); diff != "" {
					t.Errorf("concurrent plaintext (-want +got):\n%s", diff)
				}
				return nil
			}); err != nil {
				t.Errorf("concurrent open: %v", err)
			}
		})
	}
	for range 8 {
		<-ready
	}
	close(release)
	workers.Wait()
}

func TestSecretPlaintextDestroyed(t *testing.T) {
	if mode := os.Getenv("AGENTCTL_TEST_SECRET_LIFETIME"); mode != "" {
		s, err := NewSecret([]byte("lifetime-test-token"))
		if err != nil {
			t.Fatal(err)
		}
		var stale []byte
		callbackErr := errors.New("callback failed")
		func() {
			defer func() {
				got := recover()
				if mode == "panic" && got != callbackErr {
					t.Fatalf("panic = %v", got)
				}
				if mode != "panic" && got != nil {
					t.Fatalf("unexpected panic: %v", got)
				}
			}()
			err := s.WithPlaintext(func(b []byte) error {
				stale = b
				if mode == "panic" {
					panic(callbackErr)
				}
				if mode == "error" {
					return callbackErr
				}
				return nil
			})
			if mode == "error" && !errors.Is(err, callbackErr) {
				t.Fatalf("error = %v", err)
			}
			if mode == "return" && err != nil {
				t.Fatal(err)
			}
		}()
		// A saved alias must fault after the mapping is destroyed. This runs
		// in a child so a missing protection cannot corrupt the test process.
		previous := debug.SetPanicOnFault(true)
		defer debug.SetPanicOnFault(previous)
		faulted := false
		func() {
			defer func() { faulted = recover() != nil }()
			if stale[0] == 'l' {
				t.Error("plaintext remains readable after callback")
			}
		}()
		if !faulted {
			t.Fatal("destroyed plaintext mapping did not fault")
		}
		return
	}
	tests := map[string]struct{ mode string }{
		"success: return destroys":       {mode: "return"},
		"error: callback error destroys": {mode: "error"},
		"error: callback panic destroys": {mode: "panic"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSecretPlaintextDestroyed$", "-test.v")
			cmd.Env = append(os.Environ(), "AGENTCTL_TEST_SECRET_LIFETIME="+tt.mode)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("lifetime child: %v\n%s", err, output)
			}
		})
	}
}

var (
	_ fmt.Stringer           = Secret{}
	_ fmt.GoStringer         = Secret{}
	_ fmt.Formatter          = Secret{}
	_ slog.LogValuer         = Secret{}
	_ json.Marshaler         = Secret{}
	_ json.MarshalerTo       = Secret{}
	_ encoding.TextMarshaler = Secret{}
	_ encoding.TextAppender  = Secret{}
)
