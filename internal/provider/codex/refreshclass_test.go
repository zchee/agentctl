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
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

var refreshBody = []byte(`{"grant_type":"refresh_token","refresh_token":"test-token"}`)

func TestDefaultPhaseTimeouts(t *testing.T) {
	t.Parallel()
	want := PhaseTimeouts{2 * time.Second, 3 * time.Second, 2 * time.Second, 2 * time.Second, 8 * time.Second, 2 * time.Second}
	if diff := gocmp.Diff(want, DefaultPhaseTimeouts()); diff != "" {
		t.Errorf("phase bounds (-want +got):\n%s", diff)
	}
	c := newRefreshClient()
	transport := c.client.Transport.(*http.Transport)
	if c.client.Timeout != 0 || c.client.Jar != nil || !transport.DisableKeepAlives || transport.MaxIdleConns != 0 {
		t.Fatal("client must have no overall timeout, cookie jar, or reusable connection")
	}
	if transport.TLSHandshakeTimeout != want.Connect || transport.ResponseHeaderTimeout != want.RecvResponse {
		t.Fatal("transport phase bounds differ from production constants")
	}
	if !transport.Protocols.HTTP1() || transport.Protocols.HTTP2() || transport.Protocols.UnencryptedHTTP2() {
		t.Fatal("only HTTP/1.1 may be negotiated")
	}
	if err := c.client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("redirect policy = %v, want ErrUseLastResponse", err)
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		err   error
		trace Observed
		kind  OutcomeKind
		class UnknownClass
	}{
		"success: refused":                            {err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, kind: OutcomeNotSent},
		"success: DNS absence":                        {err: &net.DNSError{IsNotFound: true}, kind: OutcomeNotSent},
		"success: DNS timeout":                        {err: &net.DNSError{IsTimeout: true}, kind: OutcomeNotSent},
		"success: dial timeout":                       {err: &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, kind: OutcomeNotSent},
		"success: TLS timeout":                        {err: os.ErrDeadlineExceeded, trace: Observed{TLSStarted: true, TLSErr: os.ErrDeadlineExceeded}, kind: OutcomeNotSent},
		"error: unreachable remains unknown":          {err: &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}},
		"error: reset without callbacks":              {err: syscall.ECONNRESET},
		"error: broken pipe without callbacks":        {err: syscall.EPIPE},
		"error: truncated response":                   {err: io.ErrUnexpectedEOF, trace: Observed{WroteRequest: true}},
		"error: closed connection":                    {err: net.ErrClosed},
		"error: deadline without phase proof":         {err: context.DeadlineExceeded},
		"error: cancellation without phase proof":     {err: context.Canceled},
		"error: failed write callback is not absence": {err: syscall.ECONNREFUSED, trace: Observed{WroteRequest: true, WroteRequestErr: syscall.ECONNREFUSED}},
		"error: response byte defeats pre-send proof": {err: syscall.ECONNREFUSED, trace: Observed{GotFirstResponseByte: true}},
		"error: certificate failure":                  {err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, class: UnknownTLS},
		"error: TLS record failure":                   {err: tls.RecordHeaderError{}, trace: Observed{TLSStarted: true, TLSOK: true}, class: UnknownTLS},
		"error: handshake EOF is transport":           {err: io.EOF, trace: Observed{TLSStarted: true, TLSErr: io.EOF}},
		"error: nil is fail closed":                   {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tt.err
			if err != nil {
				err = &url.Error{Op: "Post", URL: "http://test.invalid/", Err: err}
			}
			out := Classify(err, tt.trace)
			if out.Kind != tt.kind || out.Class != tt.class {
				t.Fatalf("kind/class = %d/%d, want %d/%d; error %v, trace %+v", out.Kind, out.Class, tt.kind, tt.class, err, tt.trace)
			}
			if got, want := out.AllowsAutomaticResend(), tt.kind == OutcomeNotSent; got != want {
				t.Errorf("automatic resend = %t, want %t", got, want)
			}
		})
	}
}

// rawEndpoint uses real loopback sockets; the accept count detects hidden retries.
func rawEndpoint(ctx context.Context, t *testing.T, serve func(net.Conn)) (string, *atomic.Int32) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	count := new(atomic.Int32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Error(err)
			}
			serve(conn)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("raw endpoint did not stop within its connection deadline")
		}
	})
	return "http://" + ln.Addr().String(), count
}

func readRequest(ctx context.Context, t *testing.T, conn net.Conn) {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Error(err)
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = req.Body.Close() }()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Error(err)
	}
	if diff := gocmp.Diff(refreshBody, body); diff != "" {
		t.Errorf("received request (-want +got):\n%s", diff)
	}
}

// tracedTransport signals after the real transport parsed the response headers.
// This lets the server reset mid-body without timing-dependent sleeps.
type tracedTransport struct {
	base    http.RoundTripper
	headers chan struct{}
}

func (t *tracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	close(t.headers)
	return resp, err
}

func TestPostFaultMatrix(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		fault string
		kind  OutcomeKind
		class UnknownClass
	}{
		"success: refused before sending":     {fault: "refused", kind: OutcomeNotSent},
		"error: no response headers":          {fault: "headers"},
		"error: close after complete request": {fault: "close"},
		"error: reset after complete request": {fault: "reset"},
		"error: reset in response body":       {fault: "body reset", kind: OutcomeResponse},
		"error: incomplete response body":     {fault: "body eof", kind: OutcomeResponse},
		"error: stalled response body":        {fault: "body timeout", kind: OutcomeResponse},
		"error: untrusted certificate":        {fault: "certificate", class: UnknownTLS},
		"error: fatal TLS alert":              {fault: "tls alert", class: UnknownTLS},
		"success: TLS handshake timeout":      {fault: "tls timeout", kind: OutcomeNotSent},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			phases := DefaultPhaseTimeouts()
			phases.Connect = 250 * time.Millisecond
			phases.RecvResponse = 250 * time.Millisecond
			phases.RecvBody = 250 * time.Millisecond
			c := newRefreshClient(phases)
			headers := make(chan struct{})
			c.client.Transport = &tracedTransport{base: c.client.Transport, headers: headers}
			var endpoint string
			var count *atomic.Int32
			switch tt.fault {
			case "refused":
				ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				endpoint = "http://" + ln.Addr().String()
				_ = ln.Close()
			case "certificate":
				count = new(atomic.Int32)
				srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { count.Add(1) }))
				t.Cleanup(srv.Close)
				endpoint = srv.URL
			default:
				endpoint, count = rawEndpoint(ctx, t, func(conn net.Conn) {
					if strings.HasPrefix(tt.fault, "tls ") {
						var hello [5]byte
						if _, err := io.ReadFull(conn, hello[:]); err != nil {
							t.Error(err)
							return
						}
						if hello[0] != 0x16 {
							t.Errorf("first record type = %d, want handshake", hello[0])
						}
						if tt.fault == "tls timeout" {
							<-ctx.Done()
						} else if _, err := conn.Write([]byte{0x15, 0x03, 0x03, 0, 2, 2, 40}); err != nil {
							t.Error(err)
						}
						return
					}
					readRequest(ctx, t, conn)
					switch tt.fault {
					case "headers":
						<-ctx.Done()
					case "close":
					case "reset":
						if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
							t.Error(err)
						}
					case "body reset", "body eof", "body timeout":
						if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\nab"); err != nil {
							t.Error(err)
							return
						}
						select {
						case <-headers:
						case <-ctx.Done():
							return
						}
						switch tt.fault {
						case "body timeout":
							<-ctx.Done()
						case "body reset":
							if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
								t.Error(err)
							}
						}
					}
				})
				if strings.HasPrefix(tt.fault, "tls ") {
					endpoint = strings.Replace(endpoint, "http:", "https:", 1)
				}
			}
			out := c.post(ctx, endpoint, "test", refreshBody)
			if out.Kind != tt.kind || out.Class != tt.class {
				t.Fatalf("kind/class=%d/%d want=%d/%d err=%v bodyErr=%v trace=%+v", out.Kind, out.Class, tt.kind, tt.class, out.Err, out.BodyErr, out.Trace)
			}
			if out.AllowsAutomaticResend() != (tt.kind == OutcomeNotSent) {
				t.Error("automatic resend must remain forbidden without pre-send proof")
			}
			wantCalls := int32(1)
			if tt.fault == "certificate" {
				wantCalls = 0
			}
			if count != nil && count.Load() != wantCalls {
				t.Errorf("server observed %d attempts, want %d; transport must not retry", count.Load(), wantCalls)
			}
			err := out.Err
			if tt.kind == OutcomeResponse {
				err = out.BodyErr
				if out.Status != 200 || out.BodyComplete || out.Body != nil || err == nil {
					t.Errorf("partial response must retain status but discard body: status=%d complete=%t err=%v", out.Status, out.BodyComplete, err)
				}
			}
			switch tt.fault {
			case "refused":
				if !errors.Is(err, syscall.ECONNREFUSED) {
					t.Errorf("want ECONNREFUSED, got %v", err)
				}
			case "reset", "body reset":
				if !errors.Is(err, syscall.ECONNRESET) {
					t.Errorf("want ECONNRESET, got %v", err)
				}
			case "close":
				if !errors.Is(err, io.EOF) {
					t.Errorf("want EOF, got %v", err)
				}
			case "body eof":
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("want unexpected EOF, got %v", err)
				}
			case "headers", "body timeout", "tls timeout":
				if !isTimeout(err) {
					t.Errorf("want timeout, got %v", err)
				}
			case "certificate":
				if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
					t.Errorf("want certificate verification error, got %v", err)
				}
			}
			var chain []string
			for next := err; next != nil; next = errors.Unwrap(next) {
				chain = append(chain, fmt.Sprintf("%T", next))
			}
			t.Logf("chain=%s; deadline=%t unexpectedEOF=%t reset=%t refused=%t; kind/class=%d/%d; trace=%+v", strings.Join(chain, " -> "), errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED), out.Kind, out.Class, out.Trace)
		})
	}
}

func TestPostResponses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		status           int
		body, retryAfter string
	}{
		"success: empty unauthorized":       {401, "", ""},
		"success: empty success":            {204, "", ""},
		"success: grant":                    {200, `{"access_token":"test-access"}`, ""},
		"success: unauthorized":             {401, `{}`, ""},
		"success: rate limited":             {429, `{}`, "120"},
		"success: server error":             {503, `{}`, ""},
		"success: redirect is not followed": {307, `{}`, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/token" || r.Header.Get("Cookie") != "" {
					t.Errorf("unexpected request: %s %s cookie=%q", r.Method, r.URL.Path, r.Header.Get("Cookie"))
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.Header().Set("Set-Cookie", "session=test; Path=/")
				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			c := newRefreshClient()
			for range 2 {
				out := c.post(t.Context(), srv.URL+"/token", "test", refreshBody)
				if out.Kind != OutcomeResponse || out.Status != tt.status || !out.BodyComplete || string(out.Body) != tt.body || out.Header.Get("Retry-After") != tt.retryAfter {
					t.Fatalf("response mismatch: kind=%d status=%d complete=%t err=%v bodyErr=%v", out.Kind, out.Status, out.BodyComplete, out.Err, out.BodyErr)
				}
				if out.AllowsAutomaticResend() {
					t.Error("an HTTP response requires policy settlement, not automatic resend")
				}
			}
			if calls.Load() != 2 {
				t.Errorf("received %d calls, want exactly two explicit sends", calls.Load())
			}
		})
	}
}

func TestPostResponseLimit(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(maxRefreshResponseBytes)+1))
	}))
	t.Cleanup(srv.Close)
	out := newRefreshClient().post(t.Context(), srv.URL, "test", refreshBody)
	if out.Kind != OutcomeResponse || out.BodyComplete || out.Body != nil || !errors.Is(out.BodyErr, errResponseBodyOverCap) {
		t.Fatalf("oversized body: kind=%d complete=%t err=%v bodyErr=%v", out.Kind, out.BodyComplete, out.Err, out.BodyErr)
	}
}

func TestPostCancellationBeforeSend(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out := newRefreshClient().post(ctx, srv.URL, "test", refreshBody)
	if out.Kind != OutcomeNotSent || calls.Load() != 0 || !out.AllowsAutomaticResend() {
		t.Errorf("cancelled attempt: kind=%d calls=%d", out.Kind, calls.Load())
	}
}

func TestPostHTTP1ToHTTP2Server(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("protocol=%s, want HTTP/1.1", r.Proto)
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c := newRefreshClient()
	c.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	out := c.post(t.Context(), srv.URL, "test", refreshBody)
	if out.Kind != OutcomeResponse || !out.BodyComplete || !out.Trace.TLSOK {
		t.Fatalf("trusted TLS request failed: kind=%d err=%v bodyErr=%v", out.Kind, out.Err, out.BodyErr)
	}
}

func TestPostTLSRecordFailures(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		record []byte
	}{
		"error: invalid record version":           {record: []byte{0x17, 0x99, 0x99, 0, 1, 0}},
		"error: invalid authenticated ciphertext": {record: append([]byte{0x17, 0x03, 0x03, 0, 32}, make([]byte, 32)...)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				raw := conn.(*tls.Conn).NetConn()
				defer func() { _ = raw.Close() }()
				if _, err := raw.Write(tt.record); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(srv.Close)
			roots := x509.NewCertPool()
			roots.AddCert(srv.Certificate())
			c := newRefreshClient()
			c.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
			out := c.post(t.Context(), srv.URL, "test", refreshBody)
			if out.Kind != OutcomeUnknown || out.Class != UnknownTLS || out.AllowsAutomaticResend() || !out.Trace.TLSOK || calls.Load() != 1 {
				t.Fatalf("TLS record failure: kind/class=%d/%d calls=%d err=%v trace=%+v", out.Kind, out.Class, calls.Load(), out.Err, out.Trace)
			}
			for err := out.Err; err != nil; err = errors.Unwrap(err) {
				t.Logf("error chain: %T", err)
			}
		})
	}
}

func TestPostWriteDeadlines(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ header, body bool }{
		"error: blocked request headers": {header: true},
		"error: blocked request body":    {body: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			endpoint, count := rawEndpoint(ctx, t, func(conn net.Conn) {
				if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
					t.Error(err)
				}
				<-ctx.Done()
			})
			phases := DefaultPhaseTimeouts()
			phases.SendRequest, phases.SendBody = 150*time.Millisecond, 150*time.Millisecond
			c := newRefreshClient(phases)
			transport := c.client.Transport.(*http.Transport)
			dial := transport.DialContext
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err == nil {
					if err := conn.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
						_ = conn.Close()
						return nil, err
					}
				}
				return conn, err
			}
			body, userAgent := refreshBody, "test"
			if tt.header {
				userAgent = strings.Repeat("a", 8<<20)
			}
			if tt.body {
				body = bytes.Repeat([]byte("x"), 8<<20)
			}
			out := c.post(ctx, endpoint, userAgent, body)
			if out.Kind != OutcomeUnknown || out.AllowsAutomaticResend() || !isTimeout(out.Err) || ctx.Err() != nil {
				t.Fatalf("write timeout: kind=%d err=%v context=%v trace=%+v", out.Kind, out.Err, ctx.Err(), out.Trace)
			}
			if out.Trace.WroteHeaders != tt.body {
				t.Errorf("headers-written=%t, want %t", out.Trace.WroteHeaders, tt.body)
			}
			if count.Load() != 1 {
				t.Errorf("accepted %d connections, want one", count.Load())
			}
		})
	}
}

func TestPostPreservesCallerTrace(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	var headers atomic.Bool
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{WroteHeaders: func() { headers.Store(true) }})
	out := newRefreshClient().post(ctx, srv.URL, "test", refreshBody)
	if out.Kind != OutcomeResponse || !headers.Load() {
		t.Fatalf("header trace missing: kind=%d err=%v", out.Kind, out.Err)
	}
}
