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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/awnumar/memguard"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/secret"
)

const (
	authFile       = "auth.json"
	pendingFile    = "auth.json.pending"
	pendingMeta    = "auth.pending.meta"
	authTempPrefix = "auth.json.tmp."
)

// ShownName returns the credential leaf name for diagnostics only.
func ShownName() string { return authFile }

// ResolvedKind describes a read-only credential read.
type ResolvedKind string

const (
	// ResolvedCredentials means a parsed document exists.
	ResolvedCredentials ResolvedKind = "credentials"
	// ResolvedAbsent means no credential file exists.
	ResolvedAbsent ResolvedKind = "absent"
	// ResolvedTransient means an existing file cannot be used this pass.
	ResolvedTransient ResolvedKind = "transient"
	// ResolvedTorn means an in-place writer has left incomplete JSON.
	ResolvedTorn ResolvedKind = "torn"
)

// Resolved carries a sealed document or a content-free reason.
type Resolved struct {
	Kind        ResolvedKind
	Credentials *Credentials
	Reason      string
}

// ReadAuth is the sole read-only auth-file reader; owned namespaces use a locked namespace handle.
func ReadAuth(ctx context.Context, home string) Resolved {
	path := filepath.Join(home, authFile)
	raw, absent, err := readRegularAuth(ctx, path)
	if absent {
		return Resolved{Kind: ResolvedAbsent}
	}
	if err != nil {
		return Resolved{Kind: ResolvedTransient, Reason: err.Error()}
	}
	defer memguard.WipeBytes(raw)
	credentials, err := ParseCredentials(raw)
	if err != nil {
		if failure, ok := errors.AsType[*CredentialsError](err); ok && failure.Kind == "truncated" {
			return Resolved{Kind: ResolvedTorn}
		}
		return Resolved{Kind: ResolvedTransient, Reason: fmt.Sprintf("`%s`: %s", path, err)}
	}
	return Resolved{Kind: ResolvedCredentials, Credentials: credentials}
}

func readRegularAuth(ctx context.Context, path string) ([]byte, bool, error) {
	file, err := OpenReadonlyNoFollow(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
			return nil, false, fmt.Errorf("`%s` is a symbolic link; agentctl will not read through one", path)
		}
		return nil, false, fmt.Errorf("could not open `%s`: %s", path, ioReason(err))
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("could not stat `%s`: %s", path, ioReason(err))
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("`%s` is not a regular file", path)
	}
	if info.Size() > secret.MaxCredentialsBytes {
		return nil, false, fmt.Errorf("`%s` is %d bytes, larger than the %d-byte limit", path, info.Size(), secret.MaxCredentialsBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(file, secret.MaxCredentialsBytes))
	if err != nil {
		memguard.WipeBytes(raw)
		return nil, false, fmt.Errorf("could not read `%s`: %s", path, ioReason(err))
	}
	return raw, false, nil
}
