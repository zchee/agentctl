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

//go:build !agentctl_testing

package secret

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCredentialsReleaseIgnoresFaultEnvironment(t *testing.T) {
	store := newFileStore(t)
	resume := filepath.Join(t.TempDir(), "resume")
	t.Setenv("AGENTCTL_FAULT", "pause_before_rename,rename_fail")
	t.Setenv("AGENTCTL_FAULT_RESUME", resume)
	replacement := blobJSON("new", "refresh")
	outcome, err := WriteCredentials(t.Context(), &WriteRequest{Paths: store.paths, NSDir: store.nsDir, BlobJSON: []byte(replacement)})
	if err != nil || outcome.SavedToPending {
		t.Fatalf("release write followed a testing fault: %+v, %v", outcome, err)
	}
	if got := readText(t, filepath.Join(store.nsDir, CredentialsFile)); got != replacement {
		t.Fatal("release write did not replace target")
	}
	if _, err := os.Stat(resume + ".reached"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release write reached a testing pause: %v", err)
	}
}
