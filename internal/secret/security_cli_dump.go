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
	"fmt"
	"strings"
)

// parseDump parses security(1) dump-keychain output into one entry per item.
//
// The format is a run of records, each introduced by a class: line and
// followed by an attributes: block of "key"<type>=value lines. Only four
// attributes are kept — service, account and the two timestamps — and every
// value is optional, because security prints <NULL> for anything unset.
//
// Items without a service name are dropped: they cannot be credential items,
// and carrying them would mean holding every generic-password label on the
// machine in memory for no reason. A kept attribute whose line is present
// but whose value cannot be extracted is a parse failure, reported rather
// than swallowed, so a format drift across macOS versions surfaces as a
// diagnosable error instead of a silently shorter listing. Output that holds
// no records at all — an error message, an empty dump — parses as an empty
// listing, which is a real answer, not a failure.
func parseDump(text string) ([]ServiceEntry, error) {
	var (
		entries []ServiceEntry
		current partialEntry
		broken  []string
	)

	lineno := 0
	for line := range strings.Lines(text) {
		lineno++
		trimmed := strings.TrimLeft(strings.TrimRight(line, "\r\n"), " \t")
		if strings.HasPrefix(trimmed, "class:") || strings.HasPrefix(trimmed, "keychain:") {
			current.flushInto(&entries)
			continue
		}
		for _, key := range [...]string{"svce", "acct", "cdat", "mdat"} {
			value, found, err := attribute(trimmed, key)
			if err != nil {
				broken = append(broken, fmt.Sprintf("line %d: %v", lineno, err))
			}
			if !found {
				continue
			}
			switch key {
			case "svce":
				current.service = value
			case "acct":
				current.account = value
			case "cdat":
				current.createdAt = value
			case "mdat":
				current.modifiedAt = value
			}
			break
		}
	}
	current.flushInto(&entries)

	if len(broken) > 0 {
		return entries, fmt.Errorf("dump-keychain output did not parse: %s", strings.Join(broken, "; "))
	}
	return entries, nil
}

// partialEntry is an item being assembled from consecutive attribute lines.
type partialEntry struct {
	service    string
	account    string
	createdAt  string
	modifiedAt string
}

// flushInto appends the assembled item and resets the assembly. An item
// without a service name is dropped.
func (p *partialEntry) flushInto(entries *[]ServiceEntry) {
	taken := *p
	*p = partialEntry{}
	if taken.service == "" {
		return
	}
	*entries = append(*entries, ServiceEntry{
		Service:    taken.service,
		Account:    taken.account,
		CreatedAt:  taken.createdAt,
		ModifiedAt: taken.modifiedAt,
	})
}

// attribute extracts the printable value of one attribute line.
//
// security writes a value three ways: quoted ("x"), as <NULL>, or as hex
// followed by the quoted printable form (0x6162  "ab"). Taking the text
// between the first and last quote after the equals sign handles all three,
// because the hex form always ends with the quoted rendering. The trailing
// \000 security prints inside the quoted rendering of a C string is dropped.
//
// found reports whether the line carries this key at all; err reports a line
// that carries the key but whose value could not be extracted, which is the
// drift signal parseDump surfaces.
func attribute(line, key string) (value string, found bool, err error) {
	marker := `"` + key + `"<`
	rest, ok := strings.CutPrefix(line, marker)
	if !ok {
		return "", false, nil
	}
	_, after, ok := strings.Cut(rest, "=")
	if !ok {
		return "", true, fmt.Errorf("attribute %q has no value", key)
	}
	if strings.TrimSpace(after) == "<NULL>" {
		return "", true, nil
	}
	open := strings.Index(after, `"`)
	closing := strings.LastIndex(after, `"`)
	if open < 0 || closing <= open {
		return "", true, fmt.Errorf("attribute %q has no quoted rendering", key)
	}
	return strings.TrimSuffix(after[open+1:closing], `\000`), true, nil
}
