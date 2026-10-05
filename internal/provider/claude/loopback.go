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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

// LoopbackTimeout bounds waiting for the browser callback.
const LoopbackTimeout = 600 * time.Second

// LoopbackPollInterval bounds cancellation checks while accepting the callback.
const LoopbackPollInterval = 250 * time.Millisecond

// CallbackReadTimeout bounds an accepted connection that sends no request line.
const CallbackReadTimeout = 10 * time.Second

// ErrLoopbackTimeout means no authorization response arrived before the deadline.
var ErrLoopbackTimeout = errors.New("timed out waiting for the authorization response")

// ListenLoopback binds an arbitrary free IPv4 loopback port.
func ListenLoopback(ctx context.Context) (*net.TCPListener, error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("could not bind a loopback listener: %w", err)
	}
	return listener.(*net.TCPListener), nil
}

// LoopbackWait consumes and closes the listener after exactly one callback.
// The caller must close the listener if it abandons login before calling this.
func LoopbackWait(ctx context.Context, listener *net.TCPListener, expectedState string, deadline time.Time) (string, error) {
	defer func() { _ = listener.Close() }()
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return "", ErrLoopbackTimeout
		}
		pollDeadline := time.Now().Add(LoopbackPollInterval)
		if deadline.Before(pollDeadline) {
			pollDeadline = deadline
		}
		if err := listener.SetDeadline(pollDeadline); err != nil {
			return "", err
		}
		stream, err := listener.AcceptTCP()
		if err != nil {
			if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
				continue
			}
			return "", fmt.Errorf("the loopback listener failed: %w", err)
		}
		return readCallback(stream, expectedState)
	}
}

func readCallback(stream *net.TCPConn, expectedState string) (string, error) {
	defer func() { _ = stream.Close() }()
	if err := stream.SetReadDeadline(time.Now().Add(CallbackReadTimeout)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(stream).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("could not read the loopback request: %w", err)
	}
	code, outcome := parseCallback(line, expectedState)
	page := "<!doctype html><title>agentctl</title><p>Login complete. You can close this tab."
	status := "200 OK"
	if outcome != nil {
		page = "<!doctype html><title>agentctl</title><p>Login failed. Return to the terminal."
		status = "400 Bad Request"
	}
	// A browser disconnect after receipt does not invalidate the code in hand.
	_ = stream.SetWriteDeadline(time.Now().Add(CallbackReadTimeout))
	_, _ = fmt.Fprintf(stream, "HTTP/1.1 %s\r\ncontent-type: text/html; charset=utf-8\r\ncontent-length: %d\r\nconnection: close\r\n\r\n%s", status, len(page), page)
	return code, outcome
}

func parseCallback(line, expectedState string) (string, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return "", errors.New("the loopback request was not HTTP")
	}
	if !strings.EqualFold(parts[0], "GET") {
		return "", fmt.Errorf("the loopback request used `%s`, not GET", redactBody(parts[0]))
	}
	base := &url.URL{Scheme: "http", Host: "localhost", Path: "/"}
	target, err := base.Parse(parts[1])
	if err != nil {
		return "", errors.New("the loopback request target is not a URL")
	}
	// Duplicate parameters take their last value, including an empty value.
	values := make(map[string]string)
	for pair := range strings.SplitSeq(target.RawQuery, "&") {
		key, value, _ := strings.Cut(pair, "=")
		key, _ = url.QueryUnescape(key)
		value, _ = url.QueryUnescape(value)
		values[key] = value
	}
	if refusal, ok := values["error"]; ok {
		return "", fmt.Errorf("the authorization request was refused: %s", redactBody(refusal))
	}
	code, hasCode := values["code"]
	state, hasState := values["state"]
	if !hasCode || !hasState {
		return "", errors.New("the loopback request carried no authorization code")
	}
	if err := VerifyState(expectedState, state); err != nil {
		return "", err
	}
	return code, nil
}
