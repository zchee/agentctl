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
	"fmt"
	"os"
	"syscall"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

// RunForget removes an owned account's isolated session after confirmation.
// Credentials, the namespace, and its lock file are never removed.
func (p SessionProcess) RunForget(ctx context.Context, globals cli.Globals, opts cli.ClaudeUseOptions) error {
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return err
	}
	if err := paths.EnsureDirs(ctx); err != nil {
		return err
	}
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return err
	}
	rec, err := registry.ResolveID(opts.Forget)
	if err != nil {
		return err
	}
	in, _ := p.In.(*os.File)
	return ForgetSession(ctx, paths, rec, TerminalPrompt{In: in, Out: p.Out}, opts.Yes)
}

// ForgetSession removes only the session entry, without following symlinks.
// A missing session is a successful no-op; other accounts and declined prompts
// are refused without removing anything.
func ForgetSession(ctx context.Context, paths *config.Paths, rec *config.AccountRecord, prompt Prompter, yes bool) error {
	if rec.Kind.Owned == nil {
		return errs.NewConfig(fmt.Sprintf("only an account agentctl owns can have an isolated session; `%s` is `%s`, which has none", rec.AccountUUID, rec.Kind.Name()))
	}
	path := paths.SessionDir(rec.AccountUUID, rec.OrganizationUUID)
	if !paths.IsUnderSessionRoot(path) {
		return errs.NewConfig(fmt.Sprintf("`%s` is not under `%s`; refusing to remove it", path, paths.SessionRoot()))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return prompt.Tell(fmt.Sprintf("`%s` has no isolated session directory; nothing to forget.", rec.AccountUUID))
	}
	if !yes {
		if err := prompt.Tell(fmt.Sprintf("This removes the isolated session directory `%s` for `%s`, including its `.claude.json` seed and every symlink into your live Claude Code configuration. The account's own stored credentials and namespace are not touched.", path, rec.AccountUUID)); err != nil {
			return err
		}
		confirmed, err := prompt.Confirm(ctx, "Remove this session directory?")
		if err != nil {
			return err
		}
		if !confirmed {
			return errs.NewRefused(0, "cancelled; nothing was removed")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		err = syscall.ENOTDIR
	}
	if err == nil {
		err = os.RemoveAll(path)
	}
	if err != nil {
		return errs.NewIO(fmt.Sprintf("could not remove `%s`", path), err)
	}
	return nil
}
