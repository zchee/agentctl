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

package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// durationValue adapts the package's duration grammar to a flag value, so
// every interval and timeout flag parses through the same checked parser
// instead of the standard library's multi-unit grammar.
type durationValue struct {
	target *time.Duration
	parse  func(string) (time.Duration, error)
}

// newDurationValue binds a duration flag to target, parsing with parse.
func newDurationValue(target *time.Duration, parse func(string) (time.Duration, error)) *durationValue {
	return &durationValue{target: target, parse: parse}
}

// String renders the current value in the grammar the flag accepts, whole
// seconds with an `s` suffix, so the help text shows a default the user
// can paste back.
func (v *durationValue) String() string {
	return strconv.FormatInt(int64(*v.target/time.Second), 10) + "s"
}

// Set parses and stores one occurrence of the flag.
func (v *durationValue) Set(value string) error {
	d, err := v.parse(value)
	if err != nil {
		return err
	}
	*v.target = d
	return nil
}

// Type names the value in the flag's usage line.
func (v *durationValue) Type() string {
	return "DUR"
}

// enumValue restricts a string-kinded flag to a fixed vocabulary and
// rejects anything else naming the accepted values.
type enumValue[T ~string] struct {
	target  *T
	allowed []string
}

// newEnumValue binds an enum flag to target, accepting only allowed.
func newEnumValue[T ~string](target *T, allowed ...string) *enumValue[T] {
	return &enumValue[T]{target: target, allowed: allowed}
}

// String renders the current value.
func (v *enumValue[T]) String() string {
	return string(*v.target)
}

// Set stores one occurrence of the flag after checking the vocabulary.
func (v *enumValue[T]) Set(value string) error {
	if !slices.Contains(v.allowed, value) {
		return fmt.Errorf("must be one of %s", strings.Join(v.allowed, ", "))
	}
	*v.target = T(value)
	return nil
}

// Type names the accepted vocabulary in the flag's usage line.
func (v *enumValue[T]) Type() string {
	return strings.Join(v.allowed, "|")
}
