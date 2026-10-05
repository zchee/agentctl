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
	"errors"
	"io/fs"

	"github.com/zchee/agentctl/internal/errs"
)

// LocationKind picks the one store a credential may be read from.
type LocationKind int

const (
	// LocationKeychain is a macOS keychain item, named by its service.
	LocationKeychain LocationKind = iota + 1
	// LocationFile is a credential file this program's store owns.
	LocationFile
)

// Location names where exactly one account's credential lives.
//
// A location is one store, never a chain of them. The vendor tooling this
// program coexists with composes its stores as "keychain, and if that does
// not work, the file", which is right for a tool that must start a session
// somehow. This program does not: the two stores can hold different
// accounts' credentials, so falling through would show one account's usage
// under another's row and — worse — decide a namespace was free to write
// while a session owned it. [Location.Classify] therefore has no outcome
// that names another store.
type Location struct {
	// Kind picks the store. The zero value names no store at all, and every
	// read against it classifies as transient, because failing closed beats
	// inventing a place to read from.
	Kind LocationKind
	// Service is the keychain service name; meaningful only when Kind is
	// [LocationKeychain].
	Service string
	// Path is the credential file path; meaningful only when Kind is
	// [LocationFile].
	Path string
}

// KeychainItem returns the location of one keychain item.
func KeychainItem(service string) Location {
	return Location{Kind: LocationKeychain, Service: service}
}

// CredentialFile returns the location of one credential file.
func CredentialFile(path string) Location {
	return Location{Kind: LocationFile, Path: path}
}

// OutcomeKind is what resolving one credential location produced. The set is
// closed on purpose: there is no kind that says "try the other store".
type OutcomeKind int

const (
	// OutcomeCredential means the read produced the credential.
	OutcomeCredential OutcomeKind = iota + 1
	// OutcomeAbsent means the store answered "nothing here" — for an owned
	// account, "needs login".
	OutcomeAbsent
	// OutcomeLocked means the keychain is locked. Split out from transient
	// because the recovery is "unlock it", not "retry".
	OutcomeLocked
	// OutcomeTransient means the read failed in a way a later pass might
	// not. Never a reason to consult another store.
	OutcomeTransient
)

// Outcome is one read attempt classified for the caller's data flow.
type Outcome struct {
	// Kind says what the attempt produced.
	Kind OutcomeKind
	// Reason carries the transient failure, phrased for a status row. It is
	// set only for [OutcomeTransient].
	Reason string
}

// Classify maps the error of one read against this location onto the
// outcome the caller branches on. It is pure: it inspects only its
// arguments, and it never performs or suggests a read of any other store.
//
// The rule, by store:
//
//	| store    | absent means            | locked | anything else |
//	|----------|-------------------------|--------|---------------|
//	| keychain | no item (ErrItemNotFound) | locked | transient   |
//	| file     | no file (fs.ErrNotExist)  | —      | transient   |
//
// A keychain failure classifying as transient — a timeout, an unreachable
// keychain, a spawn failure — leaves the credential file unconsulted: a
// keychain that is merely slow still holds the authoritative credentials,
// and reading the file instead is how two writers of one refresh chain
// happen.
func (l Location) Classify(err error) Outcome {
	if err == nil {
		return Outcome{Kind: OutcomeCredential}
	}
	switch l.Kind {
	case LocationKeychain:
		if kerr, ok := errors.AsType[*errs.KeychainError](err); ok {
			switch kerr.Class {
			case errs.KeychainNotFound:
				return Outcome{Kind: OutcomeAbsent}
			case errs.KeychainLocked:
				return Outcome{Kind: OutcomeLocked}
			}
		}
	case LocationFile:
		if errors.Is(err, fs.ErrNotExist) {
			return Outcome{Kind: OutcomeAbsent}
		}
	}
	return Outcome{Kind: OutcomeTransient, Reason: err.Error()}
}
