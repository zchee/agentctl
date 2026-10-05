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
	"io"
	"strings"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/render"
)

// ExportSpec is the environment delta for a session; it contains no credentials.
type ExportSpec struct {
	SecureStorageDir string
	ConfigDir        string
	MCPConfig        string
}

// SessionExport checks the stored namespace hash before exporting its spelling.
// Non-owned accounts and empty or inconsistent namespace records are refused.
func SessionExport(rec *config.AccountRecord, session SessionDir) (ExportSpec, error) {
	owned := rec.Kind.Owned
	if owned == nil {
		return ExportSpec{}, errs.NewConfig(fmt.Sprintf("only an account agentctl owns can be exported into a session; `%s` is `%s`, whose credentials live outside agentctl's own store", rec.AccountUUID, rec.Kind.Name()))
	}
	if owned.ExportSpelling == "" || owned.ExportSHA8 == "" {
		return ExportSpec{}, errs.NewConfig(fmt.Sprintf("`%s`'s registry record has an empty export spelling or hash, so no keychain service can be derived for it; run `agentctl claude login` again", rec.AccountUUID))
	}
	hash := claude.SHA8(owned.ExportSpelling)
	if hash != owned.ExportSHA8 {
		return ExportSpec{}, errs.NewConfig(fmt.Sprintf("`%s`'s export spelling `%s` hashes to `%s`, not the recorded `%s`; the namespace may have moved — see `agentctl claude accounts show %s`", rec.AccountUUID, owned.ExportSpelling, hash, owned.ExportSHA8, rec.AccountUUID))
	}
	return ExportSpec{SecureStorageDir: owned.ExportSpelling, ConfigDir: session.Path, MCPConfig: session.MCPConfig}, nil
}

// RenderEnv renders the shell-specific environment and optional MCP alias.
// The alias body is quoted twice so expanding it cannot interpret path content.
func RenderEnv(spec ExportSpec, shell cli.Shell) string {
	var lines []string
	if shell == cli.ShellFish {
		lines = append(lines, "set -gx "+claude.SecureStorageEnv+" "+quoteFish(spec.SecureStorageDir), "set -gx "+claude.ConfigDirEnv+" "+quoteFish(spec.ConfigDir), "# "+claude.OAuthTokenEnv+" would bypass this session's stored credential", "set -e "+claude.OAuthTokenEnv)
		if spec.MCPConfig != "" {
			lines = append(lines, "function claude\n    command claude --mcp-config "+quoteFish(spec.MCPConfig)+" $argv\nend", "# a fish function only reaches an interactive shell; a script started from one will not inherit it")
		}
	} else {
		lines = append(lines, "export "+claude.SecureStorageEnv+"="+quotePOSIX(spec.SecureStorageDir), "export "+claude.ConfigDirEnv+"="+quotePOSIX(spec.ConfigDir), "# "+claude.OAuthTokenEnv+" would bypass this session's stored credential", "unset "+claude.OAuthTokenEnv)
		if spec.MCPConfig != "" {
			lines = append(lines, "alias claude="+quotePOSIX("claude --mcp-config "+quotePOSIX(spec.MCPConfig)), "# an alias only reaches an interactive shell; a script started from one will not inherit it")
		}
	}
	return strings.Join(lines, "\n")
}

func quotePOSIX(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func quoteFish(value string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
}

func prepareSession(ctx context.Context, configDir, id string, opts SessionOptions) (SessionDir, ExportSpec, error) {
	paths, err := config.Resolve(configDir)
	if err != nil {
		return SessionDir{}, ExportSpec{}, err
	}
	if err := paths.EnsureDirs(ctx); err != nil {
		return SessionDir{}, ExportSpec{}, err
	}
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return SessionDir{}, ExportSpec{}, err
	}
	rec, err := registry.ResolveID(id)
	if err != nil {
		return SessionDir{}, ExportSpec{}, err
	}
	env := claude.EnvFromProcess()
	session, err := EnsureSession(ctx, paths, rec, opts, &env)
	if err != nil {
		return SessionDir{}, ExportSpec{}, err
	}
	spec, err := SessionExport(rec, session)
	return session, spec, err
}

// RunEnv prepares a session and prints its shell commands with one trailing LF.
func RunEnv(ctx context.Context, globals cli.Globals, opts cli.ClaudeEnvOptions, out io.Writer) error {
	_, spec, err := prepareSession(ctx, globals.ConfigDir, opts.ID, SessionOptions{ConfigDir: opts.ClaudeConfigDir, FreshContext: opts.FreshContext, NoMCP: opts.NoMCP})
	if err != nil {
		return err
	}
	return render.Print(out, RenderEnv(spec, opts.Shell))
}
