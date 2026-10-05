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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/awnumar/memguard"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/runtime/tty"
	"github.com/zchee/agentctl/internal/secret"
)

// LoginIO extends shared prompting with the browser and authorization-code input.
type LoginIO interface {
	Prompter
	OpenBrowser(ctx context.Context, url string)
	ReadLine(ctx context.Context, prompt string) (string, error)
	Warn(message string) error
}

// LoginTerminal handles login input without granting consent to piped answers.
type LoginTerminal struct {
	TerminalPrompt
	Err io.Writer
}

// OpenBrowser tries the desktop opener after the URL has been printed.
func (p LoginTerminal) OpenBrowser(ctx context.Context, url string) { claude.OpenBrowser(ctx, url) }

// Warn writes a notice separately from the login's machine-consumed output.
func (p LoginTerminal) Warn(message string) error {
	_, err := fmt.Fprintln(p.Err, message)
	return err
}

// Confirm requires a terminal because login has no noninteractive overwrite flag.
func (p LoginTerminal) Confirm(ctx context.Context, question string) (bool, error) {
	if !tty.IsTerminal(p.In) {
		return false, errs.NewConfig(question + " — refusing: `login` overwrites stored credentials only after an interactive confirmation, and standard input is not a terminal")
	}
	return p.TerminalPrompt.Confirm(ctx, question)
}

// ReadLine reads one pasted authorization code while remaining cancellable.
func (p LoginTerminal) ReadLine(ctx context.Context, prompt string) (string, error) {
	if _, err := io.WriteString(p.Out, prompt); err != nil {
		return "", errs.NewIO("could not write the login prompt", err)
	}
	if out, ok := p.Out.(interface{ Flush() error }); ok {
		if err := out.Flush(); err != nil {
			return "", errs.NewIO("could not write the login prompt", err)
		}
	}
	var line strings.Builder
	var readiness tty.Readiness
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ready, err := readiness.WaitReadable(p.In, claude.LoopbackPollInterval)
		if err != nil {
			return "", errs.NewIO("could not read the pasted authorization code", err)
		}
		if !ready {
			continue
		}
		var b [1]byte
		n, err := p.In.Read(b[:])
		if err != nil && !errors.Is(err, io.EOF) {
			return "", errs.NewIO("could not read the pasted authorization code", err)
		}
		if n == 0 || b[0] == '\n' {
			return line.String(), nil
		}
		line.WriteByte(b[0])
	}
}

// Login owns one authorization exchange and the store it may write afterwards.
type Login struct {
	Paths              *config.Paths
	Client             *claude.LoginClient
	IO                 LoginIO
	LiveIdentity       *claude.Identity
	LiveIdentitySource string
}

// Run authorizes a fresh session, confirms replacements, then persists it under locks.
func (l *Login) Run(ctx context.Context, opts cli.ClaudeLoginOptions) error {
	pkce, err := claude.NewPKCE()
	if err != nil {
		return errs.NewConfig(err.Error())
	}
	scopes := claude.RequestedScopes()
	var listener *net.TCPListener
	redirect := claude.Redirect{}
	if !opts.Manual {
		listener, err = claude.ListenLoopback(ctx)
		if err != nil {
			return errs.NewConfig(err.Error())
		}
		defer func() { _ = listener.Close() }()
		redirect.Port = uint16(listener.Addr().(*net.TCPAddr).Port)
	}
	url, err := l.Client.AuthorizeURL(pkce, redirect, scopes)
	if err != nil {
		return errs.NewConfig(err.Error())
	}
	if err := l.IO.Tell(fmt.Sprintf("Open this URL to authorize agentctl:\n\n  %s\n", url)); err != nil {
		return err
	}
	l.IO.OpenBrowser(ctx, url)
	var code string
	if listener != nil {
		if err := l.IO.Tell("Waiting for the browser to come back…"); err != nil {
			return err
		}
		code, err = claude.LoopbackWait(ctx, listener, pkce.State, time.Now().Add(claude.LoopbackTimeout))
	} else {
		var pasted, state string
		pasted, err = l.IO.ReadLine(ctx, "Paste the `code#state` value the page shows: ")
		if err == nil {
			code, state, err = claude.ParseManualCode(pasted)
		}
		if err == nil {
			err = claude.VerifyState(pkce.State, state)
		}
	}
	if err != nil {
		return errs.NewConfig(err.Error())
	}
	response, err := l.Client.Exchange(ctx, code, pkce.State, pkce, redirect)
	if err != nil {
		return errs.NewConfig(err.Error())
	}
	credentials, err := claude.LoginCredentials(response, time.Now().UnixMilli(), scopes)
	if err != nil {
		return errs.NewConfig(err.Error())
	}
	profile, err := l.Client.ProfileOf(ctx, credentials)
	if err == nil {
		applyLoginProfile(credentials, profile)
	} else if credentials.Identity() == nil {
		slog.Warn("could not read the account profile", "error", err)
	} else {
		slog.Info("could not read the account profile", "error", err)
	}
	identity := credentials.Identity()
	if identity == nil {
		return errs.NewConfig("the login succeeded but named no account, so agentctl cannot tell which account these credentials belong to; nothing was written")
	}
	account, organization := identity.AccountUUID, config.UnknownOrg
	if identity.OrganizationUUID != nil {
		organization = *identity.OrganizationUUID
	}
	if err := config.ValidateSegment(account); err != nil {
		return err
	}
	if err := config.ValidateSegment(organization); err != nil {
		return err
	}
	if isLoginLiveIdentity(l.LiveIdentity, account, organization) {
		if opts.NoDuplicate {
			return errs.NewConfig(fmt.Sprintf("`%s/%s` is already the account Claude Code is signed in as (per `%s`), and `--no-duplicate` was given, so nothing was written. To hand Claude Code a different account, run `agentctl claude use --live <id>`, which adopts the credential it displaces. To mint a second independent session for this account after all, run the same login without `--no-duplicate`.", account, organization, l.LiveIdentitySource))
		}
		if err := l.IO.Warn(fmt.Sprintf("Note: `%s/%s` is the account Claude Code is signed in as right now. This login mints a second, independent session; both stay valid. To hand Claude Code a different account instead, use `agentctl claude use --live <id>`.", account, organization)); err != nil {
			return err
		}
	}
	registry, err := config.LoadRegistry(ctx, l.Paths)
	if err != nil {
		return err
	}
	if registry.Get(account, organization) != nil {
		confirmed, err := l.IO.Confirm(ctx, fmt.Sprintf("`%s/%s` is already logged in. Replace its stored credentials?", account, organization))
		if err != nil {
			return err
		}
		if !confirmed {
			return errs.NewConfig("login cancelled; nothing was written")
		}
	}
	if err := l.Paths.EnsureDirs(ctx); err != nil {
		return err
	}
	nsDir := l.Paths.NamespaceDir(account, organization)
	guard, err := secret.Acquire(ctx, l.Paths.LocksDir(), filepath.Base(l.Paths.LockPath(account, organization)), time.Now().Add(secret.NamespaceLockWait))
	if err != nil {
		return errs.NewConfig(fmt.Sprintf("could not take the namespace lock: %v", err))
	}
	defer func() { _ = guard.Release() }()
	if err := ClearStaleFiles(ctx, l.Paths, nsDir); err != nil {
		return err
	}
	prior := loginPriorDigests(nsDir)
	blob, err := credentials.BlobJSON()
	if err != nil {
		return err
	}
	defer memguard.WipeBytes(blob)
	writeCtx, cancel := context.WithTimeout(ctx, claude.TokenTimeout)
	defer cancel()
	outcome, err := secret.WriteCredentials(writeCtx, &secret.WriteRequest{Paths: l.Paths, NSDir: nsDir, BlobJSON: blob, Prior: prior, NewExpiresAtMS: credentials.ExpiresAtMillis})
	if err != nil {
		return errs.NewConfig(fmt.Sprintf("could not store the credentials: %v", err))
	}
	spelling := claude.ExportSpelling(nsDir)
	record, err := config.NewRecord(account, organization, config.AccountKindOwned(spelling, claude.SHA8(spelling)))
	if err != nil {
		return err
	}
	record.Email, record.OrgName = identity.Email, identity.OrgName
	if opts.Label != "" {
		record.Label = new(opts.Label)
	}
	if err := config.UpdateRegistry(ctx, l.Paths, func(registry *config.Registry) { registry.Upsert(record) }); err != nil {
		return err
	}
	if err := guard.Release(); err != nil {
		return err
	}
	if outcome.SavedToPending {
		slog.Warn("the credential file could not be replaced; saved as pending", "error", outcome.PendingError)
		if err := l.IO.Tell("The credentials could not be written into place and were saved as pending; the next `agentctl claude status` will finish the job."); err != nil {
			return err
		}
	}
	id := account
	if registry, err := config.LoadRegistry(ctx, l.Paths); err == nil {
		if record := registry.Get(account, organization); record != nil {
			id = record.DisplayID(registry.Accounts)
		}
	}
	if identity.Email != nil {
		return l.IO.Tell(fmt.Sprintf("Logged in as %s (%s).", *identity.Email, id))
	}
	return l.IO.Tell(fmt.Sprintf("Logged in (%s).", id))
}

func isLoginLiveIdentity(live *claude.Identity, account, organization string) bool {
	if live == nil {
		return false
	}
	liveOrg := config.UnknownOrg
	if live.OrganizationUUID != nil {
		liveOrg = *live.OrganizationUUID
	}
	return live.AccountUUID == account && liveOrg == organization
}

func applyLoginProfile(credentials *claude.Credentials, profile *claude.Profile) {
	if exchanged := credentials.Identity(); exchanged != nil {
		if exchanged.AccountUUID == profile.AccountUUID && (exchanged.OrganizationUUID == nil || *exchanged.OrganizationUUID == profile.OrganizationUUID) {
			claude.FillPlan(credentials, profile)
		} else {
			slog.Warn("the account profile names another account; the plan was not recorded")
		}
		return
	}
	claude.FillPlan(credentials, profile)
	if credentials.TokenAccount == nil {
		credentials.TokenAccount = &claude.TokenAccount{}
	}
	credentials.TokenAccount.UUID = new(profile.AccountUUID)
	credentials.TokenAccount.EmailAddress = new(profile.Email)
	credentials.TokenAccount.OrganizationUUID = new(profile.OrganizationUUID)
	if name, ok := profile.OrganizationName(); ok {
		credentials.TokenAccount.OrganizationName = new(name)
	}
}

// ClearStaleFiles removes superseded pending credentials and recognized temporary files.
// The caller must hold the namespace lock. Missing directories are left missing.
func ClearStaleFiles(ctx context.Context, paths *config.Paths, nsDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !paths.IsUnderNamespaceRoot(nsDir) {
		return errs.NewConfig(fmt.Sprintf("`%s` is outside the agentctl namespace root; refusing to touch it", nsDir))
	}
	dir, err := secret.OpenDirUnder(paths.NamespaceRoot(), nsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(dir), nsDir)
	defer func() { _ = file.Close() }()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return errs.NewIO(fmt.Sprintf("could not list `%s`", nsDir), err)
	}
	targets := []string{secret.PendingFile, secret.PendingMetaFile}
	for _, name := range names {
		if suffix, ok := strings.CutPrefix(name, secret.CredentialsFile+".tmp."); ok && len(suffix) == 8 {
			if _, err := hex.DecodeString(suffix); err == nil {
				targets = append(targets, name)
			}
		}
	}
	for _, name := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := secret.NewSecretFile(paths.NamespaceRoot(), dir, name, filepath.Join(nsDir, name)).Remove(); err != nil {
			return err
		}
	}
	return nil
}

func loginPriorDigests(nsDir string) *secret.Digests {
	prior, err := secret.ReadCredentials(nsDir)
	if err != nil || !prior.Present {
		return nil
	}
	defer memguard.WipeBytes(prior.Bytes)
	credentials, err := claude.ParseBlob(prior.Bytes)
	if err != nil {
		return nil
	}
	digests, err := credentials.Digests()
	if err != nil {
		return nil
	}
	return &secret.Digests{AccessSHA256: digests.AccessSHA256, RefreshSHA256: digests.RefreshSHA256}
}
