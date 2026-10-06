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
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"syscall"
	"time"
)

const (
	// TimeoutResolve bounds name resolution.
	TimeoutResolve = 2 * time.Second
	// TimeoutConnect bounds TCP connection establishment and TLS together.
	TimeoutConnect = 3 * time.Second
	// TimeoutSendRequest bounds writing the request line and headers.
	TimeoutSendRequest = 2 * time.Second
	// TimeoutSendBody bounds writing the request body.
	TimeoutSendBody = 2 * time.Second
	// TimeoutRecvResponse bounds receiving the complete response headers.
	TimeoutRecvResponse = 8 * time.Second
	// TimeoutRecvBody bounds reading the complete response body.
	TimeoutRecvBody = 2 * time.Second
)

const maxRefreshResponseBytes int64 = 256 << 10

var errResponseBodyOverCap = errors.New("the response body exceeds the readable cap")

// PhaseTimeouts bounds each phase independently; there is no overall deadline.
type PhaseTimeouts struct {
	Resolve, Connect, SendRequest, SendBody, RecvResponse, RecvBody time.Duration
}

// DefaultPhaseTimeouts returns the production bounds in request order.
func DefaultPhaseTimeouts() PhaseTimeouts {
	return PhaseTimeouts{TimeoutResolve, TimeoutConnect, TimeoutSendRequest, TimeoutSendBody, TimeoutRecvResponse, TimeoutRecvBody}
}

// OutcomeKind distinguishes send certainty from response policy.
type OutcomeKind uint8

const (
	// OutcomeUnknown is the fail-closed default: the token may be consumed.
	OutcomeUnknown OutcomeKind = iota
	// OutcomeNotSent proves that no HTTP request was sent.
	OutcomeNotSent
	// OutcomeResponse carries an HTTP response, not proof the grant survived.
	// A 429, 5xx, redirect, or unusable 2xx still needs an unknown marker.
	OutcomeResponse
)

// UnknownClass distinguishes transport failures from TLS failures.
type UnknownClass uint8

const (
	// UnknownTransport covers unclassified errors and interrupted transfers.
	UnknownTransport UnknownClass = iota
	// UnknownTLS covers certificate, handshake, and TLS record failures.
	UnknownTLS
)

// Observed is a snapshot of a single request's trace milestones.
// Missing write callbacks do not prove absence: callbacks can arrive late.
type Observed struct {
	ConnectStarted, TLSStarted, TLSOK, WroteHeaders, WroteRequest, GotFirstResponseByte bool
	TLSErr, WroteRequestErr                                                             error
}

// Outcome retains the response facts needed by the refresh policy.
// Body is nil unless read completely; callers must not log it or Header.
// Response does not imply a successful refresh or permission to resend.
type Outcome struct {
	Kind         OutcomeKind
	Class        UnknownClass
	Reason       string
	Status       int
	Header       http.Header
	Body         []byte
	BodyComplete bool
	BodyErr      error
	Err          error
	Trace        Observed
}

// AllowsAutomaticResend reports whether this attempt alone proves a retry safe.
// It does not override a durable unknown marker from an earlier attempt.
// Responses require the caller's status/body policy and are never allowed here.
func (o Outcome) AllowsAutomaticResend() bool { return o.Kind == OutcomeNotSent }

// Classify classifies a failed POST. Only resolve/connect proof without a write
// milestone permits automatic resend. TLS failures remain unknown even when a
// handshake failed before sending; a connect-phase timeout instead proves not-sent.
func Classify(err error, trace Observed) Outcome {
	out := Outcome{Kind: OutcomeUnknown, Err: err, Trace: trace, Reason: "the request may have reached the server"}
	wrote := trace.WroteHeaders || trace.WroteRequest || trace.GotFirstResponseByte
	if err == nil {
		return out
	}
	if !wrote {
		dns, isDNS := errors.AsType[*net.DNSError](err)
		op, isOp := errors.AsType[*net.OpError](err)
		switch {
		case isDNS:
			out.Kind, out.Reason = OutcomeNotSent, "name resolution failed"
		case errors.Is(err, syscall.ECONNREFUSED):
			out.Kind, out.Reason = OutcomeNotSent, "connection refused"
		case isOp && op.Op == "dial" && isTimeout(err):
			out.Kind, out.Reason = OutcomeNotSent, "timed out connecting"
		case trace.TLSStarted && !trace.TLSOK && isTimeout(trace.TLSErr):
			out.Kind, out.Reason = OutcomeNotSent, "timed out connecting"
		}
		if isDNS && dns.IsNotFound {
			out.Reason = "host not found"
		} else if isDNS && dns.IsTimeout {
			out.Reason = "timed out resolving the host"
		}
		if out.Kind == OutcomeNotSent {
			return out
		}
	}
	// Network errors inside a TLS connection are not themselves TLS failures.
	switch {
	case isTimeout(err):
		out.Reason = "timed out after the request may have gone out"
	case errors.Is(err, syscall.ECONNRESET):
		out.Reason = "the connection was reset"
	case errors.Is(err, syscall.EPIPE):
		out.Reason = "the connection broke while writing"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), errors.Is(err, context.Canceled):
		out.Reason = "the transfer ended before a complete response"
	default:
		_, certificate := errors.AsType[*tls.CertificateVerificationError](err)
		_, record := errors.AsType[tls.RecordHeaderError](err)
		op, isOp := errors.AsType[*net.OpError](err)
		// crypto/tls wraps its private alert type in these operation names.
		alert := isOp && trace.TLSStarted && (op.Op == "local error" || op.Op == "remote error")
		if certificate || record || alert || (trace.TLSStarted && !trace.TLSOK && trace.TLSErr != nil) {
			out.Class, out.Reason = UnknownTLS, "a TLS failure ended the request"
		}
	}
	return out
}

func isTimeout(err error) bool {
	if err, ok := errors.AsType[*url.Error](err); ok && err.Timeout() {
		return true
	}
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Timeout() {
		return true
	}
	netErr, ok := errors.AsType[net.Error](err)
	return errors.Is(err, context.DeadlineExceeded) || (ok && netErr.Timeout())
}

type observer struct {
	mu   sync.Mutex
	seen Observed
	conn net.Conn
}

func (o *observer) snapshot() Observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seen
}

func (o *observer) clientTrace(phases PhaseTimeouts) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectStart: func(string, string) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.seen.ConnectStarted = true
		},
		TLSHandshakeStart: func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.seen.TLSStarted = true
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.seen.TLSOK, o.seen.TLSErr = err == nil, err
		},
		GotConn: func(info httptrace.GotConnInfo) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.conn = info.Conn
			if err := o.conn.SetDeadline(time.Time{}); err != nil {
				_ = o.conn.Close()
				return
			}
			if err := o.conn.SetWriteDeadline(time.Now().Add(phases.SendRequest)); err != nil {
				_ = o.conn.Close()
			}
		},
		WroteHeaders: func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.seen.WroteHeaders = true
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.seen.WroteRequest, o.seen.WroteRequestErr = true, info.Err
		},
		GotFirstResponseByte: func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.seen.GotFirstResponseByte = true
		},
	}
}

// refreshBodyReader hides the in-memory reader's type so net/http flushes
// headers before its first Read. WroteHeaders alone fires before that flush.
// The body deadline starts once, after the header write has completed.
type refreshBodyReader struct {
	mu      sync.Mutex
	reader  *bytes.Reader
	observe *observer
	budget  time.Duration
	started bool
	done    chan struct{}
	once    sync.Once
}

func (r *refreshBodyReader) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		// Cancellation may close the body before the transport's read finishes.
		r.reader = nil
		close(r.done)
	})
	return nil
}

func (r *refreshBodyReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader == nil {
		return 0, net.ErrClosed
	}
	if !r.started {
		r.started = true
		if err := r.observe.conn.SetWriteDeadline(time.Now().Add(r.budget)); err != nil {
			return 0, err
		}
	}
	return r.reader.Read(p)
}

type refreshClient struct {
	client *http.Client
	phases PhaseTimeouts
}

// newRefreshClient constructs a single-send HTTP/1.1 client. net/http retries
// idempotent requests only on reused connections; disabling keep-alives removes
// that path. POST bodies have no GetBody, no idempotency key, and are never retried
// by this transport. Redirects return their original response without a resend.
// The optional bounds shorten tests without a whole-request deadline.
func newRefreshClient(bounds ...PhaseTimeouts) *refreshClient {
	phases := DefaultPhaseTimeouts()
	if len(bounds) != 0 {
		phases = bounds[0]
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialRefresh(ctx, network, address, phases)
		},
		TLSHandshakeTimeout:   phases.Connect,
		ResponseHeaderTimeout: phases.RecvResponse,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		Protocols:             protocols,
	}
	return &refreshClient{
		phases: phases,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// post sends exactly one JSON body. Its caller must first persist the send
// marker, and must settle a received status/body before clearing that marker.
func (c *refreshClient) post(ctx context.Context, endpoint, userAgent string, body []byte) Outcome {
	if err := ctx.Err(); err != nil {
		return Outcome{Kind: OutcomeNotSent, Reason: "cancelled before the send", Err: err}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return Outcome{Kind: OutcomeNotSent, Reason: "the request could not be built", Err: err}
	}
	obs := new(observer)
	req.ContentLength = int64(len(body))
	var requestBody *refreshBodyReader
	if len(body) != 0 {
		requestBody = &refreshBodyReader{reader: bytes.NewReader(body), observe: obs, budget: c.phases.SendBody, done: make(chan struct{})}
		req.Body = requestBody
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req = req.WithContext(httptrace.WithClientTrace(ctx, obs.clientTrace(c.phases)))
	resp, err := c.client.Do(req)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if requestBody != nil {
		// A response or transport error can precede the asynchronous body Close.
		waitCtx, stop := context.WithTimeout(ctx, c.phases.SendBody)
		var waitErr error
		select {
		case <-requestBody.done:
		default:
			select {
			case <-requestBody.done:
			case <-waitCtx.Done():
				waitErr = waitCtx.Err()
			}
		}
		stop()
		if waitErr != nil {
			cancel()
			_ = requestBody.Close()
			return Outcome{Kind: OutcomeUnknown, Reason: "the request body did not finish closing", Err: waitErr, Trace: obs.snapshot()}
		}
	}
	if err != nil {
		return Classify(err, obs.snapshot())
	}
	out := Outcome{Kind: OutcomeResponse, Status: resp.StatusCode, Header: resp.Header.Clone()}
	if resp.Body == http.NoBody {
		out.BodyComplete = true
		out.Trace = obs.snapshot()
		return out
	}
	if err := obs.conn.SetReadDeadline(time.Now().Add(c.phases.RecvBody)); err != nil {
		out.BodyErr = err
	} else {
		out.Body, out.BodyErr = io.ReadAll(io.LimitReader(resp.Body, maxRefreshResponseBytes+1))
		if int64(len(out.Body)) > maxRefreshResponseBytes {
			out.BodyErr = errResponseBodyOverCap
		}
	}
	out.BodyComplete = out.BodyErr == nil
	if !out.BodyComplete {
		clear(out.Body)
		out.Body = nil
	}
	out.Trace = obs.snapshot()
	return out
}

func dialRefresh(ctx context.Context, network, address string, phases PhaseTimeouts) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	targets := []string{address}
	if net.ParseIP(host) == nil {
		resolveCtx, cancel := context.WithTimeout(ctx, phases.Resolve)
		addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
		cancel()
		if err != nil {
			return nil, err
		}
		targets = targets[:0]
		for _, addr := range addrs {
			targets = append(targets, net.JoinHostPort(addr.String(), port))
		}
	}
	// One absolute deadline covers every address attempt and the TLS handshake.
	deadline := time.Now().Add(phases.Connect)
	dialer := &net.Dialer{Deadline: deadline}
	var lastErr error
	for _, target := range targets {
		conn, err := dialer.DialContext(ctx, network, target)
		if err != nil {
			lastErr = err
			continue
		}
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	return nil, lastErr
}
