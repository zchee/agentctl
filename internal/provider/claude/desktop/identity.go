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

package desktop

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// ConfigFile is the one file inside a profile agentctl ever opens.
const ConfigFile = "config.json"

// identityKey is the plaintext member naming the account the app last
// observed. It is last-observed identity, never proof of a live sign-in.
const identityKey = "lastKnownAccountUuid"

// MaxConfigBytes bounds the read of config.json. The file holds encrypted
// token caches beside the identity; a file larger than this is refused
// rather than read without limit.
const MaxConfigBytes = 64 << 10

// ReaderState is what the identity reader could say about a profile.
type ReaderState string

const (
	// StateFresh is a profile without config.json: the app has not yet
	// written one, so no account was ever observed in it.
	StateFresh ReaderState = "fresh"
	// StateUnreadable is a config.json that could not be read or did not
	// have the expected shape; nothing is inferred from it.
	StateUnreadable ReaderState = "unreadable"
	// StateUnknown is a config.json object without the identity member,
	// or with it set to null.
	StateUnknown ReaderState = "unknown"
	// StateUUID is a config.json naming a valid account UUID.
	StateUUID ReaderState = "uuid"
)

// Identity is the reader's answer for one profile directory.
type Identity struct {
	// State is one of the four reader states.
	State ReaderState
	// UUID is the observed account UUID in lower case, set only for
	// StateUUID.
	UUID string
	// Reason says why the state is StateUnreadable; empty otherwise.
	Reason string
}

// ReadDesktopIdentity reads the plaintext lastKnownAccountUuid of the
// profile at dir. The directory is opened as a root, so config.json cannot
// resolve outside it, and config.json is the only name opened under it.
func ReadDesktopIdentity(dir string) Identity {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Identity{State: StateUnreadable, Reason: fmt.Sprintf("opening the profile: %v", err)}
	}
	defer func() { _ = root.Close() }()
	return ReadIdentityFS(root.FS())
}

// ReadIdentityFS is ReadDesktopIdentity over any file system whose root is
// the profile directory. It opens exactly one name, config.json.
func ReadIdentityFS(fsys fs.FS) Identity {
	file, err := fsys.Open(ConfigFile)
	if errors.Is(err, fs.ErrNotExist) {
		return Identity{State: StateFresh}
	}
	if err != nil {
		return Identity{State: StateUnreadable, Reason: fmt.Sprintf("opening %s: %v", ConfigFile, err)}
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Identity{State: StateUnreadable, Reason: fmt.Sprintf("checking %s: %v", ConfigFile, err)}
	}
	if !info.Mode().IsRegular() {
		return Identity{State: StateUnreadable, Reason: ConfigFile + " is not a regular file"}
	}
	if info.Size() > MaxConfigBytes {
		return Identity{State: StateUnreadable, Reason: fmt.Sprintf("%s is larger than %d bytes", ConfigFile, MaxConfigBytes)}
	}
	// One byte past the bound tells a file that grew after the check from
	// one that fits exactly.
	data, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil {
		return Identity{State: StateUnreadable, Reason: fmt.Sprintf("reading %s: %v", ConfigFile, err)}
	}
	if len(data) > MaxConfigBytes {
		return Identity{State: StateUnreadable, Reason: fmt.Sprintf("%s is larger than %d bytes", ConfigFile, MaxConfigBytes)}
	}
	return parseIdentity(data)
}

// parseIdentity walks the top-level object token by token and keeps only
// the identity member. Every other member's value, the encrypted caches
// included, is skipped without being decoded into a string.
func parseIdentity(data []byte) Identity {
	unreadable := func(reason string) Identity { return Identity{State: StateUnreadable, Reason: reason} }
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	tok, err := dec.ReadToken()
	if err != nil {
		return unreadable(fmt.Sprintf("%s is not JSON: %v", ConfigFile, err))
	}
	if tok.Kind() != '{' {
		return unreadable(ConfigFile + " is not a JSON object")
	}
	var raw jsontext.Value
	found := false
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return unreadable(fmt.Sprintf("%s is not JSON: %v", ConfigFile, err))
		}
		if name.String() != identityKey {
			if err := dec.SkipValue(); err != nil {
				return unreadable(fmt.Sprintf("%s is not JSON: %v", ConfigFile, err))
			}
			continue
		}
		value, err := dec.ReadValue()
		if err != nil {
			return unreadable(fmt.Sprintf("%s is not JSON: %v", ConfigFile, err))
		}
		raw, found = value.Clone(), true
	}
	if _, err := dec.ReadToken(); err != nil {
		return unreadable(fmt.Sprintf("%s is not JSON: %v", ConfigFile, err))
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return unreadable(ConfigFile + " has content after its top-level object")
	}
	if !found {
		return Identity{State: StateUnknown}
	}
	switch raw.Kind() {
	case 'n':
		return Identity{State: StateUnknown}
	case '"':
		var s string
		dec := jsontext.NewDecoder(bytes.NewReader(raw))
		tok, err := dec.ReadToken()
		if err != nil {
			return unreadable(fmt.Sprintf("%s: %v", identityKey, err))
		}
		s = tok.String()
		if !ValidUUID(s) {
			return unreadable(identityKey + " is not a valid UUID")
		}
		return Identity{State: StateUUID, UUID: strings.ToLower(s)}
	default:
		return unreadable(identityKey + " is not a string")
	}
}

// Word is the identity word a target profile reads as, relative to the
// account the caller asked for.
type Word string

const (
	// WordMatches is a profile that last observed the requested account.
	WordMatches Word = "matches"
	// WordMismatch is a profile that last observed another account,
	// registered with agentctl or not.
	WordMismatch Word = "mismatch"
	// WordFresh is a profile that never observed an account.
	WordFresh Word = "fresh"
	// WordUnknown is a profile that names no account.
	WordUnknown Word = "unknown"
	// WordUnreadable is a profile whose config.json could not be read.
	WordUnreadable Word = "unreadable"
)

// WordFor derives the identity word of id for the requested account UUID.
func (id Identity) WordFor(requested string) Word {
	switch id.State {
	case StateUUID:
		if strings.EqualFold(id.UUID, requested) {
			return WordMatches
		}
		return WordMismatch
	case StateFresh:
		return WordFresh
	case StateUnknown:
		return WordUnknown
	default:
		return WordUnreadable
	}
}
