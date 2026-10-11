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

// Package desktop owns the on-disk side of switching the Claude desktop
// app between accounts: where whole profile directories are kept, how they
// are named, which account a profile last observed, the append-only
// journal of every exchange, and the two filesystem operations allowed on a
// profile — an exclusive rename of the whole directory and the deletion of
// one stored, inactive profile.
//
// The app signs in with a web session it keeps inside its data directory,
// so the only lever that touches no credential is which directory sits at
// the app's fixed data path when it starts. Nothing in this package opens a
// file inside a profile other than config.json, and that one only through a
// bounded read that keeps a single plaintext member.
//
// The desktop store lives beside the app's data directory, not inside the
// agentctl store: one data path per user needs one lock and one journal
// that every agentctl store sees, and a rename only works on one
// filesystem.
package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BundleID is the desktop app's bundle identifier.
const BundleID = "com.anthropic.claudefordesktop"

// LockName is the file name of the per-user desktop lock inside Root.
const LockName = "lock"

// Layout is every path the desktop feature uses for one user.
type Layout struct {
	// Home is the user's home directory the layout was derived from.
	Home string
	// Root is the per-user desktop store, beside the app's data directory.
	Root string
	// ProfilesDir holds one stored profile directory per entry.
	ProfilesDir string
	// Journal is the append-only record of every exchange.
	Journal string
	// Lock is the per-user lock file that serializes every desktop
	// mutation, whichever agentctl store the caller uses.
	Lock string
	// DefaultData is the app's fixed data directory: whichever profile
	// sits here when the app starts is the one it uses.
	DefaultData string
}

// Paths derives the layout from the user's home directory.
func Paths(home string) Layout {
	support := filepath.Join(home, "Library", "Application Support")
	root := filepath.Join(support, "agentctl", "claude-desktop")
	return Layout{
		Home:        home,
		Root:        root,
		ProfilesDir: filepath.Join(root, "profiles"),
		Journal:     filepath.Join(root, "journal.jsonl"),
		Lock:        filepath.Join(root, LockName),
		DefaultData: filepath.Join(support, "Claude"),
	}
}

// Profile returns the path of a stored profile. The name must have passed
// [ParseProfileName]; the caller never joins an unchecked name.
func (l Layout) Profile(name string) string {
	return filepath.Join(l.ProfilesDir, name)
}

// EnsureStore creates Root and ProfilesDir at 0700 when missing, after
// proving that no component of either path is a symbolic link: a link in
// the chain would let a rename land somewhere the layout does not name.
func (l Layout) EnsureStore() error {
	for _, dir := range []string{l.Root, l.ProfilesDir} {
		if err := ParentChainHasNoSymlink(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("checking %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
	}
	return ParentChainHasNoSymlink(l.ProfilesDir)
}

// SymlinkComponentError reports a path component that is a symbolic link.
type SymlinkComponentError struct {
	// Path is the full path checked.
	Path string
	// Component is the prefix of Path that is a link.
	Component string
}

// Error names the offending component.
func (e *SymlinkComponentError) Error() string {
	return fmt.Sprintf("%s: the component %s is a symbolic link", e.Path, e.Component)
}

// ParentChainHasNoSymlink checks every existing component of the absolute
// path, the path itself included, with Lstat and fails when one is a
// symbolic link. Components that do not exist end the walk without error,
// because a missing directory cannot redirect anything; the caller still
// meets the absence when it acts.
func ParentChainHasNoSymlink(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	clean := filepath.Clean(path)
	current := string(filepath.Separator)
	for part := range strings.SplitSeq(strings.TrimPrefix(clean, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking %s: %w", current, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return &SymlinkComponentError{Path: clean, Component: current}
		}
	}
	return nil
}

// NameKind is the shape of a stored profile's directory name.
type NameKind string

const (
	// NameAccount is a profile that last observed one account: its name is
	// that account's UUID.
	NameAccount NameKind = "account"
	// NameFresh is a profile with no config.json that left the data path
	// during the named operation.
	NameFresh NameKind = "fresh"
	// NameFreshTarget is a profile the app created during the named
	// operation and that a rollback had to park; it can never collide
	// with the outgoing fresh profile of the same operation.
	NameFreshTarget NameKind = "fresh-target"
	// NameUnknown is a profile whose config.json names no account.
	NameUnknown NameKind = "unknown"
	// NameStray is a directory found at the data path that matched
	// neither side of an open operation, moved aside by repair.
	NameStray NameKind = "stray"
)

// ProfileName is a parsed stored-profile name.
type ProfileName struct {
	// Kind is the shape of the name.
	Kind NameKind
	// UUID is the account UUID for NameAccount, lower case.
	UUID string
	// OpID is the operation id for every other kind.
	OpID string
}

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	opIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ValidUUID reports whether s is a UUID in its canonical hyphenated text
// form, in either case.
func ValidUUID(s string) bool { return uuidPattern.MatchString(s) }

// ValidOpID reports whether s is an operation id: sixteen lower-case
// hexadecimal digits.
func ValidOpID(s string) bool { return opIDPattern.MatchString(s) }

// NewOpID returns a random operation id.
func NewOpID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating an operation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// AccountName is the stored name of a profile that last observed uuid.
func AccountName(uuid string) string { return strings.ToLower(uuid) }

// FreshName is the stored name of an outgoing profile without config.json.
func FreshName(opID string) string { return "fresh-" + opID }

// FreshTargetName is where a rollback parks a profile the app created
// during the operation.
func FreshTargetName(opID string) string { return "fresh-" + opID + "-target" }

// UnknownName is the stored name of an outgoing profile that names no
// account.
func UnknownName(opID string) string { return "unknown-" + opID }

// StrayName is where repair moves an unexpected directory at the data
// path.
func StrayName(opID string) string { return "stray-" + opID }

// ErrBadProfileName reports a name outside the stored-profile grammar.
var ErrBadProfileName = errors.New("not a stored profile name")

// ParseProfileName accepts exactly the names this package creates: an
// account UUID in lower case, fresh-<op>, fresh-<op>-target, unknown-<op>
// and stray-<op>, where <op> is sixteen lower-case hexadecimal digits.
// Anything else, including a name with a separator, is refused, so a name
// read from a listing or a command line can be joined to ProfilesDir.
func ParseProfileName(name string) (ProfileName, error) {
	if uuidPattern.MatchString(name) {
		if name != strings.ToLower(name) {
			return ProfileName{}, fmt.Errorf("%q: %w (account names are lower case)", name, ErrBadProfileName)
		}
		return ProfileName{Kind: NameAccount, UUID: name}, nil
	}
	if rest, ok := strings.CutPrefix(name, "fresh-"); ok {
		if op, ok := strings.CutSuffix(rest, "-target"); ok && opIDPattern.MatchString(op) {
			return ProfileName{Kind: NameFreshTarget, OpID: op}, nil
		}
		if opIDPattern.MatchString(rest) {
			return ProfileName{Kind: NameFresh, OpID: rest}, nil
		}
	}
	if op, ok := strings.CutPrefix(name, "unknown-"); ok && opIDPattern.MatchString(op) {
		return ProfileName{Kind: NameUnknown, OpID: op}, nil
	}
	if op, ok := strings.CutPrefix(name, "stray-"); ok && opIDPattern.MatchString(op) {
		return ProfileName{Kind: NameStray, OpID: op}, nil
	}
	return ProfileName{}, fmt.Errorf("%q: %w", name, ErrBadProfileName)
}
