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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// SecurityWrite runs the fake security the way the write transport does:
// argv -i, with one command line on standard input.
func (f *Fixture) SecurityWrite(line string) Output {
	f.tb.Helper()
	cmd := exec.CommandContext(testContext(f.tb), f.SecurityBin(), "-i")
	f.Apply(cmd)
	cmd.Stdin = strings.NewReader(line)
	stdout, stderr := Capture(cmd)
	if err := cmd.Start(); err != nil {
		f.tb.Fatalf("start the stand-in: %v", err)
	}
	return Finish(f.tb, cmd, stdout, stderr)
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
