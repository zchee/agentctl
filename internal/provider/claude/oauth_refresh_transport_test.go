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
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider"
)

func TestOAuthTokenPOSTDoesNotFollowRedirects(t *testing.T) {
	tests := map[string]struct {
		status int
	}{
		"error: moved permanently":  {http.StatusMovedPermanently},
		"error: found":              {http.StatusFound},
		"error: see other":          {http.StatusSeeOther},
		"error: temporary redirect": {http.StatusTemporaryRedirect},
		"error: permanent redirect": {http.StatusPermanentRedirect},
	}
	grants := map[string]struct{ login bool }{
		"refresh":            {},
		"authorization code": {login: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for grant, mode := range grants {
				t.Run(grant, func(t *testing.T) {
					var firstCalls, secondCalls atomic.Int64
					second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						secondCalls.Add(1)
						_, _ = io.WriteString(w, `{"access_token":"unexpected","expires_in":60}`)
					}))
					defer second.Close()
					first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						firstCalls.Add(1)
						body, err := io.ReadAll(r.Body)
						if err != nil || r.Method != http.MethodPost {
							t.Errorf("first request: method=%s read error=%v", r.Method, err)
						}
						if mode.login {
							if !bytes.Contains(body, []byte(`"grant_type":"authorization_code"`)) || !bytes.Contains(body, []byte(`"code_verifier":`)) {
								t.Error("the first POST did not carry the authorization grant")
							}
						} else if !bytes.Equal(body, []byte(storedRefreshBody)) {
							t.Error("the first POST did not carry the stored refresh grant")
						}
						w.Header().Set("Location", second.URL)
						w.WriteHeader(test.status)
					}))
					defer first.Close()
					client := oauthClientFor(t, first.URL)
					var err error
					if mode.login {
						pkce, pkceErr := NewPKCE()
						if pkceErr != nil {
							t.Fatal(pkceErr)
						}
						_, err = (&LoginClient{OAuthClient: client}).exchange(t.Context(), "one-shot-code", "state", pkce, Redirect{}, time.Millisecond)
					} else {
						_, err = client.RefreshAccess(t.Context(), storedCredentials(t))
					}
					httpErr, ok := errors.AsType[*errs.HTTPError](err)
					if !ok || httpErr.Status != test.status {
						t.Fatalf("outcome=%v, want HTTPError with original status %d", err, test.status)
					}
					if diff := gocmp.Diff([2]int64{1, 0}, [2]int64{firstCalls.Load(), secondCalls.Load()}); diff != "" {
						t.Errorf("requests by origin (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

// oauthCloseTransport witnesses a real transport without replacing its I/O.
type oauthCloseTransport struct {
	base    http.RoundTripper
	body    *oauthBodyReader
	payload bytes.Reader
	headers chan struct{}
	closing chan struct{}
	gate    <-chan struct{}
}

func (r *oauthCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.body, _ = req.Body.(*oauthBodyReader)
	if r.body == nil || req.GetBody != nil {
		return nil, errors.New("token POST body is replayable or unsynchronized")
	}
	r.payload = *r.body.reader
	req.Body = &oauthCloseWitness{ReadCloser: req.Body, closing: r.closing, gate: r.gate}
	response, err := r.base.RoundTrip(req)
	close(r.headers)
	return response, err
}

type oauthCloseWitness struct {
	io.ReadCloser
	closing chan struct{}
	gate    <-chan struct{}
	once    sync.Once
}

func (r *oauthCloseWitness) Close() error {
	r.once.Do(func() { close(r.closing) })
	if r.gate != nil {
		<-r.gate
	}
	return r.ReadCloser.Close()
}

func oauthRawEndpoint(t *testing.T, ctx context.Context, headers <-chan struct{}, earlyResponse bool) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		calls.Add(1)
		if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
			t.Error(err)
			return
		}
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			t.Error(err)
			return
		}
		if earlyResponse {
			payload := `{"access_token":"rotated","expires_in":60}`
			if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: "+strconv.Itoa(len(payload))+"\r\nConnection: close\r\n\r\n"+payload); err != nil {
				t.Error(err)
				return
			}
			select {
			case <-headers:
			case <-ctx.Done():
				t.Error("real transport did not return the early response")
			}
		} else {
			var prefix [1]byte
			if _, err := io.ReadFull(req.Body, prefix[:]); err != nil {
				t.Error(err)
			}
		}
	})
	t.Cleanup(func() { _ = listener.Close(); wg.Wait() })
	return "http://" + listener.Addr().String(), &calls
}

func TestOAuthTokenPOSTBodyClosedBeforeWipe(t *testing.T) {
	tests := map[string]struct{ earlyResponse, login bool }{
		"success: refresh response before upload finishes":  {earlyResponse: true},
		"error: refresh disconnect during upload":           {},
		"success: exchange response before upload finishes": {earlyResponse: true, login: true},
		"error: exchange disconnect during upload":          {login: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.DisableKeepAlives = true
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err == nil {
					if err := conn.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
						_ = conn.Close()
						return nil, err
					}
				}
				return conn, err
			}
			witness := &oauthCloseTransport{base: transport, headers: make(chan struct{}), closing: make(chan struct{})}
			endpoint, calls := oauthRawEndpoint(t, ctx, witness.headers, test.earlyResponse)
			client := oauthClientFor(t, endpoint)
			client.tokenClient.Transport = witness
			client.clientID = strings.Repeat("a", 8<<20)
			var err error
			if test.login {
				pkce, pkceErr := NewPKCE()
				if pkceErr != nil {
					t.Fatal(pkceErr)
				}
				_, err = (&LoginClient{OAuthClient: client}).exchange(ctx, "one-shot-code", "state", pkce, Redirect{}, time.Millisecond)
			} else {
				_, err = client.RefreshAccess(ctx, storedCredentials(t))
			}
			if test.earlyResponse {
				if err != nil {
					t.Fatalf("early response outcome=%v, want success", err)
				}
			} else if fetchErr, ok := errors.AsType[*provider.FetchError](err); !ok || fetchErr.Kind != provider.FetchTransport {
				t.Fatalf("disconnect outcome=%v, want uncertain transport failure", err)
			}
			select {
			case <-witness.body.done:
			default:
				t.Fatal("POST returned before the request body was closed")
			}
			var buf, zeros [4096]byte
			for {
				n, readErr := witness.payload.Read(buf[:])
				if !bytes.Equal(buf[:n], zeros[:n]) {
					t.Fatal("POST body was not wiped after Close")
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					t.Fatal(readErr)
				}
			}
			if n, err := witness.body.Read(buf[:]); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("late Read=%d, %v; closed body must not read wiped bytes", n, err)
			}
			if diff := gocmp.Diff(int64(1), calls.Load()); diff != "" {
				t.Errorf("attempt count (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOAuthTokenPOSTCloseWaitInterrupted(t *testing.T) {
	tests := map[string]struct{ cancel bool }{
		"error: whole-request deadline bounds body close": {},
		"error: cancellation while body close is pending": {cancel: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.DisableKeepAlives = true
			witness := &oauthCloseTransport{base: transport, headers: make(chan struct{}), closing: make(chan struct{}), gate: release}
			endpoint, _ := oauthRawEndpoint(t, ctx, witness.headers, true)
			client := oauthClientFor(t, endpoint)
			client.tokenClient.Transport = witness
			result := make(chan error, 1)
			credentials := storedCredentials(t)
			go func() { _, err := client.RefreshAccess(ctx, credentials); result <- err }()
			select {
			case <-witness.headers:
			case <-ctx.Done():
				t.Fatal("transport did not return response headers")
			}
			select {
			case <-witness.closing:
			case <-ctx.Done():
				t.Fatal("transport did not begin body Close")
			}
			select {
			case err := <-result:
				t.Fatalf("returned before Close completed: %v", err)
			default:
			}
			if test.cancel {
				cancel()
			}
			select {
			case err := <-result:
				fetchErr, ok := errors.AsType[*provider.FetchError](err)
				if !ok || fetchErr.Kind != provider.FetchTransport || !strings.Contains(err.Error(), "may have reached") {
					t.Fatalf("interrupted Close outcome=%v, want unknown-send transport error", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("body Close wait was unbounded")
			}
			select {
			case <-witness.body.done:
			default:
				t.Fatal("interrupted wait did not detach request body before wipe")
			}
			var buf [1]byte
			if n, err := witness.body.Read(buf[:]); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("late Read=%d, %v", n, err)
			}
			once.Do(func() { close(release) })
		})
	}
}
