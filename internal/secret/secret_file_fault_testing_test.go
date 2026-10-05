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

//go:build agentctl_testing

package secret

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteCredentialsExposesBeforeRenamePause(t *testing.T) {
	tests := map[string]struct{ cancel bool }{
		"success: resumed write replaces target":    {},
		"success: cancelled write preserves target": {cancel: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newFileStore(t)
			old, replacement := blobJSON("old", "old-refresh"), blobJSON("new", "new-refresh")
			store.write(t, old, nil)
			resume := filepath.Join(t.TempDir(), "resume")
			t.Setenv("AGENTCTL_FAULT", "pause_before_rename")
			t.Setenv("AGENTCTL_FAULT_RESUME", resume)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := WriteCredentials(ctx, &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(replacement)})
				result <- err
			}()
			t.Cleanup(func() {
				_ = os.WriteFile(resume, nil, 0o600)
				<-done
			})
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			timeout := time.NewTimer(5 * time.Second)
			defer timeout.Stop()
			for {
				if _, err := os.Stat(resume + ".reached"); err == nil {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("writer bypassed pause: %v", err)
				case <-timeout.C:
					t.Fatal("writer never reached before-rename pause")
				case <-ticker.C:
				}
			}
			if got := readText(t, filepath.Join(store.nsDir, CredentialsFile)); got != old {
				t.Fatal("paused write already changed target")
			}
			if strays, err := ListStrayTmp(store.nsDir); err != nil || len(strays) != 1 {
				t.Fatalf("staged files at pause = %v, %v", strays, err)
			}
			if tt.cancel {
				cancel()
			}
			if err := os.WriteFile(resume, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			err := <-result
			want := replacement
			if tt.cancel {
				want = old
				if !isErrorType[*WriteCancelledError](err) {
					t.Fatalf("cancelled write error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := readText(t, filepath.Join(store.nsDir, CredentialsFile)); got != want {
				t.Fatal("resumed write left wrong target")
			}
			if strays, err := ListStrayTmp(store.nsDir); err != nil || len(strays) != 0 {
				t.Fatalf("staged files after resume = %v, %v", strays, err)
			}
		})
	}
}

func TestWriteCredentialsRenameFaultParksPending(t *testing.T) {
	store := newFileStore(t)
	old, replacement := blobJSON("old", "old-refresh"), blobJSON("new", "new-refresh")
	store.write(t, old, nil)
	t.Setenv("AGENTCTL_FAULT", "rename_fail")
	outcome, err := WriteCredentials(t.Context(), &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(replacement)})
	if err != nil || !outcome.SavedToPending || outcome.PendingError == "" {
		t.Fatalf("rename fault did not park pending: %+v, %v", outcome, err)
	}
	if got := readText(t, filepath.Join(store.nsDir, CredentialsFile)); got != old {
		t.Fatal("failed rename changed target")
	}
	if got := readText(t, filepath.Join(store.nsDir, PendingFile)); got != replacement {
		t.Fatal("pending file lost the replacement")
	}
	if _, err := os.Stat(filepath.Join(store.nsDir, PendingMetaFile)); err != nil {
		t.Fatalf("pending metadata missing: %v", err)
	}
}
