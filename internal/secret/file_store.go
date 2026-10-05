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

package secret

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

// MaxCredentialsBytes is the largest credential file that will be read.
const MaxCredentialsBytes int64 = 1 << 20

// MaxMetaBytes is the largest pending-metadata file that will be read.
const MaxMetaBytes int64 = 4096

// CredentialsFile is the credential file's name, chosen to match the shape
// a Claude Code session reads so the directory can serve one.
const CredentialsFile = ".credentials.json"

// PendingFile is where credentials wait when a rename failed.
const PendingFile = ".credentials.json.pending"

// PendingMetaFile records what the pending credentials were derived from.
const PendingMetaFile = ".pending.meta"

// AdoptedFile is where a hot-swap parks the credential it displaced.
//
// Deliberately not [CredentialsFile]: the vendor's composed store falls
// through to that name on a read failure or a throttle — not only on an
// absent item — so a displaced credential parked there would be served to
// the peer session whenever its keychain read hiccuped, silently undoing
// the swap the user asked for. The session reads exactly one file name, so
// any other name in the same directory is invisible to it. It is
// adopt-only: nothing composes it, [ReadCredentials] does not look at it,
// and it is never replayed into [CredentialsFile] the way [PendingFile] is.
const AdoptedFile = ".credentials.adopted.json"

// Snapshot lstats a path.
//
// It returns nil when nothing is there, and an error when the path is a
// symbolic link: every caller is about to decide whether to trust or
// replace the file, and a link is a decision to refuse, not to follow.
func Snapshot(path string) (*FileSnapshot, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case info.Mode()&fs.ModeSymlink != 0:
		return nil, &SymlinkRefusedError{Path: path}
	}
	snap := snapshotOf(info)
	return &snap, nil
}

// SnapshotFollowing is [Snapshot] for a file this store only ever reads and
// never writes, such as the vendor's own live configuration: symbolic
// links are followed, because that file is commonly one, and refusing it
// would blind the live row rather than protect anything.
func SnapshotFollowing(path string) (*FileSnapshot, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	snap := snapshotOf(info)
	return &snap, nil
}

// ReadFile opens a file with O_NOFOLLOW and reads it, applying the size
// limit. The limit is the caller's, because the callers differ by orders
// of magnitude: a credential blob, its metadata, and a configuration file
// a session grows without bound.
func ReadFile(path string, limit int64) (ReadOutcome, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return classifyOpen(path, err)
	}
	return readOpened(fd, path, limit)
}

// ReadFileFollowing is [ReadFile] for a file this store only ever reads:
// the same regular-file and size rules, but a symbolic link is followed
// rather than refused (see [SnapshotFollowing]).
func ReadFileFollowing(path string, limit int64) (ReadOutcome, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return classifyOpen(path, err)
	}
	return readOpened(fd, path, limit)
}

// ReadCredentials reads `<nsDir>/.credentials.json`. An unreadable or
// missing path is absent; a symlink, a non-regular file or an oversized
// file is a failure, never an absence.
func ReadCredentials(nsDir string) (ReadOutcome, error) {
	return ReadFile(filepath.Join(nsDir, CredentialsFile), MaxCredentialsBytes)
}

// ReadAdopted reads `<nsDir>/.credentials.adopted.json` — the credential a
// swap displaced — under the same rules as [ReadCredentials]. Separate
// rather than a parameter, because the two answer different questions:
// that one asks what this namespace's store is, this one asks what a swap
// parked here.
func ReadAdopted(nsDir string) (ReadOutcome, error) {
	return ReadFile(filepath.Join(nsDir, AdoptedFile), MaxCredentialsBytes)
}

// WriteRequest is everything one credential write needs.
type WriteRequest struct {
	// Paths is this store, for the namespace-root check.
	Paths *config.Paths
	// NSDir is the namespace directory to write into.
	NSDir string
	// BlobJSON is the serialized credential document. The caller builds it
	// through the secret's explicit exposure path and wipes it afterwards.
	BlobJSON []byte
	// Prior holds the digests of the file these credentials were derived
	// from, nil on a first write. Recorded in the pending metadata.
	Prior *Digests
	// NewExpiresAtMS is the new access-token expiry, recorded in the
	// pending metadata.
	NewExpiresAtMS int64
	// faults is the test-only injection seam; nil in production.
	faults *writeFaults
}

// WriteCredentials replaces a namespace's credentials atomically.
//
// The target is checked lexically against the namespace root before
// anything happens, the namespace directory is reached by the no-follow
// walk, and the replacement follows [SecretFile.Write] under
// [StopDiscardStaged]: a cancelled context abandons the staged file, and a
// failed rename parks the bytes as pending rather than failing.
func WriteCredentials(ctx context.Context, req *WriteRequest) (WriteOutcome, error) {
	target := filepath.Join(req.NSDir, CredentialsFile)
	if !req.Paths.IsUnderNamespaceRoot(target) {
		return WriteOutcome{}, &OutsideRootError{Path: target}
	}

	dir, err := OpenNamespaceDir(req.Paths, req.NSDir)
	if err != nil {
		return WriteOutcome{}, err
	}
	defer func() { _ = unix.Close(dir) }()

	expires := req.NewExpiresAtMS
	spec := &PendingSpec{
		TargetName:  CredentialsFile,
		PendingName: PendingFile,
		MetaName:    PendingMetaFile,
		Prior:       req.Prior,
		ExpiresAtMS: &expires,
	}
	file := NewSecretFile(req.Paths.NamespaceRoot(), dir, CredentialsFile, target)
	file.faults = req.faults
	return file.Write(ctx, req.BlobJSON, spec, StopDiscardStaged)
}

// RemoveCredentialsFile removes the namespace's plaintext
// [CredentialsFile], if it is there, returning whether one was removed.
//
// The one caller is a swap whose store had not migrated: once the keychain
// item demonstrably holds the incoming credential this name is a second
// copy of the displaced one rather than its home, and the vendor's
// composed read would hand it to the peer session on nothing worse than a
// keychain hiccup. The directory is reached through the no-follow walk and
// the name unlinked relative to that descriptor.
func RemoveCredentialsFile(paths *config.Paths, nsDir string) (bool, error) {
	return removeStoreFile(paths, nsDir, CredentialsFile)
}

// RemoveAdoptedFile removes the redundant adopted copy after a confirmed
// live undo, returning whether the file existed. The caller must hold the
// namespace lock and verify that its own store still holds the same
// credential.
func RemoveAdoptedFile(paths *config.Paths, nsDir string) (bool, error) {
	return removeStoreFile(paths, nsDir, AdoptedFile)
}

func removeStoreFile(paths *config.Paths, nsDir, name string) (bool, error) {
	target := filepath.Join(nsDir, name)
	if !paths.IsUnderNamespaceRoot(target) {
		return false, &OutsideRootError{Path: target}
	}
	dir, err := OpenNamespaceDir(paths, nsDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(dir) }()
	err = unlinkAt(dir, name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return false, nil
	default:
		return false, errs.NewIO(fmt.Sprintf("could not remove `%s`", target), err)
	}
}

// RemoveNamespace removes a namespace's files and then its directories.
//
// The lock file is untouched: it lives outside the namespace, is never
// unlinked, and removing it would break flock's inode semantics for
// whoever is waiting on it. Deleting through a symbolic link is the same
// escape as writing through one, so a link anywhere along the chain is
// refused.
func RemoveNamespace(paths *config.Paths, nsDir string) error {
	if !paths.IsUnderNamespaceRoot(nsDir) {
		return &OutsideRootError{Path: nsDir}
	}

	root := paths.NamespaceRoot()
	chain, found, err := openChain(root, nsDir, walkMustExist)
	if err != nil {
		return err
	}
	if !found || len(chain) == 0 {
		closeChain(chain)
		return nil
	}
	defer closeChain(chain)
	leaf := chain[len(chain)-1].fd

	// The adopted copy is in this list for the reason the whole removal
	// exists: it holds a real credential at rest, and a removal that left
	// it behind would leave the user's token in a directory they had just
	// been told was gone.
	names := []string{CredentialsFile, AdoptedFile, PendingFile, PendingMetaFile}
	for _, list := range [][]string{mustList(nsDir, CredentialsFile), mustList(nsDir, AdoptedFile)} {
		for _, path := range list {
			names = append(names, filepath.Base(path))
		}
	}

	for _, name := range names {
		err := unlinkAt(leaf, name)
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return errs.NewIO(fmt.Sprintf("could not remove `%s`", filepath.Join(nsDir, name)), err)
		}
	}

	// Climb toward the namespace root, removing directories that are now
	// empty. AT_REMOVEDIR refuses a non-empty directory, which is the
	// check wanted here: a sibling organization's namespace must survive.
	// Index 0 is the root itself, which is never removed.
	for index := len(chain) - 1; index >= 1; index-- {
		if chain[index].name == "" {
			break
		}
		if unix.Unlinkat(chain[index-1].fd, chain[index].name, unix.AT_REMOVEDIR) != nil {
			break
		}
	}
	return nil
}

// mustList is [listStrayWithPrefix] for a caller that already holds the
// walked descriptor and treats a listing failure as fatal upstream; errors
// surface as an empty list because the removal's own unlink reports the
// real failure with the path attached.
func mustList(nsDir, target string) []string {
	found, err := listStrayWithPrefix(nsDir, target+".tmp.")
	if err != nil {
		return nil
	}
	return found
}

// ListStrayTmp lists leftover `.credentials.json.tmp.<8 hex>` files. A
// stray one is a crashed write and it holds token material at rest, so
// doctor reports them and login and removal clean them up.
func ListStrayTmp(nsDir string) ([]string, error) {
	return listStrayWithPrefix(nsDir, CredentialsFile+".tmp.")
}

// ListStrayAdoptedTmp lists leftover `.credentials.adopted.json.tmp.<8
// hex>` files: a crashed adoption, handled by the same callers that handle
// the credential writer's strays.
func ListStrayAdoptedTmp(nsDir string) ([]string, error) {
	return listStrayWithPrefix(nsDir, AdoptedFile+".tmp.")
}

// listStrayWithPrefix lists the `<prefix><8 hex>` files in one directory,
// sorted. The rule — an eight-digit hex suffix in either case and nothing
// else — decides which files count; every other name is ignored. A missing
// directory is an empty list.
func listStrayWithPrefix(nsDir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(nsDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	var found []string
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok || len(suffix) != 8 {
			continue
		}
		hex := true
		for _, b := range []byte(suffix) {
			if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
				hex = false
				break
			}
		}
		if hex {
			found = append(found, filepath.Join(nsDir, entry.Name()))
		}
	}
	slices.Sort(found)
	return found, nil
}

// walkMode says whether a walk may create the components it does not find.
type walkMode int

const (
	// walkCreate creates a missing component with mkdirat at 0700.
	walkCreate walkMode = iota + 1
	// walkMustExist ends the walk at a missing component.
	walkMustExist
)

// dirStep is one directory on the way from a root down to a namespace.
type dirStep struct {
	// fd is the open descriptor, never obtained by following a link.
	fd int
	// name is this directory's name within its parent; empty for the
	// root, which has no parent in the chain and is never removed.
	name string
}

// closeChain closes every descriptor a walk opened.
func closeChain(chain []dirStep) {
	for _, step := range chain {
		_ = unix.Close(step.fd)
	}
}

// openChain walks from root down to dir, one O_NOFOLLOW component at a
// time. It reports found=false when a component is missing and the walk
// may not create it. The returned chain always starts with root itself,
// and the caller owns every descriptor in it.
func openChain(root, dir string, mode walkMode) ([]dirStep, bool, error) {
	cleanRoot := filepath.Clean(root)
	cleanDir := filepath.Clean(dir)
	var relative string
	switch {
	case cleanDir == cleanRoot:
		relative = ""
	case strings.HasPrefix(cleanDir, cleanRoot+string(filepath.Separator)):
		relative = cleanDir[len(cleanRoot)+1:]
	default:
		return nil, false, &OutsideRootError{Path: dir}
	}

	rootFD, found, err := openDirAtPath(cleanRoot)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	chain := []dirStep{{fd: rootFD}}

	shown := cleanRoot
	for name := range strings.SplitSeq(relative, string(filepath.Separator)) {
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			closeChain(chain)
			return nil, false, &OutsideRootError{Path: dir}
		}
		shown = filepath.Join(shown, name)
		parent := chain[len(chain)-1].fd
		child, found, err := openDirAt(parent, name, shown)
		if err != nil {
			closeChain(chain)
			return nil, false, err
		}
		if !found {
			if mode != walkCreate {
				closeChain(chain)
				return nil, false, nil
			}
			child, err = createDirAt(parent, name, shown)
			if err != nil {
				closeChain(chain)
				return nil, false, err
			}
		}
		chain = append(chain, dirStep{fd: child, name: name})
	}
	return chain, true, nil
}

// openDirAtPath opens the walk's anchor itself without following a link at
// its final component.
func openDirAtPath(dir string) (int, bool, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		return fd, true, nil
	case isSymlinkErrno(err):
		return -1, false, &SymlinkRefusedError{Path: dir}
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ENOENT):
		// Darwin evaluates O_DIRECTORY against the link itself, so a
		// symlinked directory arrives as ENOTDIR and a dangling one as
		// ENOENT — the two errnos that also have innocent meanings. An
		// lstat tells them apart; the open already refused to traverse
		// anything, so all this decides is which error the caller sees.
		info, lerr := os.Lstat(dir)
		switch {
		case errors.Is(lerr, fs.ErrNotExist):
			return -1, false, errs.NewIO(fmt.Sprintf("`%s` is not there", dir), fs.ErrNotExist)
		case lerr != nil:
			return -1, false, errs.NewIO(fmt.Sprintf("could not stat `%s`", dir), lerr)
		case info.Mode()&fs.ModeSymlink != 0:
			return -1, false, &SymlinkRefusedError{Path: dir}
		default:
			return -1, false, &NotRegularError{Path: dir}
		}
	default:
		return -1, false, errs.NewIO(fmt.Sprintf("could not open `%s`", dir), err)
	}
}

// openDirAt opens one directory relative to dir, refusing anything that is
// not one. shown is the path the caller would recognise, used only for the
// error. found=false means nothing is there.
func openDirAt(dir int, name, shown string) (int, bool, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		return fd, true, nil
	case isSymlinkErrno(err):
		return -1, false, &SymlinkRefusedError{Path: shown}
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ENOENT):
		// The Darwin disambiguation [openDirAtPath] documents, through the
		// already-held parent descriptor.
		kind, kerr := entryAt(dir, name, shown)
		if kerr != nil {
			return -1, false, kerr
		}
		switch kind {
		case entrySymlink:
			return -1, false, &SymlinkRefusedError{Path: shown}
		case entryAbsent:
			return -1, false, nil
		default:
			return -1, false, &NotRegularError{Path: shown}
		}
	default:
		return -1, false, errs.NewIO(fmt.Sprintf("could not open `%s`", shown), err)
	}
}

// createDirAt creates one directory relative to dir and opens it, both
// without following links. Mkdirat's mode argument is masked by the
// process umask, and this directory holds refresh tokens, so the mode is
// set again on the descriptor rather than left to whatever the umask
// allowed.
func createDirAt(dir int, name, shown string) (int, error) {
	err := unix.Mkdirat(dir, name, uint32(config.DirMode))
	// Another process creating it first is a race this store wins by
	// re-opening rather than by failing; the re-open still refuses a link.
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, errs.NewIO(fmt.Sprintf("could not create `%s`", shown), err)
	}
	fd, found, err := openDirAt(dir, name, shown)
	if err != nil {
		return -1, err
	}
	if !found {
		return -1, errs.NewIO(fmt.Sprintf("`%s` vanished immediately after it was created", shown), fs.ErrNotExist)
	}
	if err := unix.Fchmod(fd, uint32(config.DirMode)); err != nil {
		_ = unix.Close(fd)
		return -1, errs.NewIO(fmt.Sprintf("could not set the mode of `%s`", shown), err)
	}
	return fd, nil
}

// OpenNamespaceDir opens a namespace directory without ever following a
// symbolic link, creating missing components at 0700.
//
// The walk starts at the namespace root — after that directory and the
// configuration directory above it have been created — and descends one
// component at a time. The returned descriptor is what every subsequent
// operation on the namespace is performed relative to, so no later
// operation re-resolves a path that could have changed underneath it. The
// caller closes it.
func OpenNamespaceDir(paths *config.Paths, nsDir string) (int, error) {
	// The two levels above the namespace root are this store's own roots,
	// and they may not exist yet: a login creates the store on its first
	// use. They are created by path; the walk below then refuses a link at
	// the root, so a redirected root is caught either way.
	if err := createDir0700(paths.ConfigDir()); err != nil {
		return -1, err
	}
	root := paths.NamespaceRoot()
	if err := createDir0700(root); err != nil {
		return -1, err
	}

	chain, found, err := openChain(root, nsDir, walkCreate)
	if err != nil {
		return -1, err
	}
	if !found || len(chain) == 0 {
		closeChain(chain)
		return -1, errs.NewIO(fmt.Sprintf("`%s` vanished while it was being created", nsDir), fs.ErrNotExist)
	}
	leaf := chain[len(chain)-1].fd
	closeChain(chain[:len(chain)-1])
	return leaf, nil
}

// OpenDirUnder opens one existing directory below anchor without ever
// following a symbolic link, for a caller that then operates relative to
// the descriptor. Resolving the way to a directory once — and then
// operating relative to what the walk produced — is what stops a link
// planted at a component from redirecting the whole protocol. The caller
// closes the descriptor.
func OpenDirUnder(anchor, dir string) (int, error) {
	chain, found, err := openChain(anchor, dir, walkMustExist)
	if err != nil {
		return -1, err
	}
	if !found || len(chain) == 0 {
		closeChain(chain)
		return -1, errs.NewIO(fmt.Sprintf("`%s` is not there", dir), fs.ErrNotExist)
	}
	leaf := chain[len(chain)-1].fd
	closeChain(chain[:len(chain)-1])
	return leaf, nil
}

// CreateDirUnder is [OpenDirUnder], but creating the directory when it is
// not there.
//
// The anchor itself is never created: every component below it is, so a
// walk that still ends early can only mean the anchor is absent — and
// creating that by path is the step this function exists to replace. The
// caller closes the descriptor.
func CreateDirUnder(anchor, dir string) (int, error) {
	chain, found, err := openChain(anchor, dir, walkCreate)
	if err != nil {
		return -1, err
	}
	if !found || len(chain) == 0 {
		closeChain(chain)
		return -1, errs.NewIO(fmt.Sprintf("`%s` is not there", anchor), fs.ErrNotExist)
	}
	leaf := chain[len(chain)-1].fd
	closeChain(chain[:len(chain)-1])
	return leaf, nil
}

// RemoveDirUnderRoot removes one empty directory under the namespace root,
// resolving the way to it without ever following a symbolic link.
//
// The lexical containment check compares spellings and cannot see a link
// planted at a component: a path can spell a location under the root while
// naming a live store in the user's home directory. So the parent is
// walked down from the root one no-follow component at a time and the
// artefact removed relative to the descriptor that walk produced.
func RemoveDirUnderRoot(paths *config.Paths, path string) error {
	// Stated here rather than left to the walk, which cannot answer until
	// the root itself exists — and "is this mine to remove?" is not a
	// question whose answer should depend on that.
	if !paths.IsUnderNamespaceRoot(path) {
		return &OutsideRootError{Path: path}
	}
	return RemoveDirUnder(paths.NamespaceRoot(), path)
}

// RemoveDirUnder is [RemoveDirUnderRoot] with the no-follow walk anchored
// elsewhere, for the one removal outside the namespace root: a lock
// directory a crashed process left in the store it was holding, where the
// anchor is that store directory's parent, leaving both components the
// record cannot vouch for to be walked and refused here.
//
// Nothing recurses. AT_REMOVEDIR fails on a directory with anything inside
// it, and that is reported: a lock directory holding a file is not the
// empty artefact a lapsed lock leaves. It also refuses anything that is
// not a directory, so a regular file or a symbolic link at the final
// component is reported rather than deleted.
func RemoveDirUnder(anchor, path string) error {
	parent := filepath.Dir(path)
	name := filepath.Base(path)
	if !config.IsSingleComponent(name) {
		return &OutsideRootError{Path: path}
	}
	chain, found, err := openChain(anchor, parent, walkMustExist)
	if err != nil {
		return err
	}
	if !found || len(chain) == 0 {
		closeChain(chain)
		return errs.NewIO(fmt.Sprintf("`%s` is not there", parent), fs.ErrNotExist)
	}
	defer closeChain(chain)
	leaf := chain[len(chain)-1].fd
	err = unix.Unlinkat(leaf, name, unix.AT_REMOVEDIR)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTEMPTY):
		return &NotEmptyError{Path: path}
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EISDIR):
		// Every parent was opened with O_DIRECTORY, so at this point
		// ENOTDIR can only be about the name itself: a regular file, a
		// socket, or a symbolic link, none of which AT_REMOVEDIR will
		// touch. EISDIR is the same refusal where a platform spells it
		// that way.
		return &NotRegularError{Path: path}
	default:
		return errs.NewIO(fmt.Sprintf("could not remove `%s`", path), err)
	}
}

// createDir0700 creates one directory — and any missing parents — at 0700,
// tolerating levels that already exist and leaving their modes alone,
// which doctor reports on rather than silently tightening. Every level is
// created individually rather than with one recursive call that would
// apply the umask's wider default to intermediate levels.
func createDir0700(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := createDir0700(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, config.DirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
				return nil
			}
		}
		return errs.NewIO(fmt.Sprintf("could not create the directory `%s`", dir), err)
	}
	return nil
}
