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

// Package errs defines the failure vocabulary of the agentctl process and
// the mapping from a failure onto the process exit status.
//
// Every command funnels its failures into one of the error types below, and
// main turns the resulting value into a process exit status through
// [ExitCode]. The contract:
//
//   - [ExitOK] — the run produced a complete, healthy result.
//   - [ExitFatal] — the run produced nothing useful.
//   - [ExitPartial] — the run produced output, but at least one shown row
//     failed, was refused, was locked, was busy, or could only be read stale.
//
// The partial/fatal split matters to callers that script agentctl: an exit
// status of 2 still carries a usable table on stdout, so a wrapper can render
// it and flag the degraded rows, whereas 1 means there is nothing to render.
//
// A credential-swap refusal carries its own exit code at or above 10, kept
// out of the 0–2 block so a script can tell "the swap was declined for this
// specific recorded cause" from "the run was degraded" and from a
// flag-parsing failure, which also exits 2.
package errs

import (
	"errors"
	"fmt"
	"time"
)

// ExitOK is the process exit status for a complete, healthy run.
const ExitOK = 0

// ExitFatal is the process exit status for a run that failed outright and
// produced nothing useful.
const ExitFatal = 1

// ExitPartial is the process exit status for a run that produced output with
// at least one shown row degraded.
const ExitPartial = 2

// KeychainClass names how a read through security(1) failed.
//
// The named constants cover the classes the data flow branches on; any other
// classified failure is carried verbatim as its own value, so widening the
// classifier does not change this type.
type KeychainClass string

const (
	// KeychainLocked means the keychain exists but is locked; the user must
	// unlock it.
	KeychainLocked KeychainClass = "locked"
	// KeychainUnavailable means no keychain is reachable at all.
	KeychainUnavailable KeychainClass = "unavailable"
	// KeychainTimeout means the security(1) child exceeded its time budget
	// and was killed.
	KeychainTimeout KeychainClass = "timeout"
	// KeychainNotFound means the keychain is readable and holds no item
	// under that service name.
	KeychainNotFound KeychainClass = "not found"
)

// ConfigError reports a request that cannot be carried out as configured.
// Fatal.
type ConfigError struct {
	// Message states what is wrong, in the user's terms. It must never quote
	// a configuration value, because values can hold secrets.
	Message string
}

// NewConfig returns a [ConfigError] carrying message.
func NewConfig(message string) error {
	return &ConfigError{Message: message}
}

// NewNotImplemented returns the [ConfigError] for a subcommand that is
// accepted by the parser but not built yet, so the command exits [ExitFatal]
// rather than pretending to have run.
func NewNotImplemented(command string) error {
	return &ConfigError{Message: fmt.Sprintf("`%s` is not implemented yet", command)}
}

// Error returns the configuration failure in the user's terms.
func (e *ConfigError) Error() string {
	return e.Message
}

// IOError reports an operating-system call the run depends on failing.
// Fatal.
type IOError struct {
	// Context states what the process was trying to do, in the user's terms.
	Context string
	// Err is the underlying operating-system error.
	Err error
}

// NewIO returns an [IOError] carrying context and the underlying cause.
func NewIO(context string, err error) error {
	return &IOError{Context: context, Err: err}
}

// Error returns the context only; the underlying cause stays reachable
// through [IOError.Unwrap] so the user-facing line never duplicates the
// operating system's wording.
func (e *IOError) Error() string {
	return e.Context
}

// Unwrap returns the underlying operating-system error.
func (e *IOError) Unwrap() error {
	return e.Err
}

// KeychainError reports a keychain-backed credential that could not be read.
type KeychainError struct {
	// Class is the classified failure.
	Class KeychainClass
}

// NewKeychain returns a [KeychainError] for the classified failure.
func NewKeychain(class KeychainClass) error {
	return &KeychainError{Class: class}
}

// Error names the classified keychain failure.
func (e *KeychainError) Error() string {
	return "keychain read failed: " + string(e.Class)
}

// HTTPError reports an HTTP request that returned a status the run cannot
// use.
type HTTPError struct {
	// Status is the HTTP status code as received.
	Status int
	// RetryAfter is the server's retry-after hint. It is meaningful only
	// when HasRetryAfter is true, so a hint of zero seconds is distinct from
	// no hint at all.
	RetryAfter time.Duration
	// HasRetryAfter reports whether the server sent a retry-after hint. The
	// message claims a retry window only when it did.
	HasRetryAfter bool
}

// NewHTTP returns an [HTTPError] for a response that carried no retry-after
// hint.
func NewHTTP(status int) error {
	return &HTTPError{Status: status}
}

// NewHTTPRetryAfter returns an [HTTPError] for a response that carried a
// retry-after hint.
func NewHTTPRetryAfter(status int, retryAfter time.Duration) error {
	return &HTTPError{Status: status, RetryAfter: retryAfter, HasRetryAfter: true}
}

// Error returns the received status, with the retry window appended only
// when the server sent one.
func (e *HTTPError) Error() string {
	if e.HasRetryAfter {
		return fmt.Sprintf("HTTP %d (retry in %ds)", e.Status, int64(e.RetryAfter/time.Second))
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// AuthError reports an OAuth grant that was rejected.
type AuthError struct {
	// InvalidGrant reports whether the server answered invalid_grant, which
	// means the stored refresh chain is dead and only a fresh login can
	// recover it.
	InvalidGrant bool
}

// NewAuth returns an [AuthError]; invalidGrant records whether the server
// answered invalid_grant.
func NewAuth(invalidGrant bool) error {
	return &AuthError{InvalidGrant: invalidGrant}
}

// Error points at the recovery when the refresh chain is dead, and claims
// nothing more than "authentication failed" otherwise.
func (e *AuthError) Error() string {
	if e.InvalidGrant {
		return "the stored refresh token was rejected (invalid_grant); run `agentctl claude login`"
	}
	return "authentication failed"
}

// RefusedError reports that agentctl declined to act, to avoid disturbing
// another holder or losing data it cannot restore.
//
// A refusal carries a letter or a reason, never both: the lettered refusals
// are the canonical recorded causes, and every unlettered one carries a
// stable reason token instead, so a consumer can branch on the pair rather
// than on a list of special cases.
type RefusedError struct {
	// Code is the process exit code this refusal carries, at or above 10. A
	// zero Code is the generic refusal, which exits [ExitPartial].
	Code int
	// Letter is the refusal's canonical letter, or empty for an unlettered
	// refusal.
	Letter string
	// Reason says why the action was declined, in the user's terms, or is
	// empty for a lettered refusal.
	Reason string
}

// NewRefused returns a [RefusedError] carrying a reason and no letter; a
// zero code means the generic refusal, which exits [ExitPartial].
func NewRefused(code int, reason string) error {
	return &RefusedError{Code: code, Reason: reason}
}

// NewRefusedLetter returns a [RefusedError] carrying a canonical letter and
// no reason.
func NewRefusedLetter(code int, letter string) error {
	return &RefusedError{Code: code, Letter: letter}
}

// Error names why the action was declined: the reason when the refusal
// carries one, the canonical letter otherwise.
func (e *RefusedError) Error() string {
	if e.Reason != "" {
		return "refused: " + e.Reason
	}
	return "refused: refusal " + e.Letter
}

// PartialError reports a pass that rendered, but with some shown rows that
// could not be completed.
type PartialError struct {
	// Failed is how many shown rows are degraded.
	Failed int
}

// NewPartial returns a [PartialError] counting the degraded shown rows.
func NewPartial(failed int) error {
	return &PartialError{Failed: failed}
}

// PartialFromShown aggregates the shown rows that failed into a
// [PartialError], or returns nil when every shown row succeeded, so a pass
// can return its exit decision in one expression. Hidden rows never count:
// the exit status describes what the user was shown.
func PartialFromShown(failed int) error {
	if failed > 0 {
		return &PartialError{Failed: failed}
	}
	return nil
}

// Error reports how many shown rows are degraded.
func (e *PartialError) Error() string {
	return fmt.Sprintf("%d shown account(s) could not be read", e.Failed)
}

// ExitCode maps err onto the process exit status.
//
// A nil err is the healthy run and maps to [ExitOK]; every error below maps
// to a non-zero status, because an error value always means the run was at
// least degraded. The error may be wrapped; the first recognised kind in the
// chain decides, with the fatal kinds checked first. An error of no
// recognised kind is fatal: an unclassified failure must never pass for a
// merely degraded run.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var (
		configErr   *ConfigError
		ioErr       *IOError
		keychainErr *KeychainError
		httpErr     *HTTPError
		authErr     *AuthError
		refusedErr  *RefusedError
		partialErr  *PartialError
		childExit   *ChildExit
	)
	switch {
	case errors.As(err, &childExit):
		return childExit.Code
	case errors.As(err, &configErr), errors.As(err, &ioErr):
		return ExitFatal
	case errors.As(err, &refusedErr):
		if refusedErr.Code > 0 {
			return refusedErr.Code
		}
		return ExitPartial
	case errors.As(err, &keychainErr), errors.As(err, &httpErr), errors.As(err, &authErr), errors.As(err, &partialErr):
		return ExitPartial
	default:
		return ExitFatal
	}
}
