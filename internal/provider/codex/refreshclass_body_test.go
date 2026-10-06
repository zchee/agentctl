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
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// refreshCloseTransport observes the real transport's asynchronous body Close.
type refreshCloseTransport struct {
	base      http.RoundTripper
	closed    chan struct{}
	headers   chan struct{}
	closeGate <-chan struct{}
	cancel    context.CancelFunc
	body      *refreshBodyReader
}

func (r *refreshCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.body, _ = req.Body.(*refreshBodyReader)
	req.Body = &refreshCloseWitness{ReadCloser: req.Body, done: r.closed, gate: r.closeGate}
	resp, err := r.base.RoundTrip(req)
	if r.cancel != nil {
		r.cancel()
	}
	close(r.headers)
	return resp, err
}

type refreshCloseWitness struct {
	io.ReadCloser
	done chan struct{}
	gate <-chan struct{}
	once sync.Once
}

func (r *refreshCloseWitness) Close() error {
	if r.gate != nil {
		<-r.gate
	}
	err := r.ReadCloser.Close()
	r.once.Do(func() { close(r.done) })
	return err
}

func TestPostRefreshBodyCloseInterrupted(t *testing.T) {
	tests := map[string]struct {
		cancel  bool
		wantErr error
	}{
		"error: send-body wait expires":                   {wantErr: context.DeadlineExceeded},
		"error: context ends while body close is pending": {cancel: true, wantErr: context.Canceled},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, namespace, _, _ := writerNamespace(t)
			credentials := lockedRead(t, namespace)
			phases := DefaultPhaseTimeouts()
			phases.SendBody = 100 * time.Millisecond
			client := newRefreshClient(phases)
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			witness := &refreshCloseTransport{base: client.client.Transport, closed: make(chan struct{}), headers: make(chan struct{}), closeGate: release}
			if test.cancel {
				witness.cancel = cancel
			}
			client.client.Transport = witness
			endpoint, _ := rawEndpoint(t.Context(), t, func(conn net.Conn) {
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					t.Error(err)
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"); err != nil {
					t.Error(err)
				}
			})
			var out Outcome
			if err := credentials.WithRefreshBody(strings.Repeat("a", 8<<20), func(body []byte) error {
				out = client.post(ctx, endpoint, "agentctl/test", body)
				select {
				case <-witness.body.done:
				default:
					t.Error("interrupted wait left the refresh body readable")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(OutcomeUnknown, out.Kind); diff != "" {
				t.Fatalf("outcome (-want +got):\n%s", diff)
			}
			if !errors.Is(out.Err, test.wantErr) || out.AllowsAutomaticResend() {
				t.Fatalf("interrupted wait: error=%v resend=%t", out.Err, out.AllowsAutomaticResend())
			}
			// Late reads must not touch the body after the plaintext window is wiped.
			var buf [1]byte
			if n, err := witness.body.Read(buf[:]); n != 0 || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("read after body close: n=%d error=%v", n, err)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-witness.closed:
			case <-time.After(3 * time.Second):
				t.Fatal("real transport did not finish its delayed body close")
			}
		})
	}
}

func TestPostRefreshBodyClosedBeforeWipe(t *testing.T) {
	tests := map[string]struct {
		earlyResponse bool
		want          OutcomeKind
	}{
		"success: headers arrive before body is consumed": {earlyResponse: true, want: OutcomeResponse},
		"error: connection closes during request body":    {want: OutcomeUnknown},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, namespace, _, _ := writerNamespace(t)
			credentials := lockedRead(t, namespace)
			client := newRefreshClient()
			witness := &refreshCloseTransport{base: client.client.Transport, closed: make(chan struct{}), headers: make(chan struct{})}
			client.client.Transport = witness
			transport := witness.base.(*http.Transport)
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
			endpoint, calls := rawEndpoint(ctx, t, func(conn net.Conn) {
				if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
					t.Error(err)
					return
				}
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					t.Error(err)
					return
				}
				if test.earlyResponse {
					if _, err := io.WriteString(conn, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"); err != nil {
						t.Error(err)
						return
					}
					select {
					case <-witness.headers:
					case <-ctx.Done():
						t.Error("transport did not return the early response")
					}
				} else {
					var prefix [1]byte
					if _, err := io.ReadFull(req.Body, prefix[:]); err != nil {
						t.Error(err)
					}
				}
			})
			var out Outcome
			// A large client identifier forces the real socket write to overlap the response.
			if err := credentials.WithRefreshBody(strings.Repeat("a", 8<<20), func(body []byte) error {
				out = client.post(ctx, endpoint, "agentctl/test", body)
				select {
				case <-witness.closed:
				default:
					t.Error("POST returned while the transport still owned the refresh body")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(test.want, out.Kind); diff != "" {
				t.Fatalf("outcome (-want +got):\n%s; error=%v trace=%+v", diff, out.Err, out.Trace)
			}
			if out.AllowsAutomaticResend() || calls.Load() != 1 {
				t.Fatalf("resend=%t attempts=%d; interrupted writes cannot authorize a resend", out.AllowsAutomaticResend(), calls.Load())
			}
		})
	}
}
