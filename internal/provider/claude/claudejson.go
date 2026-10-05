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

package claude

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// OAuthAccountKey is the one top-level member a rewrite replaces.
const OAuthAccountKey = "oauthAccount"

// StaleCaches are the top-level members a rewrite deletes: each one was
// derived from the previously installed account, and every reader of the
// file treats an absent cache as "fetch again".
var StaleCaches = [5]string{
	"modelAccessCache",
	"orgModelDefaultCache",
	"cachedExtraUsageDisabledReason",
	"cachedUsageUtilization",
	"passesEligibilityCache",
}

// The reasons a document is refused. Each one names a structural fact about
// the bytes, never a value from the document.
var (
	// ErrUnparseable reports a document that is not valid JSON.
	ErrUnparseable = errors.New("it is not valid JSON")
	// ErrNotAnObject reports a document whose top level is not a JSON object.
	ErrNotAnObject = errors.New("its top level is not a JSON object")
	// ErrNotReproducible reports a document whose bytes the canonical
	// pretty-printer does not reproduce exactly, so a rewrite of it would
	// change bytes outside the members being edited.
	ErrNotReproducible = errors.New("its bytes could not be reproduced exactly")
	// ErrChanged reports a document that no longer holds the bytes a rewrite
	// was planned from.
	ErrChanged = errors.New("the document changed after the rewrite was planned")
)

// indentUnit is the canonical pretty-printer's one level of indentation.
const indentUnit = "  "

// Reproduce verifies that rendering document with the canonical
// pretty-printer gives back its bytes exactly.
//
// One comparison catches every shape the canonical form and the input could
// disagree on: a trailing newline, CRLF line endings, another indent width, a
// \uXXXX or \/ escape the printer would not write, a number spelled another
// way, a duplicate member name. A lone surrogate, invalid UTF-8 or a byte
// order mark does not parse at all.
func Reproduce(document []byte) error {
	canon, _, err := canonicalize(document)
	if err != nil {
		return err
	}
	if !bytes.Equal(canon, document) {
		return ErrNotReproducible
	}
	return nil
}

// Rewrite is a planned edit of one configuration document, computed entirely
// in memory. The plan holds the exact input bytes it was built from, so a
// caller that re-reads the file later can prove nothing moved underneath it
// before writing the result.
type Rewrite struct {
	original []byte
	updated  []byte
	changed  bool
}

// Updated returns the document with the rewrite applied. When the rewrite
// changed nothing, it is the original slice itself: no reformatting, no copy.
func (r *Rewrite) Updated() []byte { return r.updated }

// Changed reports whether the rewrite altered any byte of the document.
func (r *Rewrite) Changed() bool { return r.changed }

// Apply returns the updated document, refusing with ErrChanged unless
// current is byte-identical to the document the rewrite was planned from.
// Callers re-read the file between planning and writing, and this comparison
// is what makes that re-read meaningful.
func (r *Rewrite) Apply(current []byte) ([]byte, error) {
	if !bytes.Equal(current, r.original) {
		return nil, ErrChanged
	}
	return r.updated, nil
}

// Plan computes the one value-level edit the live configuration file ever
// receives: the oauthAccount member is replaced with account — appended as
// the last member when absent — and the five stale caches are deleted, each
// with the separator that joined it to its neighbours. Every byte outside
// those members is carried over from document verbatim.
//
// document must pass Reproduce's gate, and account must be a valid JSON
// value; it is re-encoded canonically at the member's depth, whatever form it
// arrives in. Plan touches no file: the caller owns reading the document and
// writing the result.
func Plan(document []byte, account jsontext.Value) (*Rewrite, error) {
	canon, members, err := canonicalize(document)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canon, document) {
		return nil, ErrNotReproducible
	}

	value, err := canonicalValue(account, 1)
	if err != nil {
		return nil, fmt.Errorf("the replacement value: %w", err)
	}
	replacement, err := jsontext.AppendQuote(nil, OAuthAccountKey)
	if err != nil {
		return nil, fmt.Errorf("the replacement name: %w", err)
	}
	replacement = append(replacement, ": "...)
	replacement = append(replacement, value...)

	// The gate passed, so the member spans recorded against the canonical
	// rendering address the same bytes in document, and the document's shape
	// is exactly "{\n" + members joined by ",\n" + "\n}" (or "{}"): splicing
	// is a join of the kept members' original bytes.
	texts := make([][]byte, 0, len(members)+1)
	replaced, changed := false, false
	for _, m := range members {
		if stale(m.name) {
			changed = true
			continue
		}
		if m.name == OAuthAccountKey {
			text := document[m.start:m.end]
			if !bytes.Equal(text, replacement) {
				changed = true
			}
			texts = append(texts, replacement)
			replaced = true
			continue
		}
		texts = append(texts, document[m.start:m.end])
	}
	if !replaced {
		texts = append(texts, replacement)
		changed = true
	}
	if !changed {
		return &Rewrite{original: document, updated: document, changed: false}, nil
	}

	size := len("{\n\n}") + len(indentUnit)*len(texts)
	for _, text := range texts {
		size += len(text) + len(",\n")
	}
	updated := make([]byte, 0, size)
	updated = append(updated, '{')
	for i, text := range texts {
		if i == 0 {
			updated = append(updated, '\n')
		} else {
			updated = append(updated, ",\n"...)
		}
		updated = append(updated, indentUnit...)
		updated = append(updated, text...)
	}
	updated = append(updated, "\n}"...)
	return &Rewrite{original: document, updated: updated, changed: true}, nil
}

// stale reports whether name is one of the five cache members a rewrite
// deletes.
func stale(name string) bool {
	for _, cache := range StaleCaches {
		if name == cache {
			return true
		}
	}
	return false
}

// member is one top-level member's place in the canonical rendering: the
// span runs from the opening quote of its name through the last byte of its
// value, excluding the indentation before it and the separator after it.
type member struct {
	name       string
	start, end int
}

// canonicalize renders document in the canonical pretty form and records
// where each top-level member landed in that rendering. When the rendering
// reproduces the input exactly, those spans address the same bytes in the
// input, which is what makes verbatim splicing possible.
func canonicalize(document []byte) ([]byte, []member, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(document))
	if dec.PeekKind() != '{' {
		// Anything else must still parse completely before it can be called
		// "not an object": a truncated array is unparseable, not merely the
		// wrong shape.
		if _, err := dec.ReadValue(); err != nil {
			return nil, nil, classify(err)
		}
		if err := expectEnd(dec); err != nil {
			return nil, nil, err
		}
		return nil, nil, ErrNotAnObject
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, nil, classify(err)
	}

	buf := make([]byte, 0, len(document)+len(document)/4)
	buf = append(buf, '{')
	var members []member
	for {
		if dec.PeekKind() == '}' {
			if _, err := dec.ReadToken(); err != nil {
				return nil, nil, classify(err)
			}
			if len(members) == 0 {
				buf = append(buf, '}')
			} else {
				buf = append(buf, "\n}"...)
			}
			break
		}
		if len(members) == 0 {
			buf = append(buf, '\n')
		} else {
			buf = append(buf, ",\n"...)
		}
		buf = append(buf, indentUnit...)
		token, err := dec.ReadToken()
		if err != nil {
			return nil, nil, classify(err)
		}
		// The token is only valid until the next read, so the decoded name
		// is taken from it here.
		name := token.String()
		start := len(buf)
		if buf, err = appendQuoted(buf, name); err != nil {
			return nil, nil, err
		}
		buf = append(buf, ": "...)
		if buf, err = appendValue(buf, dec, 1); err != nil {
			return nil, nil, classify(err)
		}
		members = append(members, member{name: name, start: start, end: len(buf)})
	}
	if err := expectEnd(dec); err != nil {
		return nil, nil, err
	}
	return buf, members, nil
}

// canonicalValue renders one JSON value in the canonical pretty form at the
// given indentation depth, refusing anything that is not exactly one value.
func canonicalValue(value jsontext.Value, depth int) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(value))
	out, err := appendValue(nil, dec, depth)
	if err != nil {
		return nil, classify(err)
	}
	if err := expectEnd(dec); err != nil {
		return nil, err
	}
	return out, nil
}

// expectEnd verifies the decoder has consumed its whole input, with nothing
// but whitespace left.
func expectEnd(dec *jsontext.Decoder) error {
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return classifyTrailing(err)
	}
	return nil
}

// classify maps a decoding failure onto this package's refusal reasons. A
// duplicate member name parses in the lenient sense but can never reproduce
// its bytes, since the canonical form keeps one member of that name; every
// other failure means the bytes are not a JSON document at all. The
// decoder's own error is deliberately dropped: its message quotes document
// content, and this file's errors must never carry a value from a
// credential-bearing document.
func classify(err error) error {
	if errors.Is(err, jsontext.ErrDuplicateName) {
		return ErrNotReproducible
	}
	return ErrUnparseable
}

// classifyTrailing reports input left over after the document's one value,
// whether it failed to parse or parsed as a second value.
func classifyTrailing(err error) error {
	if err == nil {
		return ErrUnparseable
	}
	return classify(err)
}

// appendValue renders the decoder's next value canonically at depth.
func appendValue(dst []byte, dec *jsontext.Decoder, depth int) ([]byte, error) {
	switch dec.PeekKind() {
	case '{':
		return appendObject(dst, dec, depth)
	case '[':
		return appendArray(dst, dec, depth)
	default:
		raw, err := dec.ReadValue()
		if err != nil {
			return dst, err
		}
		return appendScalar(dst, raw)
	}
}

// appendObject renders an object: "{}" when empty, otherwise one member per
// line, indented one level past depth, with ": " between name and value.
func appendObject(dst []byte, dec *jsontext.Decoder, depth int) ([]byte, error) {
	if _, err := dec.ReadToken(); err != nil {
		return dst, err
	}
	dst = append(dst, '{')
	for count := 0; ; count++ {
		if dec.PeekKind() == '}' {
			if _, err := dec.ReadToken(); err != nil {
				return dst, err
			}
			if count == 0 {
				return append(dst, '}'), nil
			}
			dst = append(dst, '\n')
			dst = appendIndent(dst, depth)
			return append(dst, '}'), nil
		}
		if count == 0 {
			dst = append(dst, '\n')
		} else {
			dst = append(dst, ",\n"...)
		}
		dst = appendIndent(dst, depth+1)
		name, err := dec.ReadToken()
		if err != nil {
			return dst, err
		}
		if dst, err = appendQuoted(dst, name.String()); err != nil {
			return dst, err
		}
		dst = append(dst, ": "...)
		if dst, err = appendValue(dst, dec, depth+1); err != nil {
			return dst, err
		}
	}
}

// appendArray renders an array: "[]" when empty, otherwise one element per
// line, indented one level past depth.
func appendArray(dst []byte, dec *jsontext.Decoder, depth int) ([]byte, error) {
	if _, err := dec.ReadToken(); err != nil {
		return dst, err
	}
	dst = append(dst, '[')
	for count := 0; ; count++ {
		if dec.PeekKind() == ']' {
			if _, err := dec.ReadToken(); err != nil {
				return dst, err
			}
			if count == 0 {
				return append(dst, ']'), nil
			}
			dst = append(dst, '\n')
			dst = appendIndent(dst, depth)
			return append(dst, ']'), nil
		}
		if count == 0 {
			dst = append(dst, '\n')
		} else {
			dst = append(dst, ",\n"...)
		}
		dst = appendIndent(dst, depth+1)
		var err error
		if dst, err = appendValue(dst, dec, depth+1); err != nil {
			return dst, err
		}
	}
}

// appendScalar renders one string, number, boolean or null from its raw
// bytes.
func appendScalar(dst []byte, raw jsontext.Value) ([]byte, error) {
	switch raw[0] {
	case '"':
		decoded, err := jsontext.AppendUnquote(nil, raw)
		if err != nil {
			return dst, err
		}
		return appendQuoted(dst, string(decoded))
	case 't', 'f', 'n':
		return append(dst, raw...), nil
	default:
		return appendNumber(dst, raw)
	}
}

// appendQuoted renders a string with the canonical escaping: the two-letter
// escapes \b \t \n \f \r, \u00xx in lower case for the other control
// characters, \" and \\, and every other character raw.
func appendQuoted(dst []byte, s string) ([]byte, error) {
	out, err := jsontext.AppendQuote(dst, s)
	if err != nil {
		return dst, ErrUnparseable
	}
	return out, nil
}

// appendIndent appends depth levels of indentation.
func appendIndent(dst []byte, depth int) []byte {
	for range depth {
		dst = append(dst, indentUnit...)
	}
	return dst
}

// appendNumber renders a number canonically. A token spelled as an integer
// keeps its own digits while it fits the native integer ranges; everything
// else goes through the double it parses to, in JavaScript's shortest
// spelling. A magnitude no double can hold is refused the way the parser
// refuses it.
func appendNumber(dst []byte, raw jsontext.Value) ([]byte, error) {
	if !bytes.ContainsAny(raw, ".eE") {
		text := string(raw)
		if raw[0] == '-' {
			// "-0" is the one integer spelling that canonicalizes as the
			// double -0.0 rather than as its own digits.
			if text != "-0" {
				if _, err := strconv.ParseInt(text, 10, 64); err == nil {
					return append(dst, raw...), nil
				}
			}
		} else if _, err := strconv.ParseUint(text, 10, 64); err == nil {
			return append(dst, raw...), nil
		}
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil && math.IsInf(f, 0) {
		return dst, ErrUnparseable
	}
	return appendDouble(dst, f), nil
}

// appendDouble renders a double in its shortest round-trip spelling: plain
// decimal while the leading digit sits between the 10^-5 and 10^15 places,
// with an integral value ending in ".0"; scientific otherwise, as
// "d.ddde±x" with an explicit sign and no zero-padding on the exponent.
func appendDouble(dst []byte, f float64) []byte {
	if f == 0 {
		if math.Signbit(f) {
			return append(dst, "-0.0"...)
		}
		return append(dst, "0.0"...)
	}
	if math.Signbit(f) {
		dst = append(dst, '-')
		f = -f
	}
	var scratch [32]byte
	shortest := strconv.AppendFloat(scratch[:0], f, 'e', -1, 64)
	mantissa, expText, _ := bytes.Cut(shortest, []byte("e"))
	exponent, err := strconv.Atoi(string(expText))
	if err != nil {
		// AppendFloat emits "e" with a parseable exponent for every finite
		// double; a failure here is unreachable.
		panic(err)
	}
	digits := mantissa
	if len(mantissa) > 1 {
		digits = append(mantissa[:1], mantissa[2:]...)
	}

	// point is where the decimal point falls in the digit run: the value is
	// 0.<digits> × 10^point.
	point := exponent + 1
	if point > -5 && point <= 16 {
		switch {
		case point <= 0:
			dst = append(dst, "0."...)
			for range -point {
				dst = append(dst, '0')
			}
			return append(dst, digits...)
		case point >= len(digits):
			dst = append(dst, digits...)
			for range point - len(digits) {
				dst = append(dst, '0')
			}
			return append(dst, ".0"...)
		default:
			dst = append(dst, digits[:point]...)
			dst = append(dst, '.')
			return append(dst, digits[point:]...)
		}
	}
	dst = append(dst, digits[:1]...)
	if len(digits) > 1 {
		dst = append(dst, '.')
		dst = append(dst, digits[1:]...)
	}
	dst = append(dst, 'e')
	if exponent >= 0 {
		dst = append(dst, '+')
	}
	return strconv.AppendInt(dst, int64(exponent), 10)
}
