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

package testutil

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zchee/agentctl/fixtures"
)

// installScriptFile writes one embedded fixture script to path with the
// executable bit set, because embedding drops file modes.
func installScriptFile(name, path string) error {
	data, err := fixtures.FS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read embedded script %q: %w", name, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// installScript is installScriptFile failing the test on error.
func (f *Fixture) installScript(name, path string) {
	f.tb.Helper()
	if err := installScriptFile(name, path); err != nil {
		f.tb.Fatalf("install %q: %v", name, err)
	}
}

// WithKeychain installs the fake security(1) and switches the keychain on.
//
// The script's own knobs keep their original names, because the script is
// installed verbatim; the harness sets them.
func (f *Fixture) WithKeychain() *Fixture {
	f.tb.Helper()
	f.installScript("fake-security.sh", f.SecurityBin())
	f.Unset("AGENTCTL_KEYCHAIN_BACKEND")
	f.Set("AGENTCTL_SECURITY_BIN", f.SecurityBin())
	f.Set("AGCTL_FAKE_SECURITY_ROOT", f.root)
	f.Set("AGCTL_FAKE_SECURITY_LOG", f.SecurityLogPath())
	f.Set("AGCTL_FAKE_SECURITY_ITEMS", f.ItemsDir())
	f.Set("AGCTL_FAKE_SECURITY_DUMP", f.KeychainDumpPath())
	f.Dump()
	return f
}

// SecurityBin returns where the fake security(1) is installed.
func (f *Fixture) SecurityBin() string {
	return filepath.Join(f.BinDir(), "security")
}

// SecurityLogPath returns where the fake security logs one line per
// invocation.
func (f *Fixture) SecurityLogPath() string {
	return filepath.Join(f.root, "security-argv.log")
}

// ItemsDir returns where the fake keychain items live.
func (f *Fixture) ItemsDir() string {
	return filepath.Join(f.root, "keychain-items")
}

// KeychainDumpPath returns where the fake dump-keychain output lives.
func (f *Fixture) KeychainDumpPath() string {
	return filepath.Join(f.root, "keychain-dump.txt")
}

// KeychainItemPath returns the file one item's password is stored in,
// under this fixture's own account.
func (f *Fixture) KeychainItemPath(service string) string {
	return f.KeychainItemPathFor(KeychainAccount, service)
}

// KeychainItemPathFor returns the file one item's password would be stored
// in under account.
//
// A generic password is identified by its account and its service, so this
// is what a write with the wrong account creates and what a read with the
// right one will never serve.
func (f *Fixture) KeychainItemPathFor(account, service string) string {
	return filepath.Join(f.ItemsDir(), ItemFileName(account), ItemFileName(service))
}

// KeychainItemEntry is one item file the fake keychain holds.
type KeychainItemEntry struct {
	// Name is "<account>/<file>".
	Name string
	// Data is the item file's bytes.
	Data []byte
}

// KeychainItems returns every item file the fake keychain holds, sorted.
//
// The whole keychain rather than one item, for the tests whose claim is
// that nothing was added, changed or removed - a claim that has to see a
// sibling under another account to be worth making.
func (f *Fixture) KeychainItems() []KeychainItemEntry {
	f.tb.Helper()
	var items []KeychainItemEntry
	for _, account := range f.KeychainAccounts() {
		dir := filepath.Join(f.ItemsDir(), account)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				f.tb.Fatalf("read item %q: %v", entry.Name(), err)
			}
			items = append(items, KeychainItemEntry{Name: account + "/" + entry.Name(), Data: data})
		}
	}
	slices.SortFunc(items, func(a, b KeychainItemEntry) int { return strings.Compare(a.Name, b.Name) })
	return items
}

// KeychainAccounts returns every account directory the fake keychain has
// items under, sorted.
func (f *Fixture) KeychainAccounts() []string {
	f.tb.Helper()
	entries, err := os.ReadDir(f.ItemsDir())
	if err != nil {
		f.tb.Fatalf("read items directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}

// KeychainItem gives the fake keychain one readable item.
func (f *Fixture) KeychainItem(service, blob string) *Fixture {
	f.tb.Helper()
	path := f.KeychainItemPath(service)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.tb.Fatalf("create account directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		f.tb.Fatalf("write keychain item: %v", err)
	}
	return f
}

// AllowWrite registers service as a service the fake will let a write
// reach. The stand-in refuses any other service: a write is the one
// operation where a test that forgot to say which item must not silently
// succeed.
func (f *Fixture) AllowWrite(service string) *Fixture {
	f.tb.Helper()
	path := filepath.Join(f.ItemsDir(), ".allowed-services")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.tb.Fatalf("create allowlist: %v", err)
	}
	if _, err := fmt.Fprintln(file, service); err != nil {
		f.tb.Fatalf("write allowlist: %v", err)
	}
	if err := file.Close(); err != nil {
		f.tb.Fatalf("close allowlist: %v", err)
	}
	return f
}

// dumpListing renders what dump-keychain lists, in security(1)'s own
// format.
func dumpListing(services ...string) string {
	var text strings.Builder
	text.WriteString("keychain: \"/Users/example/Library/Keychains/login.keychain-db\"\nversion: 512\n")
	for _, service := range services {
		text.WriteString("class: \"genp\"\nattributes:\n")
		fmt.Fprintf(&text, "    0x00000007 <blob>=%q\n", service)
		fmt.Fprintf(&text, "    \"acct\"<blob>=%q\n", KeychainAccount)
		text.WriteString("    \"cdat\"<timedate>=0x32303236303930383031323030355A00  \"20260908012005Z\\000\"\n")
		text.WriteString("    \"mdat\"<timedate>=0x32303236303930383031323030355A00  \"20260908012005Z\\000\"\n")
		fmt.Fprintf(&text, "    \"svce\"<blob>=%q\n", service)
		text.WriteString("    \"type\"<uint32>=<NULL>\n")
	}
	return text.String()
}

// Dump sets what dump-keychain lists, in security(1)'s own format.
func (f *Fixture) Dump(services ...string) *Fixture {
	f.tb.Helper()
	if err := os.WriteFile(f.KeychainDumpPath(), []byte(dumpListing(services...)), 0o644); err != nil {
		f.tb.Fatalf("write keychain listing: %v", err)
	}
	return f
}

// SecurityLog returns every argv line the fake security has recorded so
// far.
func (f *Fixture) SecurityLog() []string {
	data, err := os.ReadFile(f.SecurityLogPath())
	if err != nil {
		return nil
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// AssertKeychainReadOnly fails the test when any security subcommand other
// than the three read-only ones was issued - which would mean a keychain
// write path exists where none should.
func (f *Fixture) AssertKeychainReadOnly() {
	f.tb.Helper()
	for _, line := range f.SecurityLog() {
		subcommand, _, _ := strings.Cut(line, " ")
		switch subcommand {
		case "show-keychain-info", "find-generic-password", "dump-keychain":
		default:
			f.tb.Fatalf("a security %q was issued, which the read path has no code for; full argv: %s", subcommand, line)
		}
	}
}

// SecurityWriteArgvCommand returns an unstarted command that runs the fake
// security the way the argv write transport does: add-generic-password with
// the account, the service and the hex payload in argv, and no standard
// input. A test that needs a process group or a wait delay sets them before
// starting it.
func (f *Fixture) SecurityWriteArgvCommand(account, service, hex string) *exec.Cmd {
	f.tb.Helper()
	cmd := exec.CommandContext(testContext(f.tb), f.SecurityBin(), "add-generic-password", "-U", "-a", account, "-s", service, "-X", hex)
	return f.Apply(cmd)
}

// SecurityWriteArgv runs SecurityWriteArgvCommand to completion and returns
// what it said.
func (f *Fixture) SecurityWriteArgv(account, service, hex string) Output {
	f.tb.Helper()
	return Run(f.tb, f.SecurityWriteArgvCommand(account, service, hex))
}

// ItemFileName returns the name fold the fake security applies to an
// account or a service. The fold has to match the tr -c 'A-Za-z0-9._-' '_'
// inside the installed script; the two change together.
func ItemFileName(service string) string {
	var out strings.Builder
	out.Grow(len(service))
	for _, c := range service {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			out.WriteRune(c)
		default:
			out.WriteByte('_')
		}
	}
	return out.String()
}

// slowWriteClause is the shell a test wrapper around the fake security runs
// first, to make a keychain write hang past the writer's budget: for an
// add-generic-password it logs the redacted line the fake would log, with
// the payload reduced to its length, and sleeps, so the writer's timeout
// ends the wrapper before the fake stores anything.
const slowWriteClause = "if [ \"$1\" = add-generic-password ]; then printf 'add-generic-password -U -a \"%s\" -s \"%s\" -X <REDACTED:%s>\\n' \"$4\" \"$6\" \"${#8}\" >> \"$AGCTL_FAKE_SECURITY_LOG\"; sleep 5; fi\n"

// KeychainBlobCeiling is the largest credential document, in plaintext
// bytes, the production writer hands to security(1): 256 KiB. The fixtures
// size their over-ceiling items against it.
const KeychainBlobCeiling = 256 << 10

// SiblingCanary is the marker the carry fixtures put inside a sibling value.
// It may appear only in the keychain item files themselves.
const SiblingCanary = "SIBLING-CANARY-9f31"

// itemMember is one top-level member of a keychain document: its name and
// its value as compact JSON.
type itemMember struct {
	Name  string
	Value jsontext.Value
}

// siblingPad returns n bytes of filler that is not hexadecimal, so a padded
// document never contains the long hex run the leak sweep looks for.
func siblingPad(n int) string {
	return strings.Repeat("z", max(n, 0))
}

// siblingSet returns the members a carry fixture seeds beside the account
// object, in the order Claude Code writes them. "seeded" is the live-sized
// set (two MCP logins of about 8 KB and 700 B, one client config, one plugin
// secret holding the canary); "changed" differs from it in mcpOAuth only, as
// a refresh of one MCP login by another process would; "grown" adds an MCP
// login big enough to put the document past the ceiling, with the account
// object untouched; "duplicated" is the seeded set with a second mcpOAuth
// member, holding the changed value, after the others.
func siblingSet(kind string) ([]itemMember, error) {
	if kind == "duplicated" {
		seeded, err := siblingSet("seeded")
		if err != nil {
			return nil, err
		}
		changed, err := siblingSet("changed")
		if err != nil {
			return nil, err
		}
		return append(seeded, changed[0]), nil
	}
	betaToken := "mcp-beta-" + siblingPad(600)
	if kind == "changed" {
		betaToken = "mcp-beta-rotated-" + siblingPad(600)
	}
	mcp := map[string]any{
		"plugin:alpha|0a1b2c3d": map[string]any{"serverName": "alpha", "serverUrl": "https://alpha.example.invalid/mcp", "accessToken": "mcp-alpha-" + siblingPad(8000), "refreshToken": "mcp-alpha-refresh", "expiresAt": 1893456000000},
		"plugin:beta|4e5f6a7b":  map[string]any{"serverName": "beta", "serverUrl": "https://beta.example.invalid/mcp", "accessToken": betaToken, "expiresAt": 1893456000000},
	}
	switch kind {
	case "seeded", "changed":
	case "grown":
		mcp["plugin:gamma|8c9d0e1f"] = map[string]any{"serverName": "gamma", "accessToken": "mcp-gamma-" + siblingPad(KeychainBlobCeiling)}
	default:
		return nil, fmt.Errorf("unknown sibling set %q", kind)
	}
	values := []struct {
		name  string
		value any
	}{
		{"mcpOAuth", mcp},
		{"mcpOAuthClientConfig", map[string]any{"plugin:alpha|0a1b2c3d": map[string]any{"clientId": "client-alpha", "clientSecret": "client-secret-" + siblingPad(1500)}}},
		{"pluginSecrets", map[string]any{"plugin-one": map[string]any{"apiKey": SiblingCanary + "-" + siblingPad(800)}}},
	}
	members := make([]itemMember, 0, len(values))
	for _, v := range values {
		value, err := compactJSON(v.value)
		if err != nil {
			return nil, err
		}
		members = append(members, itemMember{Name: v.name, Value: value})
	}
	return members, nil
}

// paddedSiblings returns one member, pluginSecrets, sized so that a document
// holding an account object of oauthLen bytes and this member is exactly
// total bytes long.
func paddedSiblings(oauthLen, total int) ([]itemMember, error) {
	const overhead = len(`{"claudeAiOauth":,"pluginSecrets":{"padding":""}}`)
	value, err := compactJSON(map[string]any{"padding": siblingPad(total - oauthLen - overhead)})
	if err != nil {
		return nil, err
	}
	return []itemMember{{Name: "pluginSecrets", Value: value}}, nil
}

// compactJSON marshals value as compact JSON with its object names in a
// fixed order, the way JSON.stringify writes a value it built itself.
func compactJSON(value any) (jsontext.Value, error) {
	body, err := json.Marshal(value, json.Deterministic(true))
	return jsontext.Value(body), err
}

// keychainDocument returns the item bytes Claude Code would store: the
// account object first, then the siblings in order, compact.
func keychainDocument(oauth jsontext.Value, siblings []itemMember) []byte {
	var out bytes.Buffer
	out.WriteString(`{"claudeAiOauth":`)
	out.Write(oauth)
	for _, member := range siblings {
		out.WriteByte(',')
		name, _ := jsontext.AppendQuote(nil, member.Name)
		out.Write(name)
		out.WriteByte(':')
		out.Write(member.Value)
	}
	out.WriteByte('}')
	return out.Bytes()
}

// itemMembers returns every top-level member of a keychain document in
// order, each value in the form the production encoder emits: compact, with
// duplicate names and invalid UTF-8 tolerated. Two documents preserve a
// member exactly when these forms are equal.
func itemMembers(document []byte) ([]itemMember, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(document), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, errors.New("the keychain document is not a JSON object")
	}
	var members []itemMember
	for dec.PeekKind() != '}' {
		token, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		// A token is valid only until the next read.
		name := token.String()
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		enc := jsontext.NewEncoder(&out, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
		if err := enc.WriteValue(raw); err != nil {
			return nil, err
		}
		members = append(members, itemMember{Name: name, Value: jsontext.Value(bytes.TrimSuffix(out.Bytes(), []byte("\n")))})
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the keychain document has trailing data")
	}
	return members, nil
}

// canonicalMembers returns members with every value re-emitted in the
// production encoder's form, so seeded and read members compare equal
// exactly when the encoder would treat them as the same bytes.
func canonicalMembers(members []itemMember) ([]itemMember, error) {
	canonical, err := itemMembers(keychainDocument(jsontext.Value(`null`), members))
	if err != nil {
		return nil, err
	}
	return canonical[1:], nil
}

// readCount reads the decimal number one of the fake's pid or heartbeat
// files holds. It reports false while the file is absent or mid-write.
func readCount(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return n, true
}
