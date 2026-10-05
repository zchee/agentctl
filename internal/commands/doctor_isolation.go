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
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

const doctorManagedSettingsPath = "/Library/Application Support/ClaudeCode/managed-settings.json"

type doctorIsolation struct {
	Version int                   `json:"version"`
	Rows    []doctorIsolationRow  `json:"isolation"`
	Policy  doctorIsolationPolicy `json:"isolation_policy"`
}
type doctorIsolationRow struct {
	ID            string                 `json:"id"`
	Path          string                 `json:"path"`
	Exports       doctorIsolationExports `json:"exports"`
	Links         []doctorIsolationLink  `json:"links"`
	SeededKeys    []string               `json:"seeded_keys"`
	LeakedKeys    []string               `json:"leaked_keys"`
	Unexposed     []string               `json:"unexposed"`
	MCP           doctorIsolationMCP     `json:"mcp"`
	Drift         doctorIsolationDrift   `json:"drift"`
	Migrated      bool                   `json:"migrated"`
	ForgetCommand string                 `json:"forget_command"`
}
type doctorIsolationExports struct {
	SecureStorageDir string `json:"securestorage_dir"`
	ConfigDir        string `json:"config_dir"`
	SHA8Match        bool   `json:"sha8_match"`
}
type doctorIsolationLink struct {
	Name   string  `json:"name"`
	Tier   string  `json:"tier"`
	State  string  `json:"state"`
	Target *string `json:"target"`
}
type doctorIsolationMCP struct {
	Linked            bool    `json:"linked"`
	Target            *string `json:"target"`
	CredentialEntries *uint32 `json:"credential_entries"`
}
type doctorIsolationDrift struct {
	LiveMtimeMS      *int64 `json:"live_mtime_ms"`
	SeedMtimeMS      *int64 `json:"seed_mtime_ms"`
	ChangedSinceSeed bool   `json:"changed_since_seed"`
}
type doctorIsolationPolicy struct {
	DisableSideloadFlags *bool `json:"disable_sideload_flags"`
	BackendObservable    bool  `json:"backend_observable"`
}

func (d *Doctor) collectIsolation(registry *config.Registry, listing []secret.ServiceEntry) doctorIsolation {
	out := doctorIsolation{Version: 1}
	liveDir, liveJSON := claude.LiveStoreDir(d.Env), claude.ClaudeJSONPath(d.Env)
	unexposed := doctorUnexposed(liveDir)
	accounts, _ := os.ReadDir(d.Paths.SessionRoot())
	for _, account := range accounts {
		accountPath := filepath.Join(d.Paths.SessionRoot(), account.Name())
		info, err := os.Stat(accountPath)
		if err != nil || !info.IsDir() {
			continue
		}
		orgs, _ := os.ReadDir(accountPath)
		for _, org := range orgs {
			path := filepath.Join(accountPath, org.Name())
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() {
				continue
			}
			row := doctorIsolationRow{ID: "unregistered", Path: path, Unexposed: unexposed}
			row.Exports = doctorIsolationExports{SecureStorageDir: claude.ExportSpelling(d.Paths.NamespaceDir(account.Name(), org.Name())), ConfigDir: path}
			target := path
			if registered := registry.Get(account.Name(), org.Name()); registered != nil {
				row.ID = registered.DisplayID(registry.Accounts)
				target = row.ID
				if owned := registered.Kind.Owned; owned != nil {
					row.Exports.SecureStorageDir = owned.ExportSpelling
					row.Exports.SHA8Match = claude.SHA8(owned.ExportSpelling) == owned.ExportSHA8
					row.Migrated = slices.ContainsFunc(listing, func(entry secret.ServiceEntry) bool { return entry.Service == claude.LiveService+"-"+owned.ExportSHA8 })
				}
			}
			row.ForgetCommand = "agentctl claude use --forget " + target
			for _, name := range SessionTierOne() {
				row.Links = append(row.Links, doctorLink(path, name, "tier1", filepath.Join(liveDir, name)))
			}
			for _, name := range SessionTierTwo() {
				row.Links = append(row.Links, doctorLink(path, name, "tier2", filepath.Join(liveDir, name)))
			}
			mcp := doctorLink(path, "mcp.json", "mcp", liveJSON)
			row.MCP.Target = mcp.Target
			if info, err := os.Lstat(filepath.Join(path, "mcp.json")); err == nil && info.Mode()&os.ModeSymlink != 0 {
				row.MCP.Linked = true
				doctorReadDocument(filepath.Join(path, "mcp.json"), true, func(data []byte) { row.MCP.CredentialEntries = doctorMCPCredentials(data) })
			}
			row.Links = append(row.Links, mcp)
			seedPath := filepath.Join(path, ".claude.json")
			seed := doctorIsolationLink{Name: ".claude.json", Tier: "seed", State: "absent"}
			if info, err := os.Lstat(seedPath); err == nil {
				seed.State = "occupied"
				if info.Mode().IsRegular() {
					seed.State = "seeded"
				}
			}
			row.Links = append(row.Links, seed)
			doctorReadDocument(seedPath, false, func(data []byte) { row.SeededKeys = doctorJSONKeys(data) })
			for _, key := range row.SeededKeys {
				if slices.Contains(NeverSeeded(), key) {
					row.LeakedKeys = append(row.LeakedKeys, key)
				}
			}
			row.Drift.LiveMtimeMS, row.Drift.SeedMtimeMS = doctorMtimeMS(liveJSON), doctorMtimeMS(seedPath)
			row.Drift.ChangedSinceSeed = row.Drift.LiveMtimeMS != nil && row.Drift.SeedMtimeMS != nil && *row.Drift.LiveMtimeMS > *row.Drift.SeedMtimeMS
			out.Rows = append(out.Rows, row)
		}
	}
	out.Policy.DisableSideloadFlags = doctorDisableFlags(filepath.Join(liveDir, "settings.json"), doctorManagedSettingsPath)
	return out
}

// Configuration can carry MCP credentials; keep its bytes inside one exposure and retain only diagnostic metadata.
func doctorReadDocument(path string, follow bool, visit func([]byte)) {
	read := secret.ReadFile
	if follow {
		read = secret.ReadFileFollowing
	}
	file, err := read(path, claude.MaxClaudeJSONBytes)
	if err != nil || !file.Present {
		return
	}
	value, err := secret.NewSecret(file.Bytes)
	if err != nil {
		return
	}
	_ = value.WithPlaintext(func(data []byte) error { visit(data); return nil })
}

func doctorLink(session, name, tier, expected string) doctorIsolationLink {
	out := doctorIsolationLink{Name: name, Tier: tier, State: "absent"}
	path := filepath.Join(session, name)
	info, err := os.Lstat(path)
	if err != nil {
		return out
	}
	out.State = "occupied"
	if info.Mode()&os.ModeSymlink == 0 {
		return out
	}
	if target, err := os.Readlink(path); err == nil {
		out.Target = new(target)
	}
	resolved, err := claude.Canonical(path)
	if err != nil {
		out.State = "missing-target"
		return out
	}
	if want, err := claude.Canonical(expected); err == nil && resolved == want {
		out.State = "linked"
	}
	return out
}

func doctorUnexposed(path string) []string {
	entries, _ := os.ReadDir(path)
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !slices.Contains(SessionTierOne(), name) && !slices.Contains(SessionTierTwo(), name) && !slices.Contains(NeverLinked(), name) {
			out = append(out, name)
		}
	}
	return out
}

func doctorJSONKeys(data []byte) []string {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	if dec.PeekKind() != '{' {
		return nil
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil
	}
	var out []string
	for dec.PeekKind() != '}' {
		token, err := dec.ReadToken()
		if err != nil || token.Kind() != '"' {
			return nil
		}
		out = append(out, token.String())
		if dec.SkipValue() != nil {
			return nil
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return nil
	}
	slices.Sort(out)
	return out
}

// Only object cardinalities survive parsing; credential values are skipped without decoding strings.
type doctorObjectCount uint32

func (count *doctorObjectCount) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	*count = 0
	if dec.PeekKind() != '{' {
		return dec.SkipValue()
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	for dec.PeekKind() != '}' {
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		if err := dec.SkipValue(); err != nil {
			return err
		}
		*count++
	}
	_, err := dec.ReadToken()
	return err
}

func doctorMCPCredentials(data []byte) *uint32 {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	count, err := doctorMCPCount(dec, 0)
	if err != nil {
		return nil
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return nil
	}
	return new(count)
}

func doctorMCPCount(dec *jsontext.Decoder, depth int) (uint32, error) {
	if dec.PeekKind() != '{' {
		return 0, dec.SkipValue()
	}
	if _, err := dec.ReadToken(); err != nil {
		return 0, err
	}
	var count uint32
	for dec.PeekKind() != '}' {
		key, err := dec.ReadToken()
		if err != nil {
			return 0, err
		}
		switch {
		case depth == 0 && key.String() == "mcpServers", depth == 1:
			value, err := doctorMCPCount(dec, depth+1)
			if err != nil {
				return 0, err
			}
			count += value
		case depth == 2 && (key.String() == "env" || key.String() == "headers"):
			var fields doctorObjectCount
			if err := fields.UnmarshalJSONFrom(dec); err != nil {
				return 0, err
			}
			if fields > 0 {
				count = 1
			}
		default:
			if err := dec.SkipValue(); err != nil {
				return 0, err
			}
		}
	}
	_, err := dec.ReadToken()
	return count, err
}

func doctorMtimeMS(path string) *int64 {
	info, err := os.Stat(path)
	if err != nil || info.ModTime().UnixMilli() < 0 {
		return nil
	}
	return new(info.ModTime().UnixMilli())
}

func doctorDisableFlags(paths ...string) *bool {
	var result *bool
	for _, path := range paths {
		doctorReadDocument(path, true, func(data []byte) {
			var value struct {
				Policy struct {
					Disabled *bool `json:"disableSideloadFlags"`
				} `json:"policySettings"`
			}
			if json.Unmarshal(data, &value) == nil && value.Policy.Disabled != nil && (result == nil || *value.Policy.Disabled) {
				result = value.Policy.Disabled
			}
		})
	}
	return result
}

func doctorIsolationSection(data *doctorIsolation, root string) []string {
	out := []string{"isolation"}
	if len(data.Rows) == 0 {
		out = append(out, "  "+root+" has no isolated sessions")
	}
	words := func(values []string, absent string) string {
		if len(values) == 0 {
			return absent
		}
		return strings.Join(values, ", ")
	}
	optionalTime := func(value *int64) string {
		if value == nil {
			return "None"
		}
		return fmt.Sprintf("Some(%d)", *value)
	}
	for i := range data.Rows {
		row := &data.Rows[i]
		out = append(out, fmt.Sprintf("  %s  id=%s", row.Path, row.ID), fmt.Sprintf("    exports          CLAUDE_SECURESTORAGE_CONFIG_DIR=%s CLAUDE_CONFIG_DIR=%s sha8_match=%t migrated=%t", row.Exports.SecureStorageDir, row.Exports.ConfigDir, row.Exports.SHA8Match, row.Migrated))
		for _, link := range row.Links {
			if link.Target == nil {
				out = append(out, fmt.Sprintf("    %-14s %-5s %s", link.Name, link.Tier, link.State))
			} else {
				out = append(out, fmt.Sprintf("    %-14s %-5s %-14s -> %s", link.Name, link.Tier, link.State, *link.Target))
			}
		}
		out = append(out, "    unexposed        "+words(row.Unexposed, "none"), "    seeded keys      "+words(row.SeededKeys, "not seeded"))
		leaked := words(row.LeakedKeys, "none")
		if len(row.LeakedKeys) > 0 {
			leaked += "  — must never appear in a seeded `.claude.json`"
		}
		out = append(out, "    leaked keys      "+leaked)
		target, count := "(none)", "unreadable"
		if row.MCP.Target != nil {
			target = *row.MCP.Target
		}
		if row.MCP.CredentialEntries != nil {
			count = strconv.FormatUint(uint64(*row.MCP.CredentialEntries), 10)
		}
		out = append(out, fmt.Sprintf("    mcp              linked=%t target=%s credential_entries=%s", row.MCP.Linked, target, count), fmt.Sprintf("    drift            live_mtime_ms=%s seed_mtime_ms=%s changed_since_seed=%t", optionalTime(row.Drift.LiveMtimeMS), optionalTime(row.Drift.SeedMtimeMS), row.Drift.ChangedSinceSeed), "    forget           "+row.ForgetCommand)
	}
	policy := "not set"
	if data.Policy.DisableSideloadFlags != nil {
		policy = "false"
		if *data.Policy.DisableSideloadFlags {
			policy = "true — `--mcp-config` is refused by managed settings; isolated sessions will start without MCP"
		}
	}
	return append(out, "  policySettings.disableSideloadFlags   "+policy, "  secure-storage backend: not independently observable from outside a session (environment-only); as verified, this build's backend factory is a stub, so none is active — see docs/re-verify.md")
}
