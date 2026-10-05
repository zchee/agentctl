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
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/secret"
)

// AuditOutcome is a fixed word describing one credential operation.
type AuditOutcome string

const (
	// AuditApplied records a renamed refresh.
	AuditApplied AuditOutcome = "applied"
	// AuditSavedToPending records a parked refresh.
	AuditSavedToPending AuditOutcome = "saved_to_pending"
	// AuditDiscardedExternal records a credential changed by another writer.
	AuditDiscardedExternal AuditOutcome = "discarded_external"
	// AuditPendingReplayed records a parked credential replay.
	AuditPendingReplayed AuditOutcome = "pending_replayed"
	// AuditPendingDiscarded records an unused parked credential.
	AuditPendingDiscarded AuditOutcome = "pending_discarded"
	// AuditLoginInstall records a new login.
	AuditLoginInstall AuditOutcome = "login_install"
	// AuditLoginOverwrite records a replaced login.
	AuditLoginOverwrite AuditOutcome = "login_overwrite"
	// AuditDelete records a removed namespace.
	AuditDelete AuditOutcome = "delete"
	// AuditAdoptedExternal records a kept external grant.
	AuditAdoptedExternal AuditOutcome = "adopted_external"
	// AuditNeedsLogin records a dead grant.
	AuditNeedsLogin AuditOutcome = "needs_login"
	// AuditAmbiguous records an unknown refresh outcome.
	AuditAmbiguous AuditOutcome = "ambiguous"
	// AuditResend records a user's one-shot resend.
	AuditResend AuditOutcome = "resend"
	// AuditFloorReset records a user's floor reset.
	AuditFloorReset AuditOutcome = "floor_reset"
	// AuditLoginKeychainGained records a refused child's gained keychain item.
	AuditLoginKeychainGained AuditOutcome = "login_keychain_gained"
)

// CodexAuditEntry is an ordered, validated audit line with no token material.
type CodexAuditEntry struct {
	TS              time.Time    `json:"ts"`
	PID             uint32       `json:"agctl_pid"`
	Provider        string       `json:"provider"`
	UserID          string       `json:"user_id"`
	AccountID       string       `json:"account_id"`
	Outcome         AuditOutcome `json:"outcome"`
	Class           *string      `json:"class,omitzero"`
	Digest8Before   *string      `json:"digest8_before"`
	Digest8After    *string      `json:"digest8_after"`
	KeychainAccount *string      `json:"keychain_account,omitzero"`
}

// CodexEvent describes an event that changed no credential file.
type CodexEvent struct {
	Outcome         AuditOutcome
	Class           *string
	Digest8Before   *string
	Digest8After    *string
	KeychainAccount *string
}

// CodexAuditLogPath returns the Codex append-only audit path.
func CodexAuditLogPath(paths *config.Paths) string {
	return filepath.Join(paths.CodexRoot(), "writes.jsonl")
}

// AppendCodexReceipt consumes a one-use write receipt before attempting its audit append.
// A refused append does not undo the credential operation or restore the receipt.
func AppendCodexReceipt(ctx context.Context, paths *config.Paths, receipt *WriteReceipt) error {
	if receipt == nil {
		return errs.NewConfig("a Codex audit append requires a write receipt")
	}
	if err := receipt.Consume(); err != nil {
		return err
	}
	var outcome AuditOutcome
	switch receipt.Kind() {
	case WriteRefreshApplied:
		outcome = AuditApplied
	case WriteRefreshSavedToPending:
		outcome = AuditSavedToPending
	case WriteDiscardedExternal:
		outcome = AuditDiscardedExternal
	case WritePendingReplayed:
		outcome = AuditPendingReplayed
	case WritePendingDiscarded:
		outcome = AuditPendingDiscarded
	case WriteLoginInstall:
		outcome = AuditLoginInstall
		if receipt.Overwrote() {
			outcome = AuditLoginOverwrite
		}
	case WriteDelete:
		outcome = AuditDelete
	default:
		return errs.NewConfig("a Codex audit receipt has an unrecognized write kind")
	}
	user, account := receipt.IDs()
	return AppendCodexEvent(ctx, paths, user, account, CodexEvent{Outcome: outcome, Digest8Before: receipt.Digest8Before(), Digest8After: receipt.Digest8After()})
}

// AppendCodexEvent appends one checked event; an I/O refusal leaves its operation standing.
func AppendCodexEvent(ctx context.Context, paths *config.Paths, user, account string, event CodexEvent) error {
	if event.Outcome == AuditLoginKeychainGained {
		user, account = "none", "none"
	}
	entry := CodexAuditEntry{TS: time.Now().UTC(), PID: uint32(os.Getpid()), Provider: "codex", UserID: user, AccountID: account, Outcome: event.Outcome, Class: event.Class, Digest8Before: event.Digest8Before, Digest8After: event.Digest8After, KeychainAccount: event.KeychainAccount}
	return writeCodexAuditEntry(ctx, paths, entry)
}

func codexAuditLine(entry CodexAuditEntry) (string, error) {
	for _, id := range []string{entry.UserID, entry.AccountID} {
		if config.ValidateCodexSegment(id) != nil {
			return "", errs.NewConfig(fmt.Sprintf("a Codex audit entry's id is not a namespace segment (%d characters); the log holds ids only", len(id)))
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"digest8_before", entry.Digest8Before}, {"digest8_after", entry.Digest8After}} {
		if field.value != nil && !isCodexDigest8(*field.value) {
			return "", errs.NewConfig(fmt.Sprintf("a Codex audit entry's `%s` must be 8 lowercase hex digits, not %d characters", field.name, len(*field.value)))
		}
	}
	if entry.Provider != "codex" {
		return "", errs.NewConfig(fmt.Sprintf("a Codex audit entry's provider is not `codex` (%d characters); the log holds this provider's lines only", len(entry.Provider)))
	}
	if !slices.Contains([]AuditOutcome{AuditApplied, AuditSavedToPending, AuditDiscardedExternal, AuditPendingReplayed, AuditPendingDiscarded, AuditLoginInstall, AuditLoginOverwrite, AuditDelete, AuditAdoptedExternal, AuditNeedsLogin, AuditAmbiguous, AuditResend, AuditFloorReset, AuditLoginKeychainGained}, entry.Outcome) {
		return "", errs.NewConfig("a Codex audit entry's outcome is not one of the fixed words")
	}
	if entry.Outcome == AuditLoginKeychainGained {
		if entry.KeychainAccount == nil || !isCodexHomeAccount(*entry.KeychainAccount) {
			return "", errs.NewConfig("a Codex audit entry claims a gained keychain item without an account spelled `cli|` and sixteen lowercase hex digits")
		}
	} else if entry.KeychainAccount != nil {
		return "", errs.NewConfig("a Codex audit entry carries a keychain account on an outcome that has none")
	}
	if entry.Class != nil && !slices.Contains([]string{"ambiguous", "server_error", "rate_limited", "interrupted", "tls", "write_failed", "unauthorized", "invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated"}, *entry.Class) {
		return "", errs.NewConfig(fmt.Sprintf("a Codex audit entry's class is not one of the fixed words (%d characters)", len(*entry.Class)))
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return "", errs.NewConfig("a Codex audit entry could not be serialized")
	}
	if strings.ContainsRune(string(encoded), '@') {
		return "", errs.NewConfig("a Codex audit entry would carry an `@`; the log holds no email address")
	}
	return string(encoded) + "\n", nil
}

func writeCodexAuditEntry(ctx context.Context, paths *config.Paths, entry CodexAuditEntry) error {
	line, err := codexAuditLine(entry)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := secret.CreateDirUnder(paths.ConfigDir(), paths.CodexRoot())
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	file, err := secret.OpenAuditLogAt(dir, "writes.jsonl", CodexAuditLogPath(paths))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return secret.WriteAuditLine(file, CodexAuditLogPath(paths), line)
}

// ShownCodexAuditLine reserializes only lines whose fields would pass the writer's guard.
func ShownCodexAuditLine(line string) (string, bool) {
	var entry CodexAuditEntry
	if json.Unmarshal([]byte(line), &entry) != nil {
		return "", false
	}
	shown, err := codexAuditLine(entry)
	return strings.TrimSuffix(shown, "\n"), err == nil
}

// ReadCodexAudit returns the whole log, distinguishing absence from an unreadable log.
func ReadCodexAudit(ctx context.Context, paths *config.Paths) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	dir, err := secret.OpenDirUnder(paths.ConfigDir(), paths.CodexRoot())
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { _ = unix.Close(dir) }()
	return secret.ReadAuditLogAt(dir, "writes.jsonl", CodexAuditLogPath(paths))
}

// GainedCodexKeychainAccounts streams the complete history, deduplicating safely spelled gained items.
func GainedCodexKeychainAccounts(ctx context.Context, paths *config.Paths) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := secret.OpenDirUnder(paths.ConfigDir(), paths.CodexRoot())
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dir) }()
	found := []string{}
	seen := map[string]bool{}
	_, err = secret.ForEachAuditLineAt(dir, "writes.jsonl", CodexAuditLogPath(paths), 4096, func(line string) {
		var entry CodexAuditEntry
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Outcome == AuditLoginKeychainGained && entry.KeychainAccount != nil && isCodexHomeAccount(*entry.KeychainAccount) && !seen[*entry.KeychainAccount] {
			seen[*entry.KeychainAccount] = true
			found = append(found, *entry.KeychainAccount)
		}
	})
	return found, err
}

func isCodexDigest8(value string) bool { return len(value) == 8 && isLowerHex(value) }
func isLowerHex(value string) bool {
	for _, b := range []byte(value) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func isCodexHomeAccount(value string) bool {
	suffix, ok := strings.CutPrefix(value, "cli|")
	return ok && len(suffix) == 16 && isLowerHex(suffix)
}
