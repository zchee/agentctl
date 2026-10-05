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
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

// HomeEnv is the caller's snapshot of the two home-resolution inputs.
type HomeEnv struct {
	CodexHome string
	Home      string
}

// Env is the home-resolution environment.
type Env = HomeEnv

// Home is a read-only Codex home, never an owned namespace write target.
type Home struct{ Dir string }

// HomeError reports an unusable explicit home without reading credentials.
type HomeError struct {
	Path   string
	Kind   string
	Reason string
}

// Error describes the home resolution failure.
func (e *HomeError) Error() string {
	switch e.Kind {
	case "missing":
		return fmt.Sprintf("codex home unreadable: CODEX_HOME points to `%s`, but that path does not exist", e.Path)
	case "not_directory":
		return fmt.Sprintf("codex home unreadable: CODEX_HOME points to `%s`, but that path is not a directory", e.Path)
	case "no_home":
		return "codex home unreadable: CODEX_HOME is unset and the home directory is unknown"
	default:
		return fmt.Sprintf("codex home unreadable: could not read CODEX_HOME `%s`: %s", e.Path, e.Reason)
	}
}

// ResolveHome canonicalizes a nonempty explicit home and otherwise preserves the default spelling.
func ResolveHome(env Env) (Home, error) {
	if env.CodexHome == "" {
		if env.Home == "" {
			return Home{}, &HomeError{Kind: "no_home"}
		}
		return Home{Dir: filepath.Join(env.Home, ".codex")}, nil
	}
	info, err := os.Stat(env.CodexHome)
	if err != nil {
		kind := "unreadable"
		if errors.Is(err, os.ErrNotExist) {
			kind = "missing"
		}
		return Home{}, &HomeError{Path: env.CodexHome, Kind: kind, Reason: ioReason(err)}
	}
	if !info.IsDir() {
		return Home{}, &HomeError{Path: env.CodexHome, Kind: "not_directory"}
	}
	path, err := filepath.EvalSymlinks(env.CodexHome)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		return Home{}, &HomeError{Path: env.CodexHome, Kind: "unreadable", Reason: ioReason(err)}
	}
	return Home{Dir: path}, nil
}

// KeyringService is the service used only for read-only keychain listing matches.
const KeyringService = "Codex Auth"

// KeyringAccount returns the canonical home's keychain account name.
func KeyringAccount(home string) string {
	if real, err := filepath.EvalSymlinks(home); err == nil {
		if absolute, err := filepath.Abs(real); err == nil {
			home = absolute
		}
	}
	sum := sha256.Sum256([]byte(home))
	return "cli|" + hex.EncodeToString(sum[:8])
}

// IsHomeAccount accepts exactly the account names this tool can safely display in a shell command.
func IsHomeAccount(account string) bool {
	digest, ok := strings.CutPrefix(account, "cli|")
	if !ok || len(digest) != 16 {
		return false
	}
	for _, b := range []byte(digest) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// ReadonlyNoFollow contains no creation or mutation flags, and cannot block on a FIFO.
const ReadonlyNoFollow = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK

// OpenReadonlyNoFollow opens a leaf read-only while permitting symlinked parent directories.
func OpenReadonlyNoFollow(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, ReadonlyNoFollow, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// DaemonKind identifies evidence of a nearby Codex daemon.
type DaemonKind string

const (
	// DaemonNone means no daemon directory exists.
	DaemonNone DaemonKind = "none"
	// DaemonAlive means a live process is consistent with the pid record.
	DaemonAlive DaemonKind = "pid_alive"
	// DaemonRecycled means the pid belongs to a later process.
	DaemonRecycled DaemonKind = "recycled"
	// DaemonArtefact means only inactive filesystem evidence remains.
	DaemonArtefact DaemonKind = "artefact_only"
	// DaemonUnreadable means a record may be in the process of publication.
	DaemonUnreadable DaemonKind = "record_unreadable"
)

// Daemon carries non-secret evidence without touching the daemon's locks.
type Daemon struct {
	Kind DaemonKind
	PID  uint32
}

// DaemonRecycleTolerance is the permitted process-start lag behind the pid record.
const (
	DaemonRecycleTolerance = time.Second
	maxPIDRecordBytes      = 64 * 1024
)

// DaemonEvidence reads both pid record spellings and keeps the strongest evidence.
func DaemonEvidence(ctx context.Context, home string) Daemon {
	dir := filepath.Join(home, "app-server-daemon")
	info, err := os.Lstat(dir)
	if err != nil {
		return Daemon{Kind: DaemonNone}
	}
	if !info.IsDir() {
		return Daemon{Kind: DaemonArtefact}
	}
	best := Daemon{Kind: DaemonArtefact}
	var latest time.Time
	for _, name := range []string{"app-server.pid", "daemon.pid"} {
		found, written := oneDaemonRecord(ctx, filepath.Join(dir, name))
		if daemonRank(found.Kind) > daemonRank(best.Kind) || daemonRank(found.Kind) == daemonRank(best.Kind) && written.After(latest) {
			best, latest = found, written
		}
	}
	return best
}

func daemonRank(kind DaemonKind) int {
	switch kind {
	case DaemonAlive:
		return 4
	case DaemonUnreadable:
		return 3
	case DaemonRecycled:
		return 2
	case DaemonArtefact:
		return 1
	default:
		return 0
	}
}

func oneDaemonRecord(ctx context.Context, path string) (Daemon, time.Time) {
	unreadable := Daemon{Kind: DaemonUnreadable}
	file, err := OpenReadonlyNoFollow(ctx, path)
	if err != nil {
		if _, staterr := os.Lstat(path); errors.Is(staterr, os.ErrNotExist) {
			return Daemon{Kind: DaemonArtefact}, time.Time{}
		}
		return unreadable, time.Time{}
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPIDRecordBytes {
		return unreadable, time.Time{}
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxPIDRecordBytes))
	if err != nil {
		return unreadable, time.Time{}
	}
	var record struct {
		PID *uint32 `json:"pid"`
	}
	if json.Unmarshal(raw, &record) != nil || record.PID == nil {
		return unreadable, time.Time{}
	}
	written := info.ModTime()
	pid := *record.PID
	if pid == 0 {
		return Daemon{Kind: DaemonArtefact}, written
	}
	process, err := proc.Lookup(ctx, int(pid))
	if errors.Is(err, proc.ErrProcessGone) || err == nil && process.Holder == proc.HolderDead {
		return Daemon{Kind: DaemonArtefact}, written
	}
	if err == nil && !process.Start.IsZero() && process.Start.After(written.Add(DaemonRecycleTolerance)) {
		return Daemon{Kind: DaemonRecycled, PID: pid}, written
	}
	return Daemon{Kind: DaemonAlive, PID: pid}, written
}

func ioReason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "entity not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, unix.ELOOP):
		return "filesystem loop or indirection limit"
	default:
		return "other error"
	}
}
