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

package commands

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/proc"
	"github.com/zchee/agentctl/internal/secret"
)

// useSessionHints is what the registry scan found: the bridged sessions
// still to be named in the manual advisory, the ones the Remote Control
// follow-up will handle, and why the registry could not be read.
type useSessionHints struct {
	// names are the human names of the bridged sessions the advisory
	// names, in scan order.
	names []string
	// sessions are every bridged, live session the scan found.
	sessions []useBridgedSession
	// handled counts the sessions the Remote Control follow-up will ask
	// to start it again; they are not in names.
	handled    int
	unreadable string
}

// useBridgedSession is one registry record that named a bridge and a live
// process.
type useBridgedSession struct {
	path    string
	name    string
	pid     uint32
	record  claude.RegistryRecord
	decoded bool
}

// useRegistryRecordLimit bounds one registry record read.
const useRegistryRecordLimit = 64*1024 - 1

func useScanSessions(ctx context.Context, dir string) useSessionHints {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return useSessionHints{}
		}
		return useSessionHints{unreadable: useSessionErrorKind(err)}
	}
	var result useSessionHints
	considered := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if considered == 512 {
			break
		}
		considered++
		path := filepath.Join(dir, entry.Name())
		read, err := secret.ReadFile(path, useRegistryRecordLimit)
		if err != nil || !read.Present {
			continue
		}
		var document struct {
			PID    *uint32 `json:"pid"`
			Name   *string `json:"name"`
			Bridge *string `json:"bridgeSessionId"`
		}
		if json.Unmarshal(read.Bytes, &document) != nil || document.Bridge == nil || *document.Bridge == "" {
			continue
		}
		pid := uint64(0)
		if document.PID != nil {
			pid = uint64(*document.PID)
		} else {
			pid, _ = strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 32)
		}
		if pid == 0 {
			continue
		}
		if _, err := proc.Lookup(ctx, int(pid)); errors.Is(err, proc.ErrProcessGone) {
			continue
		}
		name := ""
		if document.Name != nil {
			name = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				if r == '`' {
					return '\''
				}
				return r
			}, strings.TrimSpace(*document.Name))
			name = strings.TrimSpace(name)
			chars := []rune(name)
			if len(chars) > 48 {
				name = string(chars[:47]) + "…"
			}
		}
		record, err := claude.DecodeRegistryRecord(read.Bytes)
		result.names = append(result.names, name)
		result.sessions = append(result.sessions, useBridgedSession{path: path, name: name, pid: uint32(pid), record: record, decoded: err == nil})
	}
	return result
}

// useSessionErrorKind reports only the failure class, never a path or error payload.
func useSessionErrorKind(err error) string {
	errno, ok := errors.AsType[syscall.Errno](err)
	if !ok {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "entity not found"
		case errors.Is(err, fs.ErrPermission):
			return "permission denied"
		case errors.Is(err, fs.ErrExist):
			return "entity already exists"
		case errors.Is(err, fs.ErrInvalid):
			return "invalid input parameter"
		case errors.Is(err, errors.ErrUnsupported):
			return "unsupported"
		}
		return "other error"
	}
	switch errno {
	case syscall.EACCES, syscall.EPERM:
		return "permission denied"
	case syscall.E2BIG:
		return "argument list too long"
	case syscall.EADDRINUSE:
		return "address in use"
	case syscall.EADDRNOTAVAIL:
		return "address not available"
	case syscall.EBUSY:
		return "resource busy"
	case syscall.ECONNABORTED:
		return "connection aborted"
	case syscall.ECONNREFUSED:
		return "connection refused"
	case syscall.ECONNRESET:
		return "connection reset"
	case syscall.EDEADLK:
		return "deadlock"
	case syscall.EDQUOT:
		return "quota exceeded"
	case syscall.EEXIST:
		return "entity already exists"
	case syscall.EFBIG:
		return "file too large"
	case syscall.EHOSTUNREACH:
		return "host unreachable"
	case syscall.EINTR:
		return "operation interrupted"
	case syscall.EINVAL:
		return "invalid input parameter"
	case syscall.EISDIR:
		return "is a directory"
	case syscall.ELOOP:
		return "filesystem loop or indirection limit (e.g. symlink loop)"
	case syscall.ENOENT:
		return "entity not found"
	case syscall.ENOMEM:
		return "out of memory"
	case syscall.ENOSPC:
		return "no storage space"
	case syscall.ENOSYS, syscall.EOPNOTSUPP:
		return "unsupported"
	case syscall.EMLINK:
		return "too many links"
	case syscall.ENAMETOOLONG:
		return "invalid filename"
	case syscall.ENETDOWN:
		return "network down"
	case syscall.ENETUNREACH:
		return "network unreachable"
	case syscall.ENOTCONN:
		return "not connected"
	case syscall.ENOTDIR:
		return "not a directory"
	case syscall.ENOTEMPTY:
		return "directory not empty"
	case syscall.EPIPE:
		return "broken pipe"
	case syscall.EROFS:
		return "read-only filesystem or storage medium"
	case syscall.ESPIPE:
		return "seek on unseekable file"
	case syscall.ESTALE:
		return "stale network file handle"
	case syscall.ETIMEDOUT:
		return "timed out"
	case syscall.ETXTBSY:
		return "executable file busy"
	case syscall.EXDEV:
		return "cross-device link or rename"
	case syscall.EINPROGRESS:
		return "in progress"
	case syscall.EMFILE, syscall.ENFILE:
		return "too many open files"
	}
	if errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK {
		return "operation would block"
	}
	return "uncategorized error"
}

func (p SessionProcess) tellUseSessions(ctx context.Context, hints useSessionHints, report *useReport) {
	if message := hints.completion(true); message != "" {
		if err := tell(p.Err, "warning: "+message); err != nil {
			slog.ErrorContext(ctx, "the Remote Control advisory could not be written", slog.Any("error", err))
		}
		report.warnings = append(report.warnings, hints.completion(false))
	}
}

func (hints useSessionHints) list() string {
	var named []string
	unnamed := 0
	for _, name := range hints.names {
		if name == "" {
			unnamed++
		} else {
			named = append(named, name)
		}
	}
	slices.Sort(named)
	var parts []string
	for _, name := range named[:min(3, len(named))] {
		parts = append(parts, "`"+name+"`")
	}
	if unnamed != 0 {
		parts = append(parts, fmt.Sprintf("%d unnamed", unnamed))
	}
	if len(named) > 3 {
		parts = append(parts, fmt.Sprintf("%d more", len(named)-3))
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func (hints useSessionHints) consent() string {
	text := ""
	if hints.handled != 0 {
		plural, have := "", "has"
		if hints.handled != 1 {
			plural, have = "s", "have"
		}
		text = fmt.Sprintf(". After the swap, agentctl will ask the %d running Claude Code session%s that %s Remote Control on and read this store to start it again", hints.handled, plural, have)
	}
	if len(hints.names) == 0 {
		return text
	}
	plural, have := "", "has"
	if len(hints.names) != 1 {
		plural, have = "s", "have"
	}
	return text + fmt.Sprintf(". %d running Claude Code session%s (%s) %s Remote Control on, and agentctl cannot tell which of them use this store. In each one that does, Claude Code stops Remote Control after the swap (now, or on its next account check), and `/remote-control` there starts it again", len(hints.names), plural, hints.list(), have)
}

func (hints useSessionHints) completion(names bool) string {
	if len(hints.names) == 0 {
		return ""
	}
	plural, list := "", ""
	if len(hints.names) != 1 {
		plural = "s"
	}
	if names {
		list = " (" + hints.list() + ")"
	}
	return fmt.Sprintf("%d Claude Code session%s%s had Remote Control on when this swap started, and agentctl cannot tell which of them use this store. In each one that does, Remote Control stops (now, or on its next account check): run `/remote-control` there to start it again", len(hints.names), plural, list)
}
