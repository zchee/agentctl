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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestLoopbackCallback(t *testing.T) {
	tests := map[string]struct {
		line, code string
		fail       bool
	}{
		"success: callback":            {"GET /callback?code=hello&state=state HTTP/1.1\r\n", "hello", false},
		"success: last parameter wins": {"get /other?code=first&code=hello&state=wrong&state=state HTTP/1.1\r\n", "hello", false},
		"success: empty code accepted": {"GET /callback?code=&state=state HTTP/1.1\r\n", "", false},
		"error: wrong state":           {line: "GET /callback?code=hello&state=wrong HTTP/1.1\r\n", fail: true},
		"error: missing code":          {line: "GET /callback?state=state HTTP/1.1\r\n", fail: true},
		"error: refused":               {line: "GET /callback?error=access_denied HTTP/1.1\r\n", fail: true},
		"error: wrong method":          {line: "POST /callback?code=hello&state=state HTTP/1.1\r\n", fail: true},
		"error: malformed request":     {line: "garbage\r\n", fail: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			listener, err := ListenLoopback(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			if listener.Addr().(*net.TCPAddr).IP.String() != "127.0.0.1" {
				t.Fatal("listener is not IPv4 loopback")
			}
			done := make(chan struct{})
			var code string
			var waitErr error
			go func() {
				defer close(done)
				code, waitErr = LoopbackWait(t.Context(), listener, "state", time.Now().Add(time.Second))
			}()
			stream, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if err := stream.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stream, test.line); err != nil {
				t.Fatal(err)
			}
			response, err := io.ReadAll(stream)
			if err != nil {
				t.Fatal(err)
			}
			<-done
			if (waitErr != nil) != test.fail {
				t.Fatalf("callback error = %v", waitErr)
			}
			if diff := gocmp.Diff(test.code, code); diff != "" {
				t.Fatal(diff)
			}
			page, status := "<!doctype html><title>agentctl</title><p>Login complete. You can close this tab.", "200 OK"
			if test.fail {
				page, status = "<!doctype html><title>agentctl</title><p>Login failed. Return to the terminal.", "400 Bad Request"
			}
			want := fmt.Sprintf("HTTP/1.1 %s\r\ncontent-type: text/html; charset=utf-8\r\ncontent-length: %d\r\nconnection: close\r\n\r\n%s", status, len(page), page)
			if diff := gocmp.Diff(want, string(response)); diff != "" {
				t.Fatalf("callback page (-want +got): %s", diff)
			}
			second, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
			if err == nil {
				_ = second.Close()
				t.Fatal("listener accepted a second visit")
			}
		})
	}
}

func TestLoopbackDeadlines(t *testing.T) {
	tests := map[string]struct {
		cancel bool
		want   error
	}{
		"error: deadline":  {want: ErrLoopbackTimeout},
		"error: cancelled": {cancel: true, want: context.Canceled},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			listener, err := ListenLoopback(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if test.cancel {
				cancel()
			}
			_, err = LoopbackWait(ctx, listener, "state", time.Now().Add(10*time.Millisecond))
			if !errors.Is(err, test.want) {
				t.Fatalf("wait error = %v; want %v", err, test.want)
			}
		})
	}
	if LoopbackTimeout != 600*time.Second || LoopbackPollInterval != 250*time.Millisecond || CallbackReadTimeout != 10*time.Second {
		t.Fatal("callback budgets changed")
	}
}

func TestCallbackErrorsRedactTokens(t *testing.T) {
	_, err := parseCallback("GET /callback?error=sk-ant-planted HTTP/1.1", "state")
	if err == nil || strings.Contains(err.Error(), "sk-ant") {
		t.Fatal("callback error did not redact the planted token")
	}
}
