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

// Package tty answers what a command may do with its own terminal:
// whether a stream is one, how big it is, and bounded waits for input on
// it — never another process's input.
package tty

import (
	"errors"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// IsTerminal reports whether f is attached to a terminal. Prompts and the
// watch display refuse to start without one, because raw mode and
// interactive confirmation both assume a terminal on the other end.
func IsTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// Size returns the terminal's width and height in character cells.
func Size(f *os.File) (width, height int, err error) {
	return term.GetSize(int(f.Fd()))
}

// Readiness waits for input on one terminal, remembering a device's
// unsupported poll result so subsequent waits use select instead.
type Readiness struct {
	useSelect bool
}

// WaitReadable waits no longer than timeout for input or hangup on f and
// reports whether any arrived. An interrupted wait returns false with no
// error, allowing the caller to observe cancellation on its next loop.
//
// The first wait probes with poll; a device that answers poll with an
// invalid-descriptor event is retried — and remembered — through select,
// which some terminal devices support when poll refuses them. A failing
// poll can itself consume time, so its fallback shares the caller's
// timeout rather than postponing the cancellation check.
func (r *Readiness) WaitReadable(f *os.File, timeout time.Duration) (bool, error) {
	started := time.Now()
	fd := int(f.Fd())
	defer runtime.KeepAlive(f)

	if !r.useSelect {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, millisecondsFor(timeout))
		switch {
		case errors.Is(err, unix.EINTR):
			return false, nil
		case err != nil:
			return false, os.NewSyscallError("poll", err)
		case n == 0:
			return false, nil
		case fds[0].Revents&unix.POLLNVAL != 0:
			r.useSelect = true
		default:
			return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
		}
	}

	remaining := timeout - time.Since(started)
	if remaining <= 0 {
		return false, nil
	}
	// select addresses descriptors by a fixed-size bit set, so one past
	// its capacity cannot be waited on at all.
	if fd < 0 || fd >= unix.FD_SETSIZE {
		return false, os.NewSyscallError("select", unix.EINVAL)
	}
	var readfds unix.FdSet
	readfds.Set(fd)
	tv := unix.NsecToTimeval(remaining.Nanoseconds())
	n, err := unix.Select(fd+1, &readfds, nil, nil, &tv)
	switch {
	case errors.Is(err, unix.EINTR):
		return false, nil
	case err != nil:
		return false, os.NewSyscallError("select", err)
	default:
		return n > 0, nil
	}
}

// millisecondsFor converts a wait bound to poll's millisecond argument,
// rounding up so a sub-millisecond bound still waits rather than spinning.
func millisecondsFor(timeout time.Duration) int {
	if timeout <= 0 {
		return 0
	}
	ms := (timeout + time.Millisecond - 1) / time.Millisecond
	return int(min(ms, 1<<31-1))
}
