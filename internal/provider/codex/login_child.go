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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/runtime/cleanup"
	"github.com/zchee/agentctl/internal/runtime/coordinator"
	"github.com/zchee/agentctl/internal/runtime/signals"
	"github.com/zchee/agentctl/internal/secret"
)

// LoginDeadline bounds the browser login and the vendor child.
const LoginDeadline = 600 * time.Second

// AllowedLoginEnv constructs the vendor child's environment, replacing CODEX_HOME.
func AllowedLoginEnv(inherited []string, scratch string) []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "LANG", "TERM", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"}
	chosen := make([]string, 0, len(allowed)+1)
	for _, entry := range inherited {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "CODEX_HOME" {
			continue
		}
		if slices.Contains(allowed, name) || strings.HasPrefix(name, "LC_") && len(name) > 3 || loginTestEnv(name) {
			chosen = append(chosen, loginFixtureEnv(entry))
		}
	}
	return append(chosen, "CODEX_HOME="+scratch)
}

// IsScratchName identifies only names generated for owned login homes.
func IsScratchName(name string) bool {
	suffix, ok := strings.CutPrefix(name, ScratchPrefix)
	if !ok || len(suffix) != 8 {
		return false
	}
	for _, ch := range suffix {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// AcquireScratchLock serializes the complete login lifecycle without claiming a namespace.
func AcquireScratchLock(ctx context.Context, paths *config.Paths) (*secret.LockGuard, error) {
	guard, err := secret.Acquire(ctx, paths.CodexLocksDir(), filepath.Base(paths.CodexScratchLock()), time.Now().Add(5*time.Second))
	if err != nil {
		return nil, errs.NewRefused(0, "another `agentctl codex login` is in progress: "+err.Error())
	}
	return guard, nil
}

// OpenScratchRoot verifies the descriptor used for creation and stale-home removal.
func OpenScratchRoot(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	refuse := func(reason string) error {
		return errs.NewRefused(0, fmt.Sprintf("the Codex scratch root `%s` %s; agentctl will not create a login home there", path, reason))
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		meta, statErr := os.Lstat(path)
		if statErr == nil && meta.Mode()&os.ModeSymlink != 0 {
			return nil, refuse("is a symbolic link")
		}
		if statErr == nil && !meta.IsDir() {
			return nil, refuse("is not a directory")
		}
		return nil, refuse("cannot be opened (" + err.Error() + ")")
	}
	root := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = root.Close()
		return nil, refuse("cannot be inspected (" + err.Error() + ")")
	}
	if reason := scratchRootVerdict(stat.Uid, uint32(stat.Mode), uint32(os.Geteuid())); reason != "" {
		_ = root.Close()
		return nil, refuse(reason)
	}
	return root, nil
}

func scratchRootVerdict(uid, mode, expected uint32) string {
	if uid != expected {
		return "is owned by another user"
	}
	if mode&0o077 != 0 {
		return fmt.Sprintf("is mode %04o, which lets other users in (it must be 0700)", mode&0o777)
	}
	return ""
}

// LoginScratch retains both descriptors so cleanup cannot follow a substituted path.
type LoginScratch struct {
	root  *os.File
	leaf  *os.File
	path  string
	name  string
	mu    sync.Mutex
	done  bool
	token cleanup.Token
}

// NewLoginScratch creates an exclusive private home, taking ownership of root.
func NewLoginScratch(ctx context.Context, root *os.File) (*LoginScratch, error) {
	if err := ctx.Err(); err != nil {
		_ = root.Close()
		return nil, err
	}
	for range 8 {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			_ = root.Close()
			return nil, err
		}
		name := ScratchPrefix + hex.EncodeToString(random[:])
		err := unix.Mkdirat(int(root.Fd()), name, 0o700)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			_ = root.Close()
			return nil, errs.NewRefused(0, fmt.Sprintf("could not create a Codex login home under `%s`: %s", root.Name(), err))
		}
		fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = unix.Unlinkat(int(root.Fd()), name, unix.AT_REMOVEDIR)
			_ = root.Close()
			return nil, err
		}
		scratch := &LoginScratch{root: root, leaf: os.NewFile(uintptr(fd), name), path: filepath.Join(root.Name(), name), name: name}
		scratch.token = cleanup.Register(func() { scratch.UnlinkCredential() })
		return scratch, nil
	}
	_ = root.Close()
	return nil, errs.NewRefused(0, "8 generated Codex login home names were already taken")
}

// Path returns the only home the vendor child is permitted to receive.
func (s *LoginScratch) Path() string { return s.path }

// UnlinkCredential removes the scratch copy without resolving its path again.
func (s *LoginScratch) UnlinkCredential() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		_ = unix.Unlinkat(int(s.leaf.Fd()), "auth.json", 0)
	}
}

func scratchRemovalReport(out io.Writer, path, what string, err error) {
	if out != nil {
		_, _ = fmt.Fprintf(out, "agentctl: could not remove %s in the Codex login scratch home `%s` (%s); remove it by hand\n", what, path, err)
	}
}

func scratchOpenSubdir(fd int, name string) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	child, err := unix.Openat(fd, name, flags, 0)
	if errors.Is(err, unix.EACCES) {
		if chmodErr := unix.Fchmodat(fd, name, 0o700, unix.AT_SYMLINK_NOFOLLOW); chmodErr != nil {
			return -1, chmodErr
		}
		return unix.Openat(fd, name, flags, 0)
	}
	return child, err
}

func scratchEntries(fd int) ([]string, error) {
	copied, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(copied), "scratch")
	defer func() { _ = file.Close() }()
	return file.Readdirnames(-1)
}

func removeScratchTree(fd, depth int) {
	if depth >= 16 {
		return
	}
	names, err := scratchEntries(fd)
	if err != nil {
		return
	}
	for _, name := range names {
		var st unix.Stat_t
		if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			continue
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			child, err := scratchOpenSubdir(fd, name)
			if err == nil {
				removeScratchTree(child, depth+1)
				_ = unix.Close(child)
			}
			_ = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
		} else {
			_ = unix.Unlinkat(fd, name, 0)
		}
	}
}

// Discard removes all owned residue, reporting failures without exposing file contents.
func (s *LoginScratch) Discard(out io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	fd := int(s.leaf.Fd())
	_ = unix.Fchmod(fd, 0o700)
	credentialErr := unix.Unlinkat(fd, "auth.json", 0)
	removeScratchTree(fd, 0)
	var stat unix.Stat_t
	if credentialErr != nil && !errors.Is(credentialErr, unix.ENOENT) && unix.Fstatat(fd, "auth.json", &stat, unix.AT_SYMLINK_NOFOLLOW) == nil {
		scratchRemovalReport(out, s.path, "the credential `auth.json`", credentialErr)
	}
	if err := unix.Unlinkat(int(s.root.Fd()), s.name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		scratchRemovalReport(out, s.path, "the directory", err)
	}
	cleanup.Unregister(s.token)
	_ = s.leaf.Close()
	_ = s.root.Close()
}

// SweepLoginScratch removes only old, exactly named homes through the verified root.
func SweepLoginScratch(ctx context.Context, root *os.File, maxAge time.Duration, out io.Writer) {
	names, err := scratchEntries(int(root.Fd()))
	if err != nil {
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		if !IsScratchName(name) {
			continue
		}
		var stat unix.Stat_t
		if unix.Fstatat(int(root.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		if stat.Mtim.Sec < 0 || time.Since(time.Unix(stat.Mtim.Sec, 0)) < maxAge {
			continue
		}
		fd, err := scratchOpenSubdir(int(root.Fd()), name)
		if err == nil {
			_ = unix.Fchmod(fd, 0o700)
			removeScratchTree(fd, 0)
			_ = unix.Close(fd)
		}
		if err := unix.Unlinkat(int(root.Fd()), name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			scratchRemovalReport(out, filepath.Join(root.Name(), name), "a stale login home", err)
		}
	}
}

// LoginSurvey records residue; incomplete observations cannot prove a clean home.
type LoginSurvey struct {
	DaemonDir bool
	HeldLocks []string
	OddLocks  []string
	Truncated bool
}

// SurveyLoginScratch inspects bounded vendor residue without following links.
func SurveyLoginScratch(ctx context.Context, path string) LoginSurvey {
	found := LoginSurvey{HeldLocks: []string{}, OddLocks: []string{}}
	if meta, err := os.Lstat(filepath.Join(path, "app-server-daemon")); err == nil {
		found.DaemonDir = meta.IsDir()
	}
	type queued struct {
		path  string
		depth int
	}
	queue := []queued{{path, 0}}
	budget := 4096
	for len(queue) > 0 {
		item := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if ctx.Err() != nil || item.depth >= 8 {
			found.Truncated = true
			continue
		}
		entries, err := os.ReadDir(item.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			found.Truncated = true
			continue
		}
		for _, entry := range entries {
			if budget == 0 {
				found.Truncated = true
				return found
			}
			budget--
			p := filepath.Join(item.path, entry.Name())
			meta, err := os.Lstat(p)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				found.Truncated = true
				continue
			}
			if meta.Mode()&os.ModeSymlink != 0 {
				continue
			}
			isLock := strings.HasSuffix(entry.Name(), ".lock")
			if meta.IsDir() && !isLock {
				queue = append(queue, queued{p, item.depth + 1})
				continue
			}
			if !isLock {
				continue
			}
			if !meta.Mode().IsRegular() {
				found.OddLocks = append(found.OddLocks, p)
				continue
			}
			fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
				continue
			}
			if err != nil {
				found.Truncated = true
				continue
			}
			err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
			switch {
			case err == nil:
				_ = unix.Flock(fd, unix.LOCK_UN)
			case errors.Is(err, unix.EWOULDBLOCK):
				found.HeldLocks = append(found.HeldLocks, p)
			case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
			default:
				found.Truncated = true
			}
			_ = unix.Close(fd)
		}
	}
	return found
}

// ResolveLoginBinary selects an executable vendor CLI, with test injection compiled separately.
func ResolveLoginBinary() (string, error) {
	if bin, ok := loginTestBinary(); ok {
		return bin, nil
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "codex")
		meta, err := os.Stat(candidate)
		if err == nil && meta.Mode().IsRegular() && meta.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errs.NewRefused(0, "`codex` is not on PATH; install the Codex CLI, or run `agentctl codex import`")
}

// LoginChild provides streams and cooperative signal ownership for an interactive child.
type LoginChild struct {
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Signals *signals.Controller
}

// Run starts the vendor login inside its owned home and reaps it before returning.
func (c LoginChild) Run(ctx context.Context, scratch *LoginScratch, bin string) (*os.ProcessState, error) {
	pass := coordinator.Standalone(ctx, c.Signals, time.Now().Add(LoginDeadline))
	release, ok := pass.BeginSpawn()
	if !ok {
		return nil, errs.NewRefused(0, "the Codex login was cancelled; nothing was installed")
	}
	cmd := exec.Command(bin, "-c", "cli_auth_credentials_store=\"file\"", "login")
	cmd.Env = AllowedLoginEnv(os.Environ(), scratch.Path())
	cmd.Dir = scratch.Path()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.In, c.Out, c.Err
	if err := cmd.Start(); err != nil {
		release()
		return nil, errs.NewRefused(0, fmt.Sprintf("could not run `%s`: %s", bin, err))
	}
	token := pass.RegisterChild(cmd)
	release()
	state, err := pass.WaitChildTimeout(token, LoginDeadline)
	if err != nil || state == nil {
		if ctx.Err() != nil || errors.Is(err, coordinator.ErrPassCancelled) || errors.Is(err, coordinator.ErrChildKilled) {
			return nil, errs.NewRefused(0, "the Codex login was cancelled; nothing was installed")
		}
		return nil, errs.NewRefused(0, "the Codex login did not finish within 600s; nothing was installed")
	}
	return state, nil
}

// RunReport observes an exited vendor child and takes a second fresh keychain listing.
// An unlistable keychain cannot prove that the child left no credential behind.
func (c LoginChild) RunReport(ctx context.Context, scratch *LoginScratch, bin string, before []string, listingAfter func() ([]string, error)) (*PostExitReport, error) {
	state, err := c.Run(ctx, scratch, bin)
	if err != nil {
		return nil, err
	}
	found := SurveyLoginScratch(ctx, scratch.Path())
	after, err := listingAfter()
	if err != nil {
		return nil, errs.NewRefused(0, "could not read the keychain after the login: "+err.Error())
	}
	gained := []string{}
	for _, account := range after {
		if !slices.Contains(before, account) && !slices.Contains(gained, account) {
			gained = append(gained, account)
		}
	}
	survey := ScratchSurvey{daemonDir: found.DaemonDir, heldLocks: found.HeldLocks, oddLocks: found.OddLocks, truncated: found.Truncated}
	return postExitReportFromChild(gained, nil, survey, state), nil
}
