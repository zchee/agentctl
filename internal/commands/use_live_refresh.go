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

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// useGuardedWriteBack runs under the namespace lock, not Claude's peer locks.
func useGuardedWriteBack(ctx context.Context, paths *config.Paths, record *config.AccountRecord, refreshed *claude.Credentials, derivedFrom claude.Digests) error {
	nsDir := paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
	status := Status{Reader: secret.NewReader()}
	activity := useDetectUnlisted(ctx, &status, nsDir, record)
	dir, err := secret.OpenNamespaceDir(paths, nsDir)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	spec := secret.PendingSpec{TargetName: secret.CredentialsFile, PendingName: secret.PendingFile, MetaName: secret.PendingMetaFile}
	decision, _, err := secret.ResolvePendingWith(dir, nsDir, &spec, activity.Kind != secret.ForeignNone, refreshPendingCredential{})
	if err != nil {
		return fmt.Errorf("the pending write could not be resolved: %w", err)
	}
	switch activity.Kind {
	case secret.ForeignClaudeLock:
		return fmt.Errorf("a Claude Code session holds `%s` there (%d ms old), and the namespace lock does not exclude it", activity.LockName, activity.LockAgeMS)
	case secret.ForeignMigratedToKeychain:
		return fmt.Errorf("that namespace has migrated into the keychain item `%s`", activity.Service)
	}
	current := rereadCredential(nsDir)
	if current == nil {
		return errors.New("the credential this refresh was derived from is gone")
	}
	digest, err := current.Digests()
	if err != nil {
		return err
	}
	if decision.Kind != secret.PendingReplayed && digest != derivedFrom {
		return errors.New("it changed while this swap was preparing, so the refreshed pair was derived from a credential that is no longer there")
	}
	blob, err := refreshed.BlobJSON()
	if err != nil {
		return err
	}
	defer memguard.WipeBytes(blob)
	write, err := secret.WriteCredentials(ctx, &secret.WriteRequest{Paths: paths, NSDir: nsDir, BlobJSON: blob, Prior: &secret.Digests{AccessSHA256: derivedFrom.AccessSHA256, RefreshSHA256: derivedFrom.RefreshSHA256}, NewExpiresAtMS: refreshed.ExpiresAtMillis})
	if err != nil {
		return err
	}
	if write.SavedToPending {
		return fmt.Errorf("the replacement could not be renamed into place and is parked in `%s` (%s)", secret.PendingFile, write.PendingError)
	}
	return nil
}

func useDetectUnlisted(ctx context.Context, status *Status, nsDir string, record *config.AccountRecord) secret.ForeignActivity {
	listing := []secret.ServiceEntry{{Service: secret.ForeignServiceName(record.Kind.Owned.ExportSHA8)}}
	if canonical, err := claude.Canonical(nsDir); err == nil {
		sha8 := claude.SHA8(claude.ExportSpelling(canonical))
		if sha8 != record.Kind.Owned.ExportSHA8 {
			listing = append(listing, secret.ServiceEntry{Service: secret.ForeignServiceName(sha8)})
		}
	}
	return status.detectRefreshActivity(ctx, nsDir, record, listing)
}

func useAdoptedTakesRefresh(dir string, derivedFrom claude.Digests) error {
	if _, err := os.Stat(filepath.Join(dir, secret.PendingFile)); err == nil {
		return errors.New("an unresolved pending write is parked in its namespace")
	}
	read, err := secret.ReadAdopted(dir)
	if err != nil || !read.Present {
		return errors.New("its adopted copy can no longer be read")
	}
	found, err := claude.ParseBlob(read.Bytes)
	memguard.WipeBytes(read.Bytes)
	if err != nil {
		return errors.New("its adopted copy can no longer be read")
	}
	digest, err := found.Digests()
	if err != nil {
		return err
	}
	if digest != derivedFrom {
		return errors.New("its adopted copy changed since this undo read it")
	}
	return nil
}

func useWriteBackAdopted(ctx context.Context, paths *config.Paths, dir string, refreshed *claude.Credentials, derivedFrom claude.Digests) error {
	if err := useAdoptedTakesRefresh(dir, derivedFrom); err != nil {
		return err
	}
	blob, err := useSealedBlob(refreshed)
	if err != nil {
		return err
	}
	_, err = secret.WriteAdopted(ctx, paths, dir, blob)
	return err
}

func useSealedBlob(credentials *claude.Credentials) (*secret.Secret, error) {
	blob, err := credentials.BlobJSON()
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(blob)
	return secret.NewSecret(blob)
}
