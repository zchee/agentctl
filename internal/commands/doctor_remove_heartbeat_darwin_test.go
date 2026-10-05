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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/secret"
	"github.com/zchee/agentctl/internal/testutil"
)

func TestDoctorRemoveStaleRefusesHeartbeatDuringWarning(t *testing.T) {
	tests := map[string]struct {
		name string
	}{
		"error: refresh lock heartbeat": {name: secret.RefreshLockName},
		"error: storage lock heartbeat": {name: secret.StorageWriteLockName},
		"error: legacy lock heartbeat":  {name: "namespace.lock"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testutil.New(t)
			paths := config.NewPaths(fixture.ConfigDir())
			path := filepath.Join(paths.NamespaceDir(testutil.Acct, testutil.Org), tt.name)
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			doctorAge(t, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			out := doctorHeartbeatWriter{path: path}
			command := Doctor{Paths: paths, Out: &out, SampleInterval: time.Millisecond}
			err = command.RemoveStale(t.Context(), path, true)
			if !out.touched {
				t.Fatal("warning output did not trigger the heartbeat")
			}
			if err == nil || !strings.Contains(err.Error(), "was rewritten between the two samples") {
				t.Errorf("heartbeat during warning must refuse removal, got %v; output: %s", err, &out.Buffer)
			}
			after, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("heartbeat lock must remain present: %v", err)
			}
			if !os.SameFile(before, after) || before.ModTime().Equal(after.ModTime()) {
				t.Fatal("expected the original directory with an updated heartbeat")
			}
			if strings.Contains(out.String(), "Removed `") {
				t.Fatalf("refusal announced removal: %s", &out.Buffer)
			}
		})
	}
}

func TestDoctorNamespaceReportObservesHeartbeatDuringWarning(t *testing.T) {
	fixture := testutil.New(t)
	fixture.WriteRegistry([]any{fixture.OwnedRecord(testutil.Acct, testutil.Org)})
	paths := config.NewPaths(fixture.ConfigDir())
	registry, err := config.LoadRegistry(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.NamespaceDir(testutil.Acct, testutil.Org), secret.RefreshLockName)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	doctorAge(t, path)
	out := doctorHeartbeatWriter{path: path}
	command := Doctor{Paths: paths, Out: &out, SampleInterval: time.Millisecond}
	lines, err := command.namespaceSection(t.Context(), registry)
	if err != nil {
		t.Fatal(err)
	}
	if !out.touched {
		t.Fatal("report warning did not trigger the heartbeat")
	}
	report := strings.Join(lines, "\n")
	if !strings.Contains(report, "holder alive (heartbeat seen)") || strings.Contains(report, "--remove-stale") {
		t.Fatalf("heartbeat during warning must report a live holder without offering removal:\n%s", report)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("report changed lock: %v", err)
	}
}

type doctorHeartbeatWriter struct {
	bytes.Buffer
	path    string
	touched bool
}

func (w *doctorHeartbeatWriter) Write(p []byte) (int, error) {
	if !w.touched && (bytes.Contains(p, []byte("Checking for a heartbeat:")) || bytes.Contains(p, []byte("sampling again in"))) {
		now := time.Now()
		if err := os.Chtimes(w.path, now, now); err != nil {
			return 0, err
		}
		w.touched = true
	}
	return w.Buffer.Write(p)
}
