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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"unicode/utf8"

	toml "github.com/zchee/go-toml"
	"golang.org/x/sys/unix"
)

// StoreMode describes where a home stores credentials.
type StoreMode string

const (
	// StoreFile is the default on-disk store.
	StoreFile StoreMode = "file"
	// StoreKeyring stores credentials only in the keychain.
	StoreKeyring StoreMode = "keyring"
	// StoreAuto uses the keychain first and the file on a missing or unavailable item.
	StoreAuto StoreMode = "auto"
	// StoreEphemeral keeps credentials in a single process.
	StoreEphemeral StoreMode = "ephemeral"
)

// Label returns the sanitized configuration spelling.
func (m StoreMode) Label() string { return string(m) }

// ConfigNote records a failure without retaining a parser message or file content.
type ConfigNote struct {
	Kind string
	Line *int
}

// String renders a positional, non-secret note.
func (n ConfigNote) String() string {
	if n.Kind == "unreadable" {
		return "config.toml unreadable"
	}
	if n.Line != nil {
		return fmt.Sprintf("config.toml unparseable (line %d)", *n.Line)
	}
	return "config.toml unparseable"
}

// Config holds only the two allowlisted configuration settings.
type Config struct {
	Store   StoreMode
	BaseURL *string
	Note    *ConfigNote
}

const maxConfigBytes = 1 << 20

// LoadConfig reads a regular config file, following dotfile-manager symlinks without blocking on FIFOs.
func LoadConfig(ctx context.Context, home string) Config {
	result := Config{Store: StoreFile}
	if ctx.Err() != nil {
		result.Note = &ConfigNote{Kind: "unreadable"}
		return result
	}
	fd, err := unix.Open(filepath.Join(home, "config.toml"), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return result
	}
	if err != nil {
		result.Note = &ConfigNote{Kind: "unreadable"}
		return result
	}
	file := os.NewFile(uintptr(fd), "config.toml")
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		result.Note = &ConfigNote{Kind: "unreadable"}
		return result
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes))
	if err != nil {
		result.Note = &ConfigNote{Kind: "unreadable"}
		return result
	}
	if !utf8.Valid(data) {
		result.Note = &ConfigNote{Kind: "unparseable"}
		return result
	}
	return parseConfig(data)
}

func parseConfig(data []byte) Config {
	result := Config{Store: StoreFile}
	var wire struct {
		Store   any `toml:"cli_auth_credentials_store,omitzero"`
		BaseURL any `toml:"chatgpt_base_url,omitzero"`
	}
	err := toml.NewDecoder(bytes.NewReader(data), toml.WithCopiedStrings()).Decode(&wire)
	if err != nil {
		note := &ConfigNote{Kind: "unparseable"}
		if syntax, ok := errors.AsType[*toml.SyntaxError](err); ok && syntax.Line > 0 {
			note.Line = new(syntax.Line)
		}
		result.Note = note
		return result
	}
	if wire.Store != nil {
		value, ok := wire.Store.(string)
		if !ok {
			result.Store = "<unrecognised>"
		} else {
			result.Store = modeFrom(value)
		}
	}
	if value, ok := wire.BaseURL.(string); ok {
		result.BaseURL = plainURL(value)
	}
	return result
}

func modeFrom(value string) StoreMode {
	switch StoreMode(value) {
	case StoreFile, StoreKeyring, StoreAuto, StoreEphemeral:
		return StoreMode(value)
	}
	if len(value) > 32 {
		return "<unrecognised>"
	}
	for _, b := range []byte(value) {
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '_' && b != '-' {
			return "<unrecognised>"
		}
	}
	return StoreMode(value)
}

func plainURL(value string) *string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return new(parsed.String())
}

// KeyringProbe is the read-only listing's result for this home.
type KeyringProbe int

const (
	// KeyringUnknown means the listing could not be taken.
	KeyringUnknown KeyringProbe = iota
	// KeyringPresent means the home's keychain item was listed.
	KeyringPresent
	// KeyringAbsent means it was not listed.
	KeyringAbsent
)

// FileInEffect consults the keychain probe only when the store mode requires it.
func FileInEffect(mode StoreMode, probe func() KeyringProbe) (bool, string) {
	switch mode {
	case StoreFile:
		return true, ""
	case StoreAuto:
		if probe() == KeyringPresent {
			return false, ""
		}
		return true, "auto (file in effect)"
	default:
		return false, ""
	}
}
